package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"
)

// sessionCookie carries the exchanged session for the life of the process.
const sessionCookie = "aicrme_session"

// launchToken authenticates one browser on one machine for the life of one
// process. It is not a credential, has no expiry of its own, and dies when the
// process does.
//
// It arrives once in the URL and is immediately exchanged for a cookie,
// rather than being held in memory and sent as a header, for two reasons that
// are both hard constraints rather than preferences:
//
//   - GET /api/events is a native EventSource (web/src/useEvents.ts), and
//     EventSource has no API for request headers. server.go's own comment on
//     requireSameOrigin already records this. A header token simply cannot
//     reach the timeline.
//   - A token held in memory does not survive a page refresh or a restored
//     tab, which would drop the operator to a dead screen mid-Apply with the
//     only copy of the token in a terminal they may have scrolled past.
//
// Putting the token in the EventSource URL instead would leak a live
// credential into browser history, the referrer on any outbound link, and
// this repo's own request logging.
//
// The exchange mints a fresh random session rather than storing the token
// itself in the cookie: the token is the thing that appears in a URL and
// therefore in browser history, and there is no reason for the value that
// authenticates every subsequent request to be the same string.
type launchToken struct {
	token string

	mu       sync.RWMutex
	sessions map[string]struct{}
}

func newLaunchToken(token string) *launchToken {
	return &launchToken{token: token, sessions: make(map[string]struct{})}
}

type sessionRequest struct {
	Token string `json:"token"`
}

// exchange trades the launch token for a session cookie. Constant-time
// compare because this is the one place a wrong guess is distinguishable from
// a right one, and a loopback attacker with a local process can make as many
// guesses as it likes.
func (l *launchToken) exchange(w http.ResponseWriter, r *http.Request) {
	var req sessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(l.token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	session, err := newSessionID()
	if err != nil {
		http.Error(w, "could not establish a session", http.StatusInternalServerError)
		return
	}
	l.mu.Lock()
	l.sessions[session] = struct{}{}
	l.mu.Unlock()

	// No Secure: loopback is plain HTTP, and a Secure cookie would simply
	// never be sent. requireSameOrigin is what stands in for it -- see its
	// comment on why SameSite alone does not cover a second localhost port.
	//
	// No MaxAge either: this cookie is a session cookie in the browser's sense
	// as well as this program's, and it authenticates exactly as long as the
	// process that minted it is alive to recognize it.
	// #nosec G124 -- Secure is deliberately unset: the console serves plain
	// HTTP on loopback, so a Secure cookie would never be sent back at all.
	// HttpOnly and SameSite=Strict are both set, and requireSameOrigin is
	// what covers the case SameSite cannot (a second port on localhost).
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    session,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// valid reports whether r carries a session this process minted.
func (l *launchToken) valid(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	l.mu.RLock()
	_, ok := l.sessions[c.Value]
	l.mu.RUnlock()
	return ok
}

// requireToken wraps h so only requests carrying an exchanged session reach
// it. It runs outside requireConnected, so an unauthenticated caller learns
// nothing about whether a cluster has been chosen.
func (s *Server) requireToken(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.launch.valid(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// newSessionID returns 256 bits of randomness, URL-safe. Same size as the
// launch token: the cookie is what authenticates every request after the
// exchange, so it is not the place to economize.
func newSessionID() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
