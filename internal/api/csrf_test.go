package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSameOriginRequestSucceeds(t *testing.T) {
	h := newTestServer(t)
	ts, client := authedClient(t, h)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/runs", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("NewRequest error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}
}

func TestSecFetchSiteCrossSiteRejected(t *testing.T) {
	h := newTestServer(t)
	ts, client := authedClient(t, h)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/runs", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("NewRequest error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

// TestSecFetchSiteSameSiteRejected guards the precise attack the same-origin
// check exists for: a page on http://localhost:3000 posting to this console
// on http://localhost:8080. Those two are different origins but the same
// registrable "site" — Sec-Fetch-Site reports "same-site" for that request,
// distinct from both "same-origin" and "cross-site", and the switch in
// sameOrigin must fall through its default case to reject it rather than
// matching neither case and allowing it through unchecked.
func TestSecFetchSiteSameSiteRejected(t *testing.T) {
	h := newTestServer(t)
	ts, client := authedClient(t, h)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/runs", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("NewRequest error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-site")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

func TestCrossOriginCreateRunRejected(t *testing.T) {
	h := newTestServer(t)
	ts, client := authedClient(t, h)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/runs", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("NewRequest error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:3000")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

// TestCrossOriginDecideRejected uses a run ID that does not exist. If the
// same-origin check runs before the handler (as it must, being middleware),
// the response is 403; if it were missing or bypassed, handleDecide would
// run and answer 404 instead. The distinct status proves where in the chain
// the rejection happens.
func TestCrossOriginDecideRejected(t *testing.T) {
	h := newTestServer(t)
	ts, client := authedClient(t, h)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/runs/does-not-exist/decide",
		strings.NewReader(`{"intent":"training"}`))
	if err != nil {
		t.Fatalf("NewRequest error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:3000")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want %d (not 404 — the same-origin check must run before handleDecide)", resp.StatusCode, http.StatusForbidden)
	}
}

// TestFormContentTypeWithoutOriginHeadersRejected mimics a cross-site
// <form enctype="text/plain"> submission: a single field whose name is
// itself a JSON object serializes as `{"intent":"training"}=\r\n`, and
// json.Decoder.Decode happily parses the leading JSON value and ignores the
// trailing "=\r\n" garbage. A real <form> submission is a navigation, not a
// fetch/XHR call, so it carries neither Sec-Fetch-Site nor Origin the way
// this test constructs it — the one request shape that can reach a
// mutating handler with neither anti-CSRF header present.
func TestFormContentTypeWithoutOriginHeadersRejected(t *testing.T) {
	h := newTestServer(t)
	ts, client := authedClient(t, h)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/runs/does-not-exist/decide",
		strings.NewReader(`{"intent":"training"}=`+"\r\n"))
	if err != nil {
		t.Fatalf("NewRequest error = %v", err)
	}
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

// TestEventsUnaffectedByCSRFMiddleware confirms GET /api/events, which
// EventSource cannot attach Sec-Fetch-Site or Origin to on its own, is
// unaffected by requireSameOrigin: it is a safe method and is exempted
// unconditionally, with no header simulation needed.
func TestEventsUnaffectedByCSRFMiddleware(t *testing.T) {
	h := newTestServer(t)
	ts, client := authedClient(t, h)

	resp, err := client.Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatalf("request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
}

func TestSecurityHeadersIncludeCSP(t *testing.T) {
	h := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "style-src 'self'", "img-src 'self' data:", "connect-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP = %q, missing %q", csp, want)
		}
	}
}

func TestAPIResponsesAreNotCached(t *testing.T) {
	h := newTestServer(t)
	ts, client := authedClient(t, h)

	resp, err := client.Get(ts.URL + "/api/runs/does-not-exist")
	if err != nil {
		t.Fatalf("request error = %v", err)
	}
	defer resp.Body.Close()
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-store")
	}
}

func TestStaticAssetsAreNotMarkedNoStore(t *testing.T) {
	h := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if cc := rec.Header().Get("Cache-Control"); cc == "no-store" {
		t.Errorf("static asset response marked no-store")
	}
}
