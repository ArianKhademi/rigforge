// Package apitest builds the real router on top of in-memory fakes, so handler
// tests go through routing, JWT verification and JSON encoding exactly as
// production requests do, without Docker.
package apitest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ArianKhademi/rigforge/api/internal/auth"
	"github.com/ArianKhademi/rigforge/api/internal/auth/authtest"
	"github.com/ArianKhademi/rigforge/api/internal/config"
	"github.com/ArianKhademi/rigforge/api/internal/server"
	"github.com/ArianKhademi/rigforge/api/internal/storage"
	"github.com/ArianKhademi/rigforge/api/internal/store"
)

// PartSize is deliberately tiny so tests can upload multi-part files of a few bytes.
const PartSize = 10

type Env struct {
	T       testing.TB
	Store   *store.Memory
	Objects *storage.Fake
	Queue   *Queue
	Events  *Events
	Issuer  *authtest.Issuer
	Server  *server.Server
}

func New(t testing.TB) *Env {
	t.Helper()
	e := &Env{
		T:       t,
		Store:   store.NewMemory(),
		Objects: storage.NewFake(),
		Queue:   &Queue{},
		Events:  &Events{subs: map[string][]chan struct{}{}},
		Issuer:  authtest.New(t),
	}
	e.Server = server.New(server.Deps{
		Store:    e.Store,
		Objects:  e.Objects,
		Queue:    e.Queue,
		Events:   e.Events,
		Verifier: auth.NewVerifier(e.Issuer.JWKSURL(), authtest.IssuerName, authtest.Audience),
		Upload: config.Upload{
			PartSize:       PartSize,
			MaxSize:        1 << 20,
			PresignTTL:     15 * time.Minute,
			IdleTimeout:    24 * time.Hour,
			ReaperInterval: time.Hour,
		},
	})
	return e
}

// Token returns a valid one-hour token for the given user.
func (e *Env) Token(user string) string {
	return e.Issuer.Token(e.T, user, time.Hour)
}

// Do performs a request as user (empty = unauthenticated) with an optional
// JSON body and returns the recorded response.
func (e *Env) Do(user, method, path string, body any) *httptest.ResponseRecorder {
	e.T.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			e.T.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != "" {
		req.Header.Set("Authorization", "Bearer "+e.Token(user))
	}
	rec := httptest.NewRecorder()
	e.Server.Router.ServeHTTP(rec, req)
	return rec
}

// Decode unmarshals a response body, failing the test on bad JSON.
func Decode[T any](t testing.TB, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

// ErrorCode extracts error.code from the api's error envelope.
func ErrorCode(t testing.TB, rec *httptest.ResponseRecorder) string {
	t.Helper()
	body := Decode[struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}](t, rec)
	return body.Error.Code
}

// WantStatus fails the test unless the response has the given status.
func WantStatus(t testing.TB, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, want, rec.Body.String())
	}
}

// Queue is a fake job queue that records what was enqueued.
type Queue struct {
	mu   sync.Mutex
	jobs []store.Job
	Fail bool // when true, Enqueue returns an error (Redis is "down")
}

func (q *Queue) Enqueue(_ context.Context, j *store.Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.Fail {
		return errors.New("queue unavailable")
	}
	q.jobs = append(q.jobs, *j)
	return nil
}

func (q *Queue) Jobs() []store.Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]store.Job(nil), q.jobs...)
}

// Events is a fake pub/sub: tests call Publish to wake SSE streams.
type Events struct {
	mu   sync.Mutex
	subs map[string][]chan struct{}
}

func (e *Events) Subscribe(_ context.Context, jobID string) (<-chan struct{}, func(), error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch := make(chan struct{}, 1)
	e.subs[jobID] = append(e.subs[jobID], ch)
	return ch, func() {}, nil
}

func (e *Events) Publish(jobID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ch := range e.subs[jobID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Subscribers reports how many streams are listening for a job.
func (e *Events) Subscribers(jobID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.subs[jobID])
}
