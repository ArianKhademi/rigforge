package upload_test

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ArianKhademi/rigforge/api/internal/apitest"
	"github.com/ArianKhademi/rigforge/api/internal/storage"
	"github.com/ArianKhademi/rigforge/api/internal/store"
)

// These tests drive the upload state machine through the real router with an
// in-memory store and bucket. apitest.PartSize is 10 bytes, so a 25-byte
// "video" is a three-part upload (10 + 10 + 5).

const user = "alice"

var ctx = context.Background()

type created struct {
	UploadID  string `json:"uploadId"`
	AssetID   string `json:"assetId"`
	PartSize  int64  `json:"partSize"`
	PartCount int    `json:"partCount"`
}

type described struct {
	Status string `json:"status"`
	Parts  []struct {
		PartNumber int       `json:"partNumber"`
		ETag       string    `json:"etag"`
		RecordedAt time.Time `json:"recordedAt"`
	} `json:"parts"`
}

type completed struct {
	AssetID string `json:"assetId"`
	JobID   string `json:"jobId"`
}

// content is the fake 25-byte video; chunk(n) is the bytes of part n.
const content = "0123456789abcdefghijKLMNO"

func chunk(n int) []byte {
	start := (n - 1) * apitest.PartSize
	return []byte(content[start:min(start+apitest.PartSize, len(content))])
}

func create(t *testing.T, e *apitest.Env) created {
	t.Helper()
	rec := e.Do(user, http.MethodPost, "/api/uploads", map[string]any{
		"filename": "dance.mp4", "size": len(content), "contentType": "video/mp4",
	})
	apitest.WantStatus(t, rec, http.StatusCreated)
	return apitest.Decode[created](t, rec)
}

// putPart stores a part in the fake bucket (the browser's PUT) and returns its ETag.
func putPart(t *testing.T, e *apitest.Env, uploadID string, n int, data []byte) string {
	t.Helper()
	u, err := e.Store.GetUpload(ctx, user, uploadID)
	if err != nil {
		t.Fatal(err)
	}
	return e.Objects.PutPart(u.S3UploadID, n, data)
}

// upload PUTs part n and reports its ETag, as the web client does.
func upload(t *testing.T, e *apitest.Env, uploadID string, n int) {
	t.Helper()
	etag := putPart(t, e, uploadID, n, chunk(n))
	rec := e.Do(user, http.MethodPut, fmt.Sprintf("/api/uploads/%s/parts/%d", uploadID, n), map[string]string{"etag": etag})
	apitest.WantStatus(t, rec, http.StatusOK)
}

func describe(t *testing.T, e *apitest.Env, uploadID string) described {
	t.Helper()
	rec := e.Do(user, http.MethodGet, "/api/uploads/"+uploadID, nil)
	apitest.WantStatus(t, rec, http.StatusOK)
	return apitest.Decode[described](t, rec)
}

func TestCreateReturnsThePartPlan(t *testing.T) {
	e := apitest.New(t)
	up := create(t, e)
	if up.PartSize != apitest.PartSize || up.PartCount != 3 {
		t.Fatalf("plan = %d parts of %d bytes, want 3 of %d", up.PartCount, up.PartSize, apitest.PartSize)
	}
	if e.Objects.OpenUploads() != 1 {
		t.Fatal("create must open a multipart upload in the bucket")
	}
}

func TestCreateValidatesInput(t *testing.T) {
	e := apitest.New(t)
	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"no filename", map[string]any{"filename": "", "size": 10, "contentType": "video/mp4"}, http.StatusBadRequest},
		{"not a video", map[string]any{"filename": "a.pdf", "size": 10, "contentType": "application/pdf"}, http.StatusBadRequest},
		{"zero size", map[string]any{"filename": "a.mp4", "size": 0, "contentType": "video/mp4"}, http.StatusBadRequest},
		{"too large", map[string]any{"filename": "a.mp4", "size": 2 << 20, "contentType": "video/mp4"}, http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apitest.WantStatus(t, e.Do(user, http.MethodPost, "/api/uploads", tc.body), tc.want)
		})
	}
	if e.Objects.OpenUploads() != 0 {
		t.Fatal("a rejected create must not open a multipart upload")
	}
}

