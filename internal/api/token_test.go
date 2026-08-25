package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPostSessionExchangesTheTokenForACookie(t *testing.T) {
	ts, client := authedClient(t, newTestServer(t))

	probe, err := client.Get(ts.URL + "/api/session")
	if err != nil {
		t.Fatalf("GET /api/session error = %v", err)
	}
	defer func() { _ = probe.Body.Close() }()
	if probe.StatusCode != http.StatusNoContent {
		t.Errorf("probe status = %d, want 204 -- the cookie did not authenticate", probe.StatusCode)
	}
}

func TestSessionCookieIsHttpOnlyAndStrict(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t))
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/session", "application/json",
		strings.NewReader(`{"token":"`+testToken+`"}`))
	if err != nil {
		t.Fatalf("POST /api/session error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var got *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "aicrme_session" {
			got = c
		}
	}
	if got == nil {
		t.Fatal("no session cookie was set")
	}
	if !got.HttpOnly {
		t.Error("session cookie is readable from JavaScript; an XSS in the SPA would exfiltrate it")
	}
	if got.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, want Strict", got.SameSite)
	}
	if got.Secure {
		t.Error("Secure is set on a cookie served over loopback HTTP; the browser would never send it back")
	}
	if got.Value == testToken {
		t.Error("the cookie carries the launch token itself -- the value that appears in browser history should not be the one that authenticates every later request")
	}
}

func TestPostSessionRejectsTheWrongToken(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t))
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/session", "application/json", strings.NewReader(`{"token":"nope"}`))
	if err != nil {
		t.Fatalf("POST /api/session error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if len(resp.Cookies()) != 0 {
		t.Error("a rejected exchange still set a cookie")
	}
}

func TestPostSessionRejectsAMalformedBody(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t))
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/session", "application/json", strings.NewReader(`{`))
	if err != nil {
		t.Fatalf("POST /api/session error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// This is the regression a header-based token would have shipped: the event
// stream is a native EventSource, which cannot attach a header, so the cookie
// is the only thing that can authenticate it.
func TestTheEventStreamAuthenticatesByCookie(t *testing.T) {
	ts, client := authedClient(t, newTestServer(t))

	// No custom headers, exactly as EventSource would issue it.
	resp, err := client.Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatalf("GET /api/events error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
}

func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t))
	defer ts.Close()

	for _, path := range []string{"/api/events", "/api/runs", "/api/contexts", "/api/session"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s error = %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s status = %d, want 401", path, resp.StatusCode)
		}
	}
}

// The token gate runs outside the connect gate, so an unauthenticated caller
// cannot use the difference between 401 and 409 to learn whether this console
// has picked a cluster.
func TestUnauthenticatedCallersLearnNothingAboutConnectState(t *testing.T) {
	ts := httptest.NewServer(serverWithCluster(t, &fakeCluster{connected: false}))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/options")
	if err != nil {
		t.Fatalf("GET /api/options error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 rather than the connect gate's 409", resp.StatusCode)
	}
}

// /healthz answers before the exchange: it is what a supervisor or an e2e
// script polls to know the port is up, and it says nothing about the cluster
// or the run.
func TestHealthzNeedsNoSession(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}
