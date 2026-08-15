package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestLoginSweepsExpiredSessions guards against unbounded growth of the
// session map. Sessions are only ever deleted on logout or on a hit to that
// specific expired token in valid() — a session whose owner never returns
// (closed tab, refreshed page that overwrote the cookie, long-idle demo)
// would otherwise sit in the map forever. login is the only path that adds
// entries, so it is the natural place to reap anything already past its TTL.
func TestLoginSweepsExpiredSessions(t *testing.T) {
	a := newAuthenticator(Config{Username: "admin", Password: "pw", SessionTTL: time.Hour, LoginRate: 100})

	a.mu.Lock()
	a.sessions["stale-token"] = session{expires: time.Now().Add(-time.Minute)}
	a.sessions["still-live"] = session{expires: time.Now().Add(time.Hour)}
	seeded := len(a.sessions)
	a.mu.Unlock()
	if seeded != 2 {
		t.Fatalf("setup: sessions = %d, want 2", seeded)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"admin","password":"pw"}`))
	rec := httptest.NewRecorder()
	a.login(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("login status = %d, want %d", rec.Code, http.StatusNoContent)
	}

	a.mu.RLock()
	_, staleStillThere := a.sessions["stale-token"]
	_, liveStillThere := a.sessions["still-live"]
	count := len(a.sessions)
	a.mu.RUnlock()

	if staleStillThere {
		t.Error("expired session was not swept on login")
	}
	if !liveStillThere {
		t.Error("sweep incorrectly deleted a live session")
	}
	// still-live plus the one this login call just created.
	if count != 2 {
		t.Errorf("sessions after login = %d, want 2 (still-live + new)", count)
	}
}
