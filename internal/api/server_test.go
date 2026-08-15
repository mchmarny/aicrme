package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