func TestPartsRecordedOutOfOrder(t *testing.T) {
	e := apitest.New(t)
	up := create(t, e)

	// Parts finish in whatever order the network delivers them.
	for _, n := range []int{3, 1, 2} {
		upload(t, e, up.UploadID, n)
	}
	// The server lists them by part number regardless of arrival order.
	var got []int
	for _, p := range describe(t, e, up.UploadID).Parts {
		got = append(got, p.PartNumber)
	}
	if !reflect.DeepEqual(got, []int{1, 2, 3}) {
		t.Fatalf("recorded parts = %v, want [1 2 3]", got)
	}

	rec := e.Do(user, http.MethodPost, "/api/uploads/"+up.UploadID+"/complete", map[string]string{"name": "My dance"})
	apitest.WantStatus(t, rec, http.StatusCreated)
	done := apitest.Decode[completed](t, rec)

	// The bucket assembled the object in part-number order.
	u, _ := e.Store.GetUpload(ctx, user, up.UploadID)
	if string(e.Objects.Objects[u.ObjectKey]) != content {
		t.Fatalf("assembled object = %q, want %q", e.Objects.Objects[u.ObjectKey], content)
	}
	// Completing creates the asset and exactly one queued job for it.
	asset, err := e.Store.GetAsset(ctx, user, done.AssetID)
	if err != nil || asset.Name != "My dance" || asset.Status != store.AssetProcessing {
		t.Fatalf("asset = %+v, err %v", asset, err)
	}
	jobs := e.Queue.Jobs()
	if len(jobs) != 1 || jobs[0].ID != done.JobID || jobs[0].AssetID != done.AssetID ||
		jobs[0].Attempt != 1 || jobs[0].Type != store.JobTypeProcessVideo {
		t.Fatalf("enqueued jobs = %+v", jobs)
	}
}

func TestDuplicateETagReportIsIdempotent(t *testing.T) {
	e := apitest.New(t)
	up := create(t, e)
	etag := putPart(t, e, up.UploadID, 1, chunk(1))
	path := "/api/uploads/" + up.UploadID + "/parts/1"

	apitest.WantStatus(t, e.Do(user, http.MethodPut, path, map[string]string{"etag": etag}), http.StatusOK)
	first := describe(t, e, up.UploadID).Parts

	// The same report again (a client retry after a lost response), this time
	// with the quotes S3 puts around ETags: still one part, untouched.
	e.Store.Now = func() time.Time { return time.Now().Add(time.Hour) }
	apitest.WantStatus(t, e.Do(user, http.MethodPut, path, map[string]string{"etag": `"` + etag + `"`}), http.StatusOK)
	second := describe(t, e, up.UploadID).Parts
	if len(second) != 1 || second[0].ETag != etag || !second[0].RecordedAt.Equal(first[0].RecordedAt) {
		t.Fatalf("duplicate report changed the part: before %+v, after %+v", first, second)
	}

	// A different ETag means the part really was uploaded again: last write wins.
	newETag := putPart(t, e, up.UploadID, 1, []byte("XXXXXXXXXX"))
	apitest.WantStatus(t, e.Do(user, http.MethodPut, path, map[string]string{"etag": newETag}), http.StatusOK)
	third := describe(t, e, up.UploadID).Parts
	if len(third) != 1 || third[0].ETag != newETag || !third[0].RecordedAt.After(first[0].RecordedAt) {
		t.Fatalf("re-uploaded part not replaced: %+v", third)
	}
}

