package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/api"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/testfs"
)

// The password auth had a logout route and an 8-hour TTL, and a test for
// each. Neither survives -- there is nothing to log out of, and the cookie
// does not expire. What replaces both is the same underlying guarantee, and
// it is the one the SPA's session probe depends on: a session this process
// did not mint is not a session.
func TestACookieThisProcessDidNotMintIsRejected(t *testing.T) {
	h := newTestServer(t)
	ts, client := authedClient(t, h)

	// Authenticated, to prove the request shape is otherwise fine.
	resp, err := client.Get(ts.URL + "/api/session")
	if err != nil {
		t.Fatalf("GET /api/session error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("probe status = %d, want 204", resp.StatusCode)
	}

	forged, err := http.NewRequest(http.MethodGet, ts.URL+"/api/session", nil)
	if err != nil {
		t.Fatalf("NewRequest error = %v", err)
	}
	forged.AddCookie(&http.Cookie{Name: "aicrme_session", Value: "not-a-session-this-server-issued"})
	forgedResp, err := http.DefaultClient.Do(forged)
	if err != nil {
		t.Fatalf("forged request error = %v", err)
	}
	defer forgedResp.Body.Close()
	if forgedResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for a session value this process never issued", forgedResp.StatusCode)
	}
}

// The operator restarting the binary is what replaces session expiry: the new
// process mints its own launch token and recognizes none of the old one's
// sessions, so a browser tab left open lands on 401 and the SPA can tell that
// from a network blip.
func TestASessionDoesNotSurviveTheProcessThatMintedIt(t *testing.T) {
	first := newTestServer(t)
	ts, client := authedClient(t, first)

	// A second server stands in for the next launch: same address, same
	// launch token value, entirely separate session state.
	second := newTestServer(t)
	replacement := httptest.NewServer(second)
	defer replacement.Close()

	req, err := http.NewRequest(http.MethodGet, replacement.URL+"/api/session", nil)
	if err != nil {
		t.Fatalf("NewRequest error = %v", err)
	}
	for _, c := range client.Jar.Cookies(mustParseURL(t, ts.URL)) {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 -- the previous process's session outlived it", resp.StatusCode)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v", raw, err)
	}
	return u
}

// api.New rejects an empty launch token for the reason it used to reject an
// empty password: a console that authenticates nothing is worse than one that
// refuses to start, because it looks like it is working.
func TestNewRejectsAnEmptyToken(t *testing.T) {
	_, err := api.New(api.Config{AICR: &aicrclient.Fake{}, WorkDir: t.TempDir(), Cluster: connectedCluster()},
		bus.New(8), engine.New(bus.New(8), engine.NewMemoryStore()), testfs.Static())
	if err == nil {
		t.Error("api.New() accepted an empty launch token")
	}
}

func TestEventStreamSinceQueryParamFallback(t *testing.T) {
	b := bus.New(64)
	srv, err := api.New(api.Config{
		Cluster: connectedCluster(),
		Token:   testToken,
		AICR:    &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, engine.New(b, engine.NewMemoryStore()), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	ts, client := authedClient(t, srv.Handler())

	b.Publish(bus.Event{Kind: bus.KindLog, Message: "one"})
	b.Publish(bus.Event{Kind: bus.KindLog, Message: "two"})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events?since=1", nil)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("stream error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}
