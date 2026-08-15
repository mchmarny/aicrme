package api_test

import (
	"context"
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

func TestLogoutClearsSession(t *testing.T) {
	h := newTestServer(t)
	ts, client := loggedInClient(t, h)

	resp, err := client.Post(ts.URL+"/api/runs", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /api/runs error = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("pre-logout status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}

	logoutResp, err := client.Post(ts.URL+"/api/logout", "application/json", nil)
	if err != nil {
		t.Fatalf("logout error = %v", err)
	}
	defer logoutResp.Body.Close()
	if logoutResp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want %d", logoutResp.StatusCode, http.StatusNoContent)
	}

	afterResp, err := client.Get(ts.URL + "/api/runs/anything")
	if err != nil {
		t.Fatalf("post-logout request error = %v", err)
	}
	defer afterResp.Body.Close()
	if afterResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status after logout = %d, want %d", afterResp.StatusCode, http.StatusUnauthorized)
	}
}

func TestSessionExpiresAndIsUnauthorized(t *testing.T) {
	srv, err := api.New(api.Config{
		Username: "admin", Password: "correct-horse", SessionTTL: 10 * time.Millisecond, LoginRate: 100,
		AICR: &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, bus.New(8), engine.New(bus.New(8), engine.NewMemoryStore()), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	ts, client := loggedInClient(t, srv.Handler())

	time.Sleep(30 * time.Millisecond)

	resp, err := client.Get(ts.URL + "/api/runs/anything")
	if err != nil {
		t.Fatalf("request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestConfigDefaultsApply(t *testing.T) {
	srv, err := api.New(api.Config{Password: "pw", AICR: &aicrclient.Fake{}, WorkDir: t.TempDir()}, bus.New(8), engine.New(bus.New(8), engine.NewMemoryStore()), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"admin","password":"pw"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("login with default username status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestEventStreamSinceQueryParamFallback(t *testing.T) {
	b := bus.New(64)
	srv, err := api.New(api.Config{
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
		AICR: &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, engine.New(b, engine.NewMemoryStore()), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	ts, client := loggedInClient(t, srv.Handler())

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
