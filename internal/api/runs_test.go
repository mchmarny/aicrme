package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/api"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/testfs"
)

func TestCreateAndGetRun(t *testing.T) {
	b := bus.New(64)
	srv, err := api.New(api.Config{
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
		AICR: &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, engine.New(b, engine.NewMemoryStore()), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	ts, client := loggedInClient(t, srv.Handler())

	resp, err := client.Post(ts.URL+"/api/runs", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /api/runs error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}

	var created engine.Run
	if decErr := json.NewDecoder(resp.Body).Decode(&created); decErr != nil {
		t.Fatalf("decode error = %v", decErr)
	}
	if created.ID == "" {
		t.Fatal("created run has no ID")
	}

	got, err := client.Get(ts.URL + "/api/runs/" + created.ID)
	if err != nil {
		t.Fatalf("GET run error = %v", err)
	}
	defer got.Body.Close()
	if got.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", got.StatusCode, http.StatusOK)
	}
}

func TestGetUnknownRunIs404(t *testing.T) {
	b := bus.New(8)
	srv, _ := api.New(api.Config{
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
		AICR: &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, engine.New(b, engine.NewMemoryStore()), testfs.Static())
	ts, client := loggedInClient(t, srv.Handler())

	resp, err := client.Get(ts.URL + "/api/runs/does-not-exist")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

// failingStep always fails, so runs_test.go's Retry tests can drive a run to
// engine.StateFailed over HTTP without engine test doubles leaking out of
// internal/engine, matching decide_test.go's decisionStep pattern.
type failingStep struct{}

func (failingStep) Phase() engine.Phase { return engine.PhaseDiscover }
func (failingStep) Requires() []string  { return nil }
func (failingStep) Run(_ context.Context, _ *engine.Run, _ engine.Emit) error {
	return errors.New("boom")
}

func TestRetryReturnsTheRun(t *testing.T) {
	b := bus.New(64)
	srv, err := api.New(api.Config{
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
		AICR: &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, engine.New(b, engine.NewMemoryStore(), failingStep{}), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	ts, client := loggedInClient(t, srv.Handler())

	resp, err := client.Post(ts.URL+"/api/runs", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /api/runs error = %v", err)
	}
	var created engine.Run
	decErr := json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if decErr != nil {
		t.Fatalf("decode error = %v", decErr)
	}

	waitForRunState(t, client, ts.URL+"/api/runs/"+created.ID, engine.StateFailed)

	retryResp, err := client.Post(ts.URL+"/api/runs/"+created.ID+"/retry", "application/json", nil)
	if err != nil {
		t.Fatalf("retry error = %v", err)
	}
	defer retryResp.Body.Close()
	if retryResp.StatusCode != http.StatusOK {
		t.Fatalf("retry status = %d, want %d", retryResp.StatusCode, http.StatusOK)
	}
	var retried engine.Run
	if decErr := json.NewDecoder(retryResp.Body).Decode(&retried); decErr != nil {
		t.Fatalf("decode error = %v", decErr)
	}
	if retried.ID != created.ID {
		t.Errorf("retried run ID = %q, want %q", retried.ID, created.ID)
	}
}

func TestRetryOnRunningRunConflicts(t *testing.T) {
	ts, client := newDecideTestServer(t)

	resp, err := client.Post(ts.URL+"/api/runs", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /api/runs error = %v", err)
	}
	var created engine.Run
	decErr := json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if decErr != nil {
		t.Fatalf("decode error = %v", decErr)
	}

	waitForRunState(t, client, ts.URL+"/api/runs/"+created.ID, engine.StateAwaitingDecision)

	retryResp, err := client.Post(ts.URL+"/api/runs/"+created.ID+"/retry", "application/json", nil)
	if err != nil {
		t.Fatalf("retry error = %v", err)
	}
	defer retryResp.Body.Close()
	if retryResp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want %d", retryResp.StatusCode, http.StatusConflict)
	}
}

func TestRetryOnUnknownRunNotFound(t *testing.T) {
	ts, client := loggedInClient(t, newTestServer(t))

	resp, err := client.Post(ts.URL+"/api/runs/does-not-exist/retry", "application/json", nil)
	if err != nil {
		t.Fatalf("retry error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestSessionProbeReturns204WhenAuthed(t *testing.T) {
	ts, client := loggedInClient(t, newTestServer(t))

	resp, err := client.Get(ts.URL + "/api/session")
	if err != nil {
		t.Fatalf("GET /api/session error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
}

func TestSessionProbeReturns401WhenNot(t *testing.T) {
	h := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
