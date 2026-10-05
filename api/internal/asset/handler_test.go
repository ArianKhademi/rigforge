package asset_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ArianKhademi/rigforge/api/internal/apitest"
	"github.com/ArianKhademi/rigforge/api/internal/store"
)

const user = "alice"

var ctx = context.Background()

// seed creates an asset with a queued job for owner and returns both ids.
func seed(t *testing.T, e *apitest.Env, owner, name string) (assetID, jobID string) {
	t.Helper()
	u := &store.Upload{
		ID: uuid.NewString(), UserID: owner, AssetID: uuid.NewString(), Filename: name + ".mp4",
		ContentType: "video/mp4", Size: 1, PartSize: 10, PartCount: 1, Status: store.UploadUploading,
	}
	if err := e.Store.CreateUpload(ctx, u); err != nil {
		t.Fatal(err)
	}
	asset := &store.Asset{ID: u.AssetID, UserID: owner, Name: name, Status: store.AssetProcessing,
		SourceKey: "assets/" + u.AssetID + "/source.mp4"}
	j := e.Server.Jobs.New(asset.ID, owner, "default")
	if err := e.Store.FinishUpload(ctx, u.ID, asset, j); err != nil {
		t.Fatal(err)
	}
	return asset.ID, j.ID
}

type assetBody struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Job    *struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		CharacterID string `json:"characterId"`
	} `json:"job"`
	URLs struct {
		Poster  string `json:"poster"`
		Preview string `json:"preview"`
		Motion  string `json:"motion"`
	} `json:"urls"`
}

type listBody struct {
	Assets []assetBody `json:"assets"`
}

func TestListIsScopedSearchableAndSortable(t *testing.T) {
	e := apitest.New(t)
	seed(t, e, user, "Walk cycle")
	seed(t, e, user, "Backflip")
	seed(t, e, "bob", "Bob's secret dance")

	rec := e.Do(user, http.MethodGet, "/api/assets?sort=name", nil)
	apitest.WantStatus(t, rec, http.StatusOK)
	all := apitest.Decode[listBody](t, rec).Assets
	if len(all) != 2 || all[0].Name != "Backflip" || all[1].Name != "Walk cycle" {
		t.Fatalf("list = %+v, want alice's two assets sorted by name", all)
	}
	if all[0].Job == nil || all[0].Job.Status != "queued" {
		t.Fatal("each listed asset carries its latest job")
	}

	rec = e.Do(user, http.MethodGet, "/api/assets?q=walk", nil)
	if found := apitest.Decode[listBody](t, rec).Assets; len(found) != 1 || found[0].Name != "Walk cycle" {
		t.Fatalf("search = %+v", found)
	}
	apitest.WantStatus(t, e.Do(user, http.MethodGet, "/api/assets?sort=size", nil), http.StatusBadRequest)
}

func TestURLsAppearOnlyWhenReady(t *testing.T) {
	e := apitest.New(t)
	id, _ := seed(t, e, user, "Dance")

	processing := apitest.Decode[assetBody](t, e.Do(user, http.MethodGet, "/api/assets/"+id, nil))
	if processing.URLs.Motion != "" || processing.URLs.Poster != "" {
		t.Fatalf("processing asset exposes output URLs: %+v", processing.URLs)
	}

	e.Store.SetAssetReady(id, 5.0)
	ready := apitest.Decode[assetBody](t, e.Do(user, http.MethodGet, "/api/assets/"+id, nil))
	for name, url := range map[string]string{
		"poster.jpg": ready.URLs.Poster, "preview.mp4": ready.URLs.Preview, "motion.glb": ready.URLs.Motion,
	} {
		if !strings.HasSuffix(url, "assets/"+id+"/"+name) {
			t.Errorf("url for %s = %q", name, url)
		}
	}
	// The grid only needs the poster, so the list does not sign the rest.
	listed := apitest.Decode[listBody](t, e.Do(user, http.MethodGet, "/api/assets", nil)).Assets[0]
	if listed.URLs.Poster == "" || listed.URLs.Motion != "" {
		t.Fatalf("list urls = %+v, want poster only", listed.URLs)
	}

	apitest.WantStatus(t, e.Do("bob", http.MethodGet, "/api/assets/"+id, nil), http.StatusNotFound)
}

