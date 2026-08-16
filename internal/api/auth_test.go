package api_test

import (
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

func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	srv, err := api.New(api.Config{
		Username:   "admin",
		Password:   "correct-horse",
		SessionTTL: time.Hour,
		LoginRate:  100,
		AICR:       &aicrclient.Fake{},
		WorkDir:    t.TempDir(),
	}, bus.New(64), engine.New(bus.New(64), engine.NewMemoryStore()), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	return srv.Handler()
}

func TestLogin(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantCode int
		wantSet  bool
	}{
		{name: "valid", body: `{"username":"admin","password":"correct-horse"}`, wantCode: http.StatusNoContent, wantSet: true},
		{name: "wrong password", body: `{"username":"admin","password":"nope"}`, wantCode: http.StatusUnauthorized},
		{name: "wrong username", body: `{"username":"root","password":"correct-horse"}`, wantCode: http.StatusUnauthorized},
		{name: "malformed", body: `{`, wantCode: http.StatusBadRequest},
	}
	h := newTestServer(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			cookie := rec.Header().Get("Set-Cookie")
			if tc.wantSet {
				if cookie == "" {
					t.Fatal("no session cookie set")
				}
				for _, attr := range []string{"HttpOnly", "SameSite=Strict"} {
					if !strings.Contains(cookie, attr) {
						t.Errorf("cookie missing %s: %s", attr, cookie)
					}
				}
			} else if cookie != "" {
				t.Errorf("unexpected cookie on failure: %s", cookie)
			}
		})
	}
}

func TestProtectedRoutesRequireSession(t *testing.T) {
	h := newTestServer(t)
	for _, path := range []string{"/api/events", "/api/runs"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
		})
	}
}

func TestLoginRateLimited(t *testing.T) {
	srv, err := api.New(api.Config{
		Username: "admin", Password: "pw", SessionTTL: time.Hour, LoginRate: 2, AICR: &aicrclient.Fake{},
		WorkDir: t.TempDir(),
	}, bus.New(8), engine.New(bus.New(8), engine.NewMemoryStore()), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	h := srv.Handler()

	var got429 bool
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"admin","password":"bad"}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Error("login was never rate limited")
	}
}

func TestHealthzIsPublic(t *testing.T) {
	h := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestEmptyPasswordRejected(t *testing.T) {
	_, err := api.New(api.Config{Username: "admin", Password: "", WorkDir: t.TempDir()}, bus.New(8),
		engine.New(bus.New(8), engine.NewMemoryStore()), testfs.Static())
	if err == nil {
		t.Error("api.New() accepted an empty password")
	}
}