func TestCompleteWithMissingPartsIsRejected(t *testing.T) {
	e := apitest.New(t)
	up := create(t, e)
	upload(t, e, up.UploadID, 1)
	upload(t, e, up.UploadID, 3)

	rec := e.Do(user, http.MethodPost, "/api/uploads/"+up.UploadID+"/complete", nil)
	apitest.WantStatus(t, rec, http.StatusConflict)
	body := apitest.Decode[struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Missing []int `json:"missing"`
			} `json:"details"`
		} `json:"error"`
	}](t, rec)
	if body.Error.Code != "missing_parts" || !reflect.DeepEqual(body.Error.Details.Missing, []int{2}) {
		t.Fatalf("error = %+v, want missing_parts [2]", body.Error)
	}

	// The rejection changed nothing: still uploading, bucket upload still open,
	// no asset, nothing enqueued.
	if describe(t, e, up.UploadID).Status != "uploading" || e.Objects.OpenUploads() != 1 || len(e.Queue.Jobs()) != 0 {
		t.Fatal("a rejected complete must leave the upload untouched")
	}

	// Supplying the missing part makes the same call succeed.
	upload(t, e, up.UploadID, 2)
	apitest.WantStatus(t, e.Do(user, http.MethodPost, "/api/uploads/"+up.UploadID+"/complete", nil), http.StatusCreated)
}

func TestCompleteRejectsAnETagTheBucketDoesNotHave(t *testing.T) {
	e := apitest.New(t)
	up := create(t, e)
	upload(t, e, up.UploadID, 1)
	upload(t, e, up.UploadID, 2)
	putPart(t, e, up.UploadID, 3, chunk(3))
	// The client lies about (or corrupts) part 3's ETag.
	apitest.WantStatus(t, e.Do(user, http.MethodPut, "/api/uploads/"+up.UploadID+"/parts/3",
		map[string]string{"etag": "deadbeef"}), http.StatusOK)

	rec := e.Do(user, http.MethodPost, "/api/uploads/"+up.UploadID+"/complete", nil)
	apitest.WantStatus(t, rec, http.StatusConflict)
	if code := apitest.ErrorCode(t, rec); code != "invalid_part" {
		t.Fatalf("code = %q, want invalid_part", code)
	}
}

func TestCompleteIsIdempotent(t *testing.T) {
	e := apitest.New(t)
	up := create(t, e)
	for n := 1; n <= 3; n++ {
		upload(t, e, up.UploadID, n)
	}
	first := e.Do(user, http.MethodPost, "/api/uploads/"+up.UploadID+"/complete", nil)
	apitest.WantStatus(t, first, http.StatusCreated)
	// The client never saw the response and retries.
	second := e.Do(user, http.MethodPost, "/api/uploads/"+up.UploadID+"/complete", nil)
	apitest.WantStatus(t, second, http.StatusOK)

	a, b := apitest.Decode[completed](t, first), apitest.Decode[completed](t, second)
	if a != b {
		t.Fatalf("retry returned %+v, first call returned %+v", b, a)
	}
	if n := len(e.Queue.Jobs()); n != 1 {
		t.Fatalf("%d jobs enqueued, want exactly 1", n)
	}
}

func TestCompleteRecoversWhenBucketCompletedButDatabaseDidNot(t *testing.T) {
	e := apitest.New(t)
	up := create(t, e)
	for n := 1; n <= 3; n++ {
		upload(t, e, up.UploadID, n)
	}
	// Simulate an api crash after CompleteMultipartUpload succeeded but before
	// the transaction committed: the bucket has the object, the row says uploading.
	u, _ := e.Store.GetUpload(ctx, user, up.UploadID)
	stored, _ := e.Objects.ListParts(ctx, u.ObjectKey, u.S3UploadID)
	parts := make([]storage.CompletedPart, len(stored))
	for i, p := range stored {
		parts[i] = storage.CompletedPart{PartNumber: p.PartNumber, ETag: p.ETag}
	}
	if err := e.Objects.CompleteMultipartUpload(ctx, u.ObjectKey, u.S3UploadID, parts); err != nil {
		t.Fatal(err)
	}

	rec := e.Do(user, http.MethodPost, "/api/uploads/"+up.UploadID+"/complete", nil)
	apitest.WantStatus(t, rec, http.StatusCreated)
	if len(e.Queue.Jobs()) != 1 {
		t.Fatal("recovered complete must still enqueue the job")
	}
}

