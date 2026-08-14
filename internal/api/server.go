// Package api serves the console HTTP surface. It carries no business logic:
// every handler is a thin adapter over engine and bus.
package api

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
)

// Config is the server's runtime configuration.
type Config struct {
	Username   string
	Password   string
	SessionTTL time.Duration
	// LoginRate is the burst and per-minute ceiling on login attempts.
	LoginRate int
	// TLS marks the session cookie Secure.
	TLS bool
}

// Server wires the HTTP routes.
type Server struct {
	auth   *authenticator
	bus    *bus.Bus
	engine *engine.Engine
	static fs.FS
}

// New validates cfg and returns a Server.
func New(cfg Config, b *bus.Bus, e *engine.Engine, static fs.FS) (*Server, error) {
	if cfg.Password == "" {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "password must not be empty")
	}
	if cfg.Username == "" {
		cfg.Username = "admin"
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 8 * time.Hour
	}
	if cfg.LoginRate <= 0 {
		cfg.LoginRate = 10
	}
	return &Server{auth: newAuthenticator(cfg), bus: b, engine: e, static: static}, nil
}

// Handler returns the fully routed http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /api/login", s.auth.login)
	mux.HandleFunc("POST /api/logout", s.auth.logout)

	protected := http.NewServeMux()
	protected.HandleFunc("GET /api/events", s.handleEvents)
	protected.HandleFunc("POST /api/runs", s.handleCreateRun)
	protected.HandleFunc("GET /api/runs/{id}", s.handleGetRun)
	protected.HandleFunc("POST /api/runs/{id}/decide", s.handleDecide)
	mux.Handle("/api/", s.auth.require(protected))

	// Unrestricted method, not "GET /": http.ServeMux (Go 1.22+) treats "GET /"
	// and "/api/" as conflicting patterns — neither is a strict subset of the
	// other's matches (one is method-restricted, the other prefix-restricted)
	// — and panics at registration time. "/" with no method matches strictly
	// more than "/api/" does, so it can only ever apply as the fallback.
	mux.Handle("/", spaHandler(s.static))
	return securityHeaders(mux)
}

// spaHandler serves static assets, falling back to index.html so client-side
// routes resolve on a hard refresh. A miss on a path that names a file (has
// an extension, e.g. /assets/app-abc123.js) is reported as a real 404
// instead: silently handing the browser an HTML document in place of the JS
// or CSS it asked for doesn't let the SPA route resolve anything, it just
// breaks the asset load in a confusing way.
func spaHandler(static fs.FS) http.Handler {
	files := http.FileServer(http.FS(static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := fs.Stat(static, cleanPath(r.URL.Path)); err != nil {
			if isAssetPath(r.URL.Path) {
				http.NotFound(w, r)
				return
			}
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	})
}

// isAssetPath reports whether p's final segment looks like a filename
// (contains a dot) rather than a client-side route.
func isAssetPath(p string) bool {
	return strings.Contains(path.Base(p), ".")
}

func cleanPath(p string) string {
	if p == "" || p == "/" {
		return "index.html"
	}
	return p[1:]
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		h.ServeHTTP(w, r)
	})
}
