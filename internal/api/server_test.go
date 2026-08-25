package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/api"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/testfs"
)

// TestHandlerRoutesDoNotConflict guards against a regression to
// mux.Handle("GET /", spaHandler(...)): http.ServeMux (Go 1.22+) panics at
// registration time when a method-restricted subtree pattern ("GET /") and a
// method-open subtree pattern ("/api/") both match the same request and
// neither is a strict subset of the other's matches. Handler() must build
// cleanly.
func TestHandlerRoutesDoNotConflict(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Handler() panicked building routes: %v", r)
		}
	}()
	newTestServer(t)
}

// TestSPAMissingAssetIs404EndToEnd exercises the fix through the full
// Handler(), not just spaHandler in isolation, confirming securityHeaders
// and the top-level mux don't reintroduce the fallback-to-200 behavior.
func TestSPAMissingAssetIs404EndToEnd(t *testing.T) {
	h := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/assets/missing-bundle.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func newDrainableTestServer(t *testing.T) *api.Server {
	t.Helper()
	srv, err := api.New(api.Config{
		Cluster: connectedCluster(),
		Token:   testToken,
		AICR:    &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, bus.New(64), engine.New(bus.New(64), engine.NewMemoryStore()), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	return srv
}

// TestDrainRejectsMutations pins the hole Drain closes: canceling the
// in-flight run lands it in StateFailed, which isLive does not treat as
// live, so an unguarded POST /api/runs during the shutdown wait would start
// a fresh run that shutdown then kills mid-flight.
func TestDrainRejectsMutations(t *testing.T) {
	srv := newDrainableTestServer(t)
	srv.Drain()

	req := httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

// TestDrainKeepsSafeMethodsServing confirms a connected browser keeps
// watching its timeline through shutdown: safe methods (here, the public
// /healthz probe and an already-open /api/events stream) must not be
// rejected once draining begins.
func TestDrainKeepsSafeMethodsServing(t *testing.T) {
	srv := newDrainableTestServer(t)
	ts, client := authedClient(t, srv.Handler())
	srv.Drain()

	healthzResp, err := client.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz error = %v", err)
	}
	defer healthzResp.Body.Close()
	if healthzResp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want %d", healthzResp.StatusCode, http.StatusOK)
	}

	eventsResp, err := client.Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatalf("GET /api/events error = %v", err)
	}
	defer eventsResp.Body.Close()
	if eventsResp.StatusCode != http.StatusOK {
		t.Errorf("events status = %d, want %d", eventsResp.StatusCode, http.StatusOK)
	}
}

// TestDrainedEngineRejectsRunCreation covers the half requireNotDraining
// cannot: it gates the outer mux, so a POST /api/runs that clears the check
// microseconds before Drain() still reaches engine.Start. The engine's own
// draining flag is what refuses that request, and engine.ErrDraining carries
// ErrCodeUnavailable so writeErr answers 503 -- the same shape the middleware
// returns -- rather than a bare 500. The server here is deliberately never
// drained, so only the engine can be producing the status.
func TestDrainedEngineRejectsRunCreation(t *testing.T) {
	b := bus.New(64)
	eng := engine.New(b, engine.NewMemoryStore())
	srv, err := api.New(api.Config{
		Cluster: connectedCluster(),
		Token:   testToken,
		AICR:    &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, eng, testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	if cancelErr := eng.CancelAndWait(context.Background()); cancelErr != nil {
		t.Fatalf("CancelAndWait() error = %v", cancelErr)
	}

	ts, client := authedClient(t, srv.Handler())
	resp, err := client.Post(ts.URL+"/api/runs", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /api/runs error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

// TestNotDrainingByDefault guards against a middleware that trivially
// passes by rejecting everything: a fresh server that never called Drain
// must still accept a normal same-origin POST /api/runs.
func TestNotDrainingByDefault(t *testing.T) {
	h := newTestServer(t)
	ts, client := authedClient(t, h)

	resp, err := client.Post(ts.URL+"/api/runs", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /api/runs error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}
}