func TestCompleteRejectsSizeMismatch(t *testing.T) {
	e := apitest.New(t)
	up := create(t, e)
	upload(t, e, up.UploadID, 1)
	upload(t, e, up.UploadID, 2)
	// Part 3 arrives truncated: 2 bytes instead of 5.
	etag := putPart(t, e, up.UploadID, 3, []byte("KL"))
	apitest.WantStatus(t, e.Do(user, http.MethodPut, "/api/uploads/"+up.UploadID+"/parts/3",
		map[string]string{"etag": etag}), http.StatusOK)

	rec := e.Do(user, http.MethodPost, "/api/uploads/"+up.UploadID+"/complete", nil)
	apitest.WantStatus(t, rec, http.StatusUnprocessableEntity)
	if code := apitest.ErrorCode(t, rec); code != "size_mismatch" {
		t.Fatalf("code = %q, want size_mismatch", code)
	}
	if len(e.Objects.Objects) != 0 || len(e.Queue.Jobs()) != 0 {
		t.Fatal("a size mismatch must remove the object and create no job")
	}
	if describe(t, e, up.UploadID).Status != "aborted" {
		t.Fatal("upload should be aborted after a size mismatch")
	}
}

func TestRecordPartValidation(t *testing.T) {
	e := apitest.New(t)
	up := create(t, e)
	base := "/api/uploads/" + up.UploadID + "/parts/"
	for _, n := range []string{"0", "4", "-1", "abc"} {
		apitest.WantStatus(t, e.Do(user, http.MethodPut, base+n, map[string]string{"etag": "x"}), http.StatusBadRequest)
	}
	apitest.WantStatus(t, e.Do(user, http.MethodPut, base+"1", map[string]string{"etag": "  "}), http.StatusBadRequest)
	apitest.WantStatus(t, e.Do(user, http.MethodPut, base+"1", nil), http.StatusBadRequest)
}

func TestPresignParts(t *testing.T) {
	e := apitest.New(t)
	up := create(t, e)
	path := "/api/uploads/" + up.UploadID + "/parts"

	rec := e.Do(user, http.MethodPost, path, map[string]any{"partNumbers": []int{2, 3}})
	apitest.WantStatus(t, rec, http.StatusOK)
	body := apitest.Decode[struct {
		URLs []struct {
			PartNumber int    `json:"partNumber"`
			URL        string `json:"url"`
		} `json:"urls"`
	}](t, rec)
	if len(body.URLs) != 2 || body.URLs[0].PartNumber != 2 || !strings.Contains(body.URLs[1].URL, "partNumber=3") {
		t.Fatalf("urls = %+v", body.URLs)
	}

	apitest.WantStatus(t, e.Do(user, http.MethodPost, path, map[string]any{"partNumbers": []int{4}}), http.StatusBadRequest)
	apitest.WantStatus(t, e.Do(user, http.MethodPost, path, map[string]any{"partNumbers": []int{}}), http.StatusBadRequest)
}

func TestUploadsAreScopedToTheirOwner(t *testing.T) {
	e := apitest.New(t)
	up := create(t, e)
	base := "/api/uploads/" + up.UploadID
	// Another user gets 404 (not 403) everywhere: ids are not even confirmed to exist.
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, base},
		{http.MethodPost, base + "/parts"},
		{http.MethodPut, base + "/parts/1"},
		{http.MethodPost, base + "/reconcile"},
		{http.MethodPost, base + "/complete"},
		{http.MethodDelete, base},
	} {
		apitest.WantStatus(t, e.Do("mallory", r.method, r.path, map[string]any{"etag": "x", "partNumbers": []int{1}}), http.StatusNotFound)
	}
	apitest.WantStatus(t, e.Do(user, http.MethodGet, "/api/uploads/not-a-uuid", nil), http.StatusNotFound)
}

