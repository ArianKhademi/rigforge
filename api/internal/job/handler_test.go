package job_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ArianKhademi/rigforge/api/internal/apitest"
	"github.com/ArianKhademi/rigforge/api/internal/store"
)

const user = "alice"

var ctx = context.Background()

// seed creates a completed upload with its asset and queued job directly in
// the store and returns the job id.
func seed(t *testing.T, e *apitest.Env) string {
	t.Helper()
	u := &store.Upload{
		ID: uuid.NewString(), UserID: user, AssetID: uuid.NewString(), Filename: "a.mp4",
		ContentType: "video/mp4", Size: 1, PartSize: 10, PartCount: 1, Status: store.UploadUploading,
	}
	if err := e.Store.CreateUpload(ctx, u); err != nil {
		t.Fatal(err)
	}
	asset := &store.Asset{ID: u.AssetID, UserID: user, Name: "a", Status: store.AssetProcessing}
	j := e.Server.Jobs.New(asset.ID, user, "default")
	if err := e.Store.FinishUpload(ctx, u.ID, asset, j); err != nil {
		t.Fatal(err)
	}
	return j.ID
}

type jobBody struct {
	ID          string  `json:"id"`
	Status      string  `json:"status"`
	Progress    int     `json:"progress"`
	Attempt     int     `json:"attempt"`
	MaxAttempts int     `json:"maxAttempts"`
	Error       *string `json:"error"`
}

func TestGetJob(t *testing.T) {
	e := apitest.New(t)
	id := seed(t, e)

	rec := e.Do(user, http.MethodGet, "/api/jobs/"+id, nil)
	apitest.WantStatus(t, rec, http.StatusOK)
	got := apitest.Decode[jobBody](t, rec)
	if got.ID != id || got.Status != "queued" || got.Attempt != 1 || got.MaxAttempts != 3 {
		t.Fatalf("job = %+v", got)
	}

	apitest.WantStatus(t, e.Do("mallory", http.MethodGet, "/api/jobs/"+id, nil), http.StatusNotFound)
	apitest.WantStatus(t, e.Do("mallory", http.MethodGet, "/api/jobs/"+id+"/events", nil), http.StatusNotFound)
	apitest.WantStatus(t, e.Do(user, http.MethodGet, "/api/jobs/nope", nil), http.StatusNotFound)
}

// TestEventStream follows a job from queued to done over a real HTTP
// connection and checks that the stream reports each change and then ends.
func TestEventStream(t *testing.T) {
	e := apitest.New(t)
	id := seed(t, e)

	srv := httptest.NewServer(e.Server.Router)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/jobs/"+id+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+e.Token(user))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}

	// next reads SSE lines until the next "data:" line and decodes it. It
	// returns false when the server closes the stream.
	lines := make(chan string)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	next := func() (jobBody, bool) {
		t.Helper()
		timeout := time.After(5 * time.Second)
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					return jobBody{}, false
				}
				if data, found := strings.CutPrefix(line, "data: "); found {
					var j jobBody
					if err := json.Unmarshal([]byte(data), &j); err != nil {
						t.Fatalf("bad event payload %q: %v", data, err)
					}
					return j, true
				}
			case <-timeout:
				t.Fatal("timed out waiting for an event")
			}
		}
	}

	// 1. The current state arrives immediately on connect.
	if j, ok := next(); !ok || j.Status != "queued" {
		t.Fatalf("first event = %+v, %v; want queued", j, ok)
	}

	// 2. The worker updates the row and publishes; the stream forwards it.
	e.Store.SetJobState(id, store.JobExtracting, 40, nil)
	e.Events.Publish(id)
	if j, ok := next(); !ok || j.Status != "extracting" || j.Progress != 40 {
		t.Fatalf("second event = %+v, %v; want extracting 40%%", j, ok)
	}

	// 3. A publish without a change to the row sends nothing new; the next
	//    event the client sees is the real change that follows.
	e.Events.Publish(id)
	e.Store.SetJobState(id, store.JobDone, 100, nil)
	e.Events.Publish(id)
	if j, ok := next(); !ok || j.Status != "done" || j.Progress != 100 {
		t.Fatalf("third event = %+v, %v; want done 100%%", j, ok)
	}

	// 4. done is terminal: the server ends the stream.
	if j, ok := next(); ok {
		t.Fatalf("stream still open after done, got %+v", j)
	}
}

func TestEventStreamEndsImmediatelyForFinishedJob(t *testing.T) {
	e := apitest.New(t)
	id := seed(t, e)
	msg := "ffmpeg exited with status 1"
	e.Store.SetJobState(id, store.JobFailed, 10, &msg)

	rec := e.Do(user, http.MethodGet, "/api/jobs/"+id+"/events", nil)
	apitest.WantStatus(t, rec, http.StatusOK)
	body := rec.Body.String()
	if strings.Count(body, "event: job") != 1 || !strings.Contains(body, `"status":"failed"`) ||
		!strings.Contains(body, msg) {
		t.Fatalf("expected exactly one failed event carrying the error, got:\n%s", body)
	}
}
