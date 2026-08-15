package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const sessionCookie = "aicrme_session"

type session struct {
	expires time.Time
}

type authenticator struct {
	username string
	password string
	ttl      time.Duration
	secure   bool

	mu       sync.RWMutex
	sessions map[string]session

	limiter *rate.Limiter
}

func newAuthenticator(cfg Config) *authenticator {
	perSecond := float64(cfg.LoginRate) / 60.0
	return &authenticator{
		username: cfg.Username,
		password: cfg.Password,
		ttl:      cfg.SessionTTL,
		secure:   cfg.TLS,
		sessions: make(map[string]session),
		limiter:  rate.NewLimiter(rate.Limit(perSecond), cfg.LoginRate),
	}
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (a *authenticator) login(w http.ResponseWriter, r *http.Request) {
	if !a.limiter.Allow() {
		http.Error(w, "too many login attempts", http.StatusTooManyRequests)
		return
	}
	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	// Compare both fields unconditionally so the response time does not leak
	// which one was wrong.
	userOK := subtle.ConstantTimeCompare([]byte(req.Username), []byte(a.username))
	passOK := subtle.ConstantTimeCompare([]byte(req.Password), []byte(a.password))
	if userOK&passOK != 1 {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		http.Error(w, "session creation failed", http.StatusInternalServerError)
		return
	}
	token := hex.EncodeToString(raw[:])
	now := time.Now()

	a.mu.Lock()
	a.sweepExpiredLocked(now)
	a.sessions[token] = session{expires: now.Add(a.ttl)}
	a.mu.Unlock()

	// #nosec G124 -- HttpOnly and SameSite are literal true/Strict; Secure is
	// intentionally cfg-driven (Config.TLS) rather than a literal, since this
	// console also serves over plain HTTP on local/Kind demo clusters, where
	// a hardcoded Secure cookie would never be sent back by the browser.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteStrictMode,
		Expires:  now.Add(a.ttl),
	})
	w.WriteHeader(http.StatusNoContent)
}

// sweepExpiredLocked deletes sessions past their expiry. login is the only
// path that adds entries to the map, so sweeping here keeps the store's size
// bounded by the number of currently-live sessions rather than growing
// without bound over the course of a long-running demo, where a browser can
// log in repeatedly (tab reloads, multiple sessions) without ever calling
// logout on the sessions it abandons. Callers must hold a.mu.
func (a *authenticator) sweepExpiredLocked(now time.Time) {
	for token, s := range a.sessions {
		if now.After(s.expires) {
			delete(a.sessions, token)
		}
	}
}

func (a *authenticator) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	// #nosec G124 -- see the identical justification on the login cookie above.
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (a *authenticator) valid(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	a.mu.RLock()
	s, ok := a.sessions[c.Value]
	a.mu.RUnlock()
	if !ok {
		return false
	}
	if time.Now().After(s.expires) {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
		return false
	}
	return true
}

// require wraps h so only requests carrying a live session reach it.
func (a *authenticator) require(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.valid(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}