func TestAbort(t *testing.T) {
	e := apitest.New(t)
	up := create(t, e)
	upload(t, e, up.UploadID, 1)
	base := "/api/uploads/" + up.UploadID

	apitest.WantStatus(t, e.Do(user, http.MethodDelete, base, nil), http.StatusNoContent)
	if e.Objects.OpenUploads() != 0 {
		t.Fatal("abort must release the multipart upload in the bucket")
	}
	// An aborted upload accepts nothing further, and aborting again is a no-op.
	apitest.WantStatus(t, e.Do(user, http.MethodPut, base+"/parts/2", map[string]string{"etag": "x"}), http.StatusConflict)
	apitest.WantStatus(t, e.Do(user, http.MethodPost, base+"/parts", map[string]any{"partNumbers": []int{2}}), http.StatusConflict)
	apitest.WantStatus(t, e.Do(user, http.MethodPost, base+"/complete", nil), http.StatusConflict)
	apitest.WantStatus(t, e.Do(user, http.MethodDelete, base, nil), http.StatusNoContent)
}

func TestCompletedUploadCannotBeAborted(t *testing.T) {
	e := apitest.New(t)
	up := create(t, e)
	for n := 1; n <= 3; n++ {
		upload(t, e, up.UploadID, n)
	}
	apitest.WantStatus(t, e.Do(user, http.MethodPost, "/api/uploads/"+up.UploadID+"/complete", nil), http.StatusCreated)
	apitest.WantStatus(t, e.Do(user, http.MethodDelete, "/api/uploads/"+up.UploadID, nil), http.StatusConflict)
}

func TestReconcileAdoptsPartsTheClientNeverReported(t *testing.T) {
	e := apitest.New(t)
	up := create(t, e)
	upload(t, e, up.UploadID, 1)
	// Part 2 reached the bucket, then the client died before reporting it.
	etag2 := putPart(t, e, up.UploadID, 2, chunk(2))
	// Part 3 is in the bucket with the wrong size; it must not be adopted.
	putPart(t, e, up.UploadID, 3, []byte("K"))

	rec := e.Do(user, http.MethodPost, "/api/uploads/"+up.UploadID+"/reconcile", nil)
	apitest.WantStatus(t, rec, http.StatusOK)
	parts := apitest.Decode[described](t, rec).Parts
	if len(parts) != 2 || parts[1].PartNumber != 2 || parts[1].ETag != etag2 {
		t.Fatalf("after reconcile parts = %+v, want parts 1 and 2", parts)
	}
}

func TestStorageViewListsBucketParts(t *testing.T) {
	e := apitest.New(t)
	up := create(t, e)
	upload(t, e, up.UploadID, 2)
	rec := e.Do(user, http.MethodGet, "/api/uploads/"+up.UploadID+"?storage=true", nil)
	apitest.WantStatus(t, rec, http.StatusOK)
	body := apitest.Decode[struct {
		StorageParts []struct {
			PartNumber   int       `json:"partNumber"`
			Size         int64     `json:"size"`
			LastModified time.Time `json:"lastModified"`
		} `json:"storageParts"`
	}](t, rec)
	if len(body.StorageParts) != 1 || body.StorageParts[0].PartNumber != 2 ||
		body.StorageParts[0].Size != apitest.PartSize || body.StorageParts[0].LastModified.IsZero() {
		t.Fatalf("storageParts = %+v", body.StorageParts)
	}
}

