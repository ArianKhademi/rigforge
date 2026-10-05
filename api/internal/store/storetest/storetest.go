// Package storetest is one behavioural test suite run against every Store
// implementation. The handler tests use store.Memory; this suite is what makes
// that trustworthy, because the same assertions also pass against Postgres.
package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ArianKhademi/rigforge/api/internal/store"
)

var ctx = context.Background()

// Run executes the suite. newStore must return an empty store.
func Run(t *testing.T, newStore func(t *testing.T) store.Store) {
	tests := map[string]func(*testing.T, store.Store){
		"upload round trip and ownership":       testUploadOwnership,
		"record part is an idempotent upsert":   testRecordPart,
		"finish upload is atomic and one-shot":  testFinishUpload,
		"abort stops the upload":                testAbort,
		"stale uploads":                         testStaleUploads,
		"assets list with search and sort":      testListAssets,
		"delete asset cascades":                 testDeleteAsset,
		"jobs: latest, enqueue marking, sweeps": testJobs,
		"characters are scoped to their owner":  testCharacters,
	}
	for name, fn := range tests {
		t.Run(name, func(t *testing.T) { fn(t, newStore(t)) })
	}
}

func newUpload(user string) *store.Upload {
	id := uuid.NewString()
	return &store.Upload{
		ID: id, UserID: user, AssetID: uuid.NewString(), Filename: "clip.mp4", ContentType: "video/mp4",
		Size: 25, PartSize: 10, PartCount: 3, ObjectKey: "assets/" + id + "/source.mp4",
		S3UploadID: "mpu-" + id, Status: store.UploadUploading,
	}
}

func newAssetAndJob(u *store.Upload, name string) (*store.Asset, *store.Job) {
	asset := &store.Asset{
		ID: u.AssetID, UserID: u.UserID, Name: name, SourceKey: u.ObjectKey,
		SourceSize: u.Size, ContentType: u.ContentType, Status: store.AssetProcessing,
	}
	job := &store.Job{
		ID: uuid.NewString(), AssetID: asset.ID, UserID: u.UserID, Type: store.JobTypeProcessVideo,
		CharacterID: "default", Status: store.JobQueued, Attempt: 1, MaxAttempts: 3,
	}
	return asset, job
}

