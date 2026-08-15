package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mchmarny/aicrme/internal/testfs"
)

// TestSPAHandlerMissingAssetIs404 guards against a stale/broken asset
// reference (e.g. a hashed bundle filename that no longer exists) silently
// getting a 200 text/html response in place of the JS or CSS the browser
// asked for, which fails in a confusing way client-side instead of a clean
// 404.
func TestSPAHandlerMissingAssetIs404(t *testing.T) {
	h := spaHandler(testfs.Static())
	req := httptest.NewRequest(http.MethodGet, "/assets/does-not-exist.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "text/html") {
		t.Errorf("missing asset served as html, Content-Type = %q", ct)
	}
}

// TestSPAHandlerUnknownRouteFallsBackToIndex confirms extensionless,
// navigational client-side routes (e.g. /runs/abc123) still resolve to
// index.html so a hard refresh on a client route works.
func TestSPAHandlerUnknownRouteFallsBackToIndex(t *testing.T) {
	h := spaHandler(testfs.Static())
	req := httptest.NewRequest(http.MethodGet, "/runs/abc123", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "aicrme") {
		t.Errorf("body = %q, want index.html content", rec.Body.String())
	}
}

// TestSPAHandlerPathTraversalCannotEscapeFS sets the request path directly
// (bypassing URL parsing/http.ServeMux's own path cleaning) so the handler's
// own defenses are what's under test, not the router in front of it.
func TestSPAHandlerPathTraversalCannotEscapeFS(t *testing.T) {
	h := spaHandler(testfs.Static())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.URL.Path = "/../../../../etc/passwd"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), "root:") {
		t.Fatal("path traversal request served real filesystem content")
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "aicrme") {
		t.Errorf("expected safe fallback to index.html, got status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestCleanPath(t *testing.T) {
	tests := map[string]string{
		"":         "index.html",
		"/":        "index.html",
		"/foo.js":  "foo.js",
		"/a/b.css": "a/b.css",
	}
	for in, want := range tests {
		if got := cleanPath(in); got != want {
			t.Errorf("cleanPath(%q) = %q, want %q", in, got, want)
		}
	}
}