func TestReaperAbortsOnlyIdleUploads(t *testing.T) {
	e := apitest.New(t)
	now := time.Now()

	// One upload last touched 25 hours ago, one touched an hour ago.
	e.Store.Now = func() time.Time { return now.Add(-25 * time.Hour) }
	idle := create(t, e)
	e.Store.Now = func() time.Time { return now.Add(-time.Hour) }
	active := create(t, e)
	e.Store.Now = time.Now

	aborted, err := e.Server.Uploads.ReapOnce(ctx, now)
	if err != nil || aborted != 1 {
		t.Fatalf("ReapOnce = %d, %v; want 1 aborted", aborted, err)
	}
	if describe(t, e, idle.UploadID).Status != "aborted" || describe(t, e, active.UploadID).Status != "uploading" {
		t.Fatal("reaper must abort the idle upload and leave the active one")
	}
	if e.Objects.OpenUploads() != 1 {
		t.Fatalf("%d multipart uploads left in the bucket, want 1", e.Objects.OpenUploads())
	}
	// Recording a part resets the idle clock, so an upload in progress is safe.
	if again, _ := e.Server.Uploads.ReapOnce(ctx, now); again != 0 {
		t.Fatal("second pass should find nothing to abort")
	}
}

func TestJobSurvivesAQueueOutageAtCompleteTime(t *testing.T) {
	e := apitest.New(t)
	up := create(t, e)
	for n := 1; n <= 3; n++ {
		upload(t, e, up.UploadID, n)
	}
	// Redis is down at the moment the upload completes.
	e.Queue.Fail = true
	rec := e.Do(user, http.MethodPost, "/api/uploads/"+up.UploadID+"/complete", nil)
	apitest.WantStatus(t, rec, http.StatusCreated) // the upload itself still succeeds
	done := apitest.Decode[completed](t, rec)

	j, _ := e.Store.GetJob(ctx, user, done.JobID)
	if j.EnqueuedAt != nil || len(e.Queue.Jobs()) != 0 {
		t.Fatal("job should be committed but not yet enqueued")
	}

	// Redis comes back; the dispatcher picks the stranded job up.
	e.Queue.Fail = false
	sent, err := e.Server.Jobs.DispatchOnce(ctx, time.Now().Add(time.Second))
	if err != nil || sent != 1 {
		t.Fatalf("DispatchOnce = %d, %v; want 1", sent, err)
	}
	j, _ = e.Store.GetJob(ctx, user, done.JobID)
	if j.EnqueuedAt == nil || len(e.Queue.Jobs()) != 1 {
		t.Fatal("dispatcher should have enqueued and marked the job")
	}
	// And it does not enqueue it a second time.
	if sent, _ := e.Server.Jobs.DispatchOnce(ctx, time.Now().Add(time.Second)); sent != 0 {
		t.Fatal("dispatcher enqueued an already-enqueued job")
	}
}

func TestCompleteWithCharacterSelection(t *testing.T) {
	e := apitest.New(t)

	finish := func(characterID string) (int, string) {
		up := create(t, e)
		for n := 1; n <= 3; n++ {
			upload(t, e, up.UploadID, n)
		}
		rec := e.Do(user, http.MethodPost, "/api/uploads/"+up.UploadID+"/complete", map[string]string{"characterId": characterID})
		if rec.Code != http.StatusCreated {
			return rec.Code, ""
		}
		j, _ := e.Store.GetJob(ctx, user, apitest.Decode[completed](t, rec).JobID)
		return rec.Code, j.CharacterID
	}

	if code, got := finish(""); code != http.StatusCreated || got != "default" {
		t.Fatalf("no character: %d %q, want 201 default", code, got)
	}
	if code, got := finish("none"); code != http.StatusCreated || got != "none" {
		t.Fatalf("skeleton only: %d %q", code, got)
	}
	// A character id that does not exist (or belongs to someone else) is refused.
	if code, _ := finish("7f3c1c1e-58a1-4b8e-9a53-0d6f3b0f2a11"); code != http.StatusBadRequest {
		t.Fatalf("unknown character: %d, want 400", code)
	}
	mine := &store.Character{ID: "0b7d1d0c-1c66-4c50-a2d5-6f6a8f0e9d01", UserID: user, Name: "Robot", ObjectKey: "characters/x.glb"}
	_ = e.Store.CreateCharacter(ctx, mine)
	if code, got := finish(mine.ID); code != http.StatusCreated || got != mine.ID {
		t.Fatalf("own character: %d %q", code, got)
	}
}