// finished creates an upload and completes it, returning the asset and job.
func finished(t *testing.T, s store.Store, user, name string) (*store.Asset, *store.Job) {
	t.Helper()
	u := newUpload(user)
	must(t, s.CreateUpload(ctx, u))
	asset, job := newAssetAndJob(u, name)
	must(t, s.FinishUpload(ctx, u.ID, asset, job))
	return asset, job
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func testUploadOwnership(t *testing.T, s store.Store) {
	u := newUpload("alice")
	must(t, s.CreateUpload(ctx, u))
	if u.CreatedAt.IsZero() {
		t.Fatal("CreateUpload must set CreatedAt")
	}
	got, err := s.GetUpload(ctx, "alice", u.ID)
	must(t, err)
	if got.S3UploadID != u.S3UploadID || got.PartCount != 3 || got.Status != store.UploadUploading {
		t.Fatalf("round trip lost data: %+v", got)
	}
	_, err = s.GetUpload(ctx, "bob", u.ID)
	wantErr(t, err, store.ErrNotFound)
	_, err = s.GetUpload(ctx, "alice", uuid.NewString())
	wantErr(t, err, store.ErrNotFound)
}

func testRecordPart(t *testing.T, s store.Store) {
	u := newUpload("alice")
	must(t, s.CreateUpload(ctx, u))

	// Out of order on purpose.
	for _, n := range []int{3, 1} {
		_, err := s.RecordPart(ctx, u.ID, n, "etag-"+string(rune('0'+n)))
		must(t, err)
	}
	first, err := s.RecordPart(ctx, u.ID, 2, "etag-2")
	must(t, err)

	parts, err := s.ListParts(ctx, u.ID)
	must(t, err)
	if len(parts) != 3 || parts[0].PartNumber != 1 || parts[1].PartNumber != 2 || parts[2].PartNumber != 3 {
		t.Fatalf("parts not sorted by number: %+v", parts)
	}

	// Same ETag again: no new row, original timestamp kept.
	time.Sleep(5 * time.Millisecond)
	again, err := s.RecordPart(ctx, u.ID, 2, "etag-2")
	must(t, err)
	if !again.RecordedAt.Equal(first.RecordedAt) {
		t.Fatalf("duplicate report moved RecordedAt from %v to %v", first.RecordedAt, again.RecordedAt)
	}
	// New ETag: replaced, timestamp moves.
	replaced, err := s.RecordPart(ctx, u.ID, 2, "etag-2b")
	must(t, err)
	if replaced.ETag != "etag-2b" || !replaced.RecordedAt.After(first.RecordedAt) {
		t.Fatalf("re-upload not recorded: %+v", replaced)
	}
	parts, _ = s.ListParts(ctx, u.ID)
	if len(parts) != 3 {
		t.Fatalf("%d parts after re-reporting, want 3", len(parts))
	}

	// Recording a part bumps the upload's idle clock.
	got, _ := s.GetUpload(ctx, "alice", u.ID)
	if !got.UpdatedAt.After(u.UpdatedAt) {
		t.Fatal("RecordPart must bump the upload's UpdatedAt")
	}

	// Unknown upload: conflict, not a silent insert.
	_, err = s.RecordPart(ctx, uuid.NewString(), 1, "x")
	wantErr(t, err, store.ErrConflict)
}

func testFinishUpload(t *testing.T, s store.Store) {
	u := newUpload("alice")
	must(t, s.CreateUpload(ctx, u))
	asset, job := newAssetAndJob(u, "Dance")
	must(t, s.FinishUpload(ctx, u.ID, asset, job))

	got, _ := s.GetUpload(ctx, "alice", u.ID)
	if got.Status != store.UploadCompleted {
		t.Fatalf("upload status = %s, want completed", got.Status)
	}
	a, err := s.GetAsset(ctx, "alice", asset.ID)
	must(t, err)
	if a.Name != "Dance" || a.Job == nil || a.Job.ID != job.ID || a.Job.Status != store.JobQueued {
		t.Fatalf("asset or job missing after finish: %+v", a)
	}

	// Finishing twice is refused and must not create a second asset or job.
	asset2, job2 := newAssetAndJob(u, "Dance again")
	job2.AssetID = asset.ID
	wantErr(t, s.FinishUpload(ctx, u.ID, asset2, job2), store.ErrConflict)
	_, err = s.GetJob(ctx, "alice", job2.ID)
	wantErr(t, err, store.ErrNotFound)

	// Parts can no longer be recorded.
	_, err = s.RecordPart(ctx, u.ID, 1, "x")
	wantErr(t, err, store.ErrConflict)
}

func testAbort(t *testing.T, s store.Store) {
	u := newUpload("alice")
	must(t, s.CreateUpload(ctx, u))
	must(t, s.AbortUpload(ctx, u.ID))
	got, _ := s.GetUpload(ctx, "alice", u.ID)
	if got.Status != store.UploadAborted {
		t.Fatalf("status = %s, want aborted", got.Status)
	}
	wantErr(t, s.AbortUpload(ctx, u.ID), store.ErrConflict)
	_, err := s.RecordPart(ctx, u.ID, 1, "x")
	wantErr(t, err, store.ErrConflict)
	asset, job := newAssetAndJob(u, "x")
	wantErr(t, s.FinishUpload(ctx, u.ID, asset, job), store.ErrConflict)
}

func testStaleUploads(t *testing.T, s store.Store) {
	inFlight := newUpload("alice")
	must(t, s.CreateUpload(ctx, inFlight))
	aborted := newUpload("alice")
	must(t, s.CreateUpload(ctx, aborted))
	must(t, s.AbortUpload(ctx, aborted.ID))

	// A cutoff in the past matches nothing; one in the future matches only the
	// upload that is still in flight.
	none, err := s.StaleUploads(ctx, time.Now().Add(-time.Hour), 10)
	must(t, err)
	if len(none) != 0 {
		t.Fatalf("%d stale uploads with a past cutoff, want 0", len(none))
	}
	stale, err := s.StaleUploads(ctx, time.Now().Add(time.Hour), 10)
	must(t, err)
	if len(stale) != 1 || stale[0].ID != inFlight.ID {
		t.Fatalf("stale = %+v, want only the in-flight upload", stale)
	}
}

func testListAssets(t *testing.T, s store.Store) {
	finished(t, s, "alice", "Walk cycle")
	time.Sleep(5 * time.Millisecond)
	finished(t, s, "alice", "backflip")
	time.Sleep(5 * time.Millisecond)
	finished(t, s, "alice", "100% effort_run")
	finished(t, s, "bob", "Bob's walk")

	names := func(f store.AssetFilter) []string {
		assets, err := s.ListAssets(ctx, "alice", f)
		must(t, err)
		var out []string
		for _, a := range assets {
			if a.Job == nil {
				t.Fatalf("asset %q listed without its job", a.Name)
			}
			out = append(out, a.Name)
		}
		return out
	}
	equal := func(got []string, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	}

	equal(names(store.AssetFilter{Sort: store.SortNewest}), "100% effort_run", "backflip", "Walk cycle")
	equal(names(store.AssetFilter{Sort: store.SortOldest}), "Walk cycle", "backflip", "100% effort_run")
	equal(names(store.AssetFilter{Sort: store.SortName}), "100% effort_run", "backflip", "Walk cycle")
	// Search is a case-insensitive substring match...
	equal(names(store.AssetFilter{Query: "WALK"}), "Walk cycle")
	// ...and treats % and _ literally rather than as wildcards.
	equal(names(store.AssetFilter{Query: "%"}), "100% effort_run")
	equal(names(store.AssetFilter{Query: "t_r"}), "100% effort_run")
	equal(names(store.AssetFilter{Query: "nothing"}))
}

func testDeleteAsset(t *testing.T, s store.Store) {
	asset, job := finished(t, s, "alice", "Dance")
	wantErr(t, s.DeleteAsset(ctx, "bob", asset.ID), store.ErrNotFound)
	must(t, s.DeleteAsset(ctx, "alice", asset.ID))

	_, err := s.GetAsset(ctx, "alice", asset.ID)
	wantErr(t, err, store.ErrNotFound)
	_, err = s.GetJob(ctx, "alice", job.ID)
	wantErr(t, err, store.ErrNotFound)
	wantErr(t, s.DeleteAsset(ctx, "alice", asset.ID), store.ErrNotFound)
}

func testJobs(t *testing.T, s store.Store) {
	asset, first := finished(t, s, "alice", "Dance")

	_, err := s.GetJob(ctx, "bob", first.ID)
	wantErr(t, err, store.ErrNotFound)

	// A job that was never marked enqueued shows up in the sweep, once its
	// creation time is before the cutoff.
	pending, err := s.UnenqueuedJobs(ctx, time.Now().Add(-time.Hour), 10)
	must(t, err)
	if len(pending) != 0 {
		t.Fatal("a job newer than the cutoff must not be swept")
	}
	pending, _ = s.UnenqueuedJobs(ctx, time.Now().Add(time.Hour), 10)
	if len(pending) != 1 || pending[0].ID != first.ID {
		t.Fatalf("pending = %+v", pending)
	}
	must(t, s.MarkJobEnqueued(ctx, first.ID))
	pending, _ = s.UnenqueuedJobs(ctx, time.Now().Add(time.Hour), 10)
	if len(pending) != 0 {
		t.Fatal("an enqueued job must not be swept again")
	}
	got, _ := s.GetJob(ctx, "alice", first.ID)
	if got.EnqueuedAt == nil {
		t.Fatal("MarkJobEnqueued did not set EnqueuedAt")
	}

	// Reprocess: a second job becomes the asset's latest job.
	time.Sleep(5 * time.Millisecond)
	second := &store.Job{
		ID: uuid.NewString(), AssetID: asset.ID, UserID: "alice", Type: store.JobTypeProcessVideo,
		CharacterID: "none", Status: store.JobQueued, Attempt: 1, MaxAttempts: 3,
	}
	must(t, s.CreateJob(ctx, second))
	latest, err := s.LatestJobForAsset(ctx, asset.ID)
	must(t, err)
	if latest.ID != second.ID || latest.CharacterID != "none" {
		t.Fatalf("latest job = %+v, want the second job", latest)
	}

	// A job cannot be attached to someone else's asset.
	foreign := *second
	foreign.ID, foreign.UserID = uuid.NewString(), "bob"
	wantErr(t, s.CreateJob(ctx, &foreign), store.ErrNotFound)
}

func testCharacters(t *testing.T, s store.Store) {
	c := &store.Character{ID: uuid.NewString(), UserID: "alice", Name: "Robot", ObjectKey: "characters/r.glb", Size: 42}
	must(t, s.CreateCharacter(ctx, c))

	mine, err := s.ListCharacters(ctx, "alice")
	must(t, err)
	if len(mine) != 1 || mine[0].Name != "Robot" || mine[0].Size != 42 {
		t.Fatalf("characters = %+v", mine)
	}
	theirs, _ := s.ListCharacters(ctx, "bob")
	if len(theirs) != 0 {
		t.Fatal("bob can see alice's character")
	}
	_, err = s.GetCharacter(ctx, "bob", c.ID)
	wantErr(t, err, store.ErrNotFound)
	wantErr(t, s.DeleteCharacter(ctx, "bob", c.ID), store.ErrNotFound)
	must(t, s.DeleteCharacter(ctx, "alice", c.ID))
	_, err = s.GetCharacter(ctx, "alice", c.ID)
	wantErr(t, err, store.ErrNotFound)
}