func TestExport(t *testing.T) {
	e := apitest.New(t)
	id, _ := seed(t, e, user, "Dance")
	path := "/api/assets/" + id + "/export"

	// Nothing to export until processing has finished.
	apitest.WantStatus(t, e.Do(user, http.MethodGet, path+"?format=glb", nil), http.StatusConflict)

	e.Store.SetAssetReady(id, 5.0)
	for format, file := range map[string]string{"glb": "motion.glb", "bvh": "motion.bvh"} {
		rec := e.Do(user, http.MethodGet, path+"?format="+format, nil)
		apitest.WantStatus(t, rec, http.StatusOK)
		body := apitest.Decode[struct{ URL, Filename string }](t, rec)
		if !strings.Contains(body.URL, "assets/"+id+"/"+file) || body.Filename != "Dance."+format {
			t.Errorf("%s export = %+v", format, body)
		}
	}
	apitest.WantStatus(t, e.Do(user, http.MethodGet, path+"?format=fbx", nil), http.StatusBadRequest)
	apitest.WantStatus(t, e.Do("bob", http.MethodGet, path+"?format=glb", nil), http.StatusNotFound)
}

func TestReprocess(t *testing.T) {
	e := apitest.New(t)
	id, firstJob := seed(t, e, user, "Dance")
	path := "/api/assets/" + id + "/jobs"

	// While a job is still in flight a second one is refused.
	apitest.WantStatus(t, e.Do(user, http.MethodPost, path, nil), http.StatusConflict)

	msg := "no person detected"
	e.Store.SetJobState(firstJob, store.JobFailed, 0, &msg)
	rec := e.Do(user, http.MethodPost, path, map[string]string{"characterId": "none"})
	apitest.WantStatus(t, rec, http.StatusCreated)

	got := apitest.Decode[assetBody](t, e.Do(user, http.MethodGet, "/api/assets/"+id, nil))
	if got.Status != "processing" || got.Job.ID == firstJob || got.Job.CharacterID != "none" {
		t.Fatalf("after reprocess: %+v job %+v", got, got.Job)
	}
	// Same asset, new job: the queue got one message for it.
	if jobs := e.Queue.Jobs(); len(jobs) != 1 || jobs[0].AssetID != id || jobs[0].ID != got.Job.ID {
		t.Fatalf("queue = %+v", jobs)
	}
}

func TestDeleteRemovesRowsAndObjects(t *testing.T) {
	e := apitest.New(t)
	id, jobID := seed(t, e, user, "Dance")
	other, _ := seed(t, e, user, "Keep me")
	for _, key := range []string{"assets/" + id + "/source.mp4", "assets/" + id + "/motion.glb", "assets/" + other + "/motion.glb"} {
		_ = e.Objects.PutObject(ctx, key, "", strings.NewReader("x"), 1)
	}

	apitest.WantStatus(t, e.Do("bob", http.MethodDelete, "/api/assets/"+id, nil), http.StatusNotFound)
	apitest.WantStatus(t, e.Do(user, http.MethodDelete, "/api/assets/"+id, nil), http.StatusNoContent)

	apitest.WantStatus(t, e.Do(user, http.MethodGet, "/api/assets/"+id, nil), http.StatusNotFound)
	apitest.WantStatus(t, e.Do(user, http.MethodGet, "/api/jobs/"+jobID, nil), http.StatusNotFound)
	if len(e.Objects.Objects) != 1 {
		t.Fatalf("objects left: %v; want only the other asset's file", e.Objects.Objects)
	}
	if _, ok := e.Objects.Objects["assets/"+other+"/motion.glb"]; !ok {
		t.Fatal("delete removed another asset's object")
	}
}
