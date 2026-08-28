// Package api serves the console HTTP surface. It carries no business logic:
// every handler is a thin adapter over engine and bus.
package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync/atomic"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
)

// errNotConnected is what every gated route returns before a cluster has been
// chosen. Conflict rather than unavailable: the server is healthy and the
// request is well-formed, it just does not yet know which cluster it means.
var errNotConnected = aicrerrors.New(aicrerrors.ErrCodeConflict, "connect to a cluster first")

// Config is the server's runtime configuration.
type Config struct {
	// Token is the launch token this console prints in its URL and exchanges
	// once for a session cookie. See internal/api/token.go for why the
	// exchange exists rather than a header.
	Token string
	// AICR backs GET /api/options: it asks the live recipe catalog which
	// intents and platforms actually have an overlay for this cluster,
	// rather than the console offering a static list that can dead-end.
	AICR aicrclient.API
	// WorkDir is the containment boundary for GET /api/runs/{id}/bundle: a
	// run's bundle.path artifact must resolve inside this directory before
	// handleBundle will open anything under it.
	WorkDir string
	// Cluster is the process's one cluster connection. Every route that needs
	// a cluster is gated on it having been established.
	Cluster Cluster
}

// Cluster is the connect-state seam. internal/console owns the connection and
// implements this; the interface is declared here, in terms of values this
// package only marshals and never inspects, so the dependency stays one-way --
// console imports api, never the reverse.
type Cluster interface {
	// Contexts lists what the operator can connect to. It must not contact a
	// cluster: most contexts in a working kubeconfig are unreachable from
	// wherever the operator happens to be sitting.
	Contexts() (any, error)
	// Connect establishes the connection, or fails. It is single-assignment:
	// a second call returns a conflict, which is what stops in-session
	// cluster switching.
	Connect(ctx context.Context, contextName string) (any, error)
	// Connected reports whether a connection has been established.
	Connected() bool
	// Info returns the established connection, or false if there is none.
	Info() (any, bool)
}

// Server wires the HTTP routes.
type Server struct {
	launch   *launchToken
	bus      *bus.Bus
	engine   *engine.Engine
	static   fs.FS
	aicr     aicrclient.API
	options  aicrclient.OptionsCache
	workDir  string
	cluster  Cluster
	draining atomic.Bool
}

// New validates cfg and returns a Server.
func New(cfg Config, b *bus.Bus, e *engine.Engine, static fs.FS) (*Server, error) {
	if cfg.Token == "" {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "launch token must not be empty")
	}
	if cfg.AICR == nil {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "aicr client must not be nil")
	}
	if cfg.WorkDir == "" {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "work dir must not be empty")
	}
	// Required rather than defaulted to something permissive: a nil Cluster
	// would leave every cluster-touching route ungated, which is a false PASS
	// that no test would notice -- the routes answer, they just answer about
	// no cluster in particular.
	if cfg.Cluster == nil {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "cluster must not be nil")
	}
	return &Server{
		launch: newLaunchToken(cfg.Token), bus: b, engine: e, static: static, aicr: cfg.AICR, workDir: cfg.WorkDir,
		cluster: cfg.Cluster,
	}, nil
}

// Drain marks the server as shutting down: mutating routes start returning
// 503 immediately. It must be called before the shutdown wait begins, not
// after -- canceling the in-flight run lands it in StateFailed, which
// isLive does not treat as live, so an HTTP surface left fully open during
// that wait would happily accept a POST /api/runs that shutdown then kills
// mid-flight. Idempotent and safe to call from the shutdown goroutine with
// requests still arriving concurrently.
func (s *Server) Drain() {
	s.draining.Store(true)
}

// requireNotDraining rejects state-changing requests once shutdown has
// begun. Canceling the in-flight run leaves it StateFailed, which isLive
// does not treat as live -- so without this, a POST /api/runs arriving
// during the shutdown wait would start a fresh run that shutdown then kills
// mid-flight. Safe methods keep serving so a connected browser watches the
// timeline through shutdown.
func (s *Server) requireNotDraining(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			h.ServeHTTP(w, r)
			return
		}
		if s.draining.Load() {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// requireConnected gates every route that needs a cluster behind an
// established connection. This is a state gate, not an auth gate -- the auth
// middleware is separate and runs first. 409 rather than 503: the request is
// well-formed and the server is healthy, it just does not yet know which
// cluster the operator means.
func (s *Server) requireConnected(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cluster.Connected() {
			writeErr(w, errNotConnected)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// handleContexts lists what the operator can connect to. It reads the
// kubeconfig and contacts nothing.
func (s *Server) handleContexts(w http.ResponseWriter, _ *http.Request) {
	contexts, err := s.cluster.Contexts()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, contexts)
}

// handleConnect establishes this process's one cluster connection. A second
// call conflicts: the connection is single-assignment, and switching clusters
// means restarting the binary.
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Context string `json:"context"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	if req.Context == "" {
		writeErr(w, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "context must not be empty"))
		return
	}
	// r.Context() threads through directly rather than being detached: unlike
	// a run, a connect that outlives the request it came from has nobody to
	// report to, and the connector's own connectTimeout already bounds it.
	info, err := s.cluster.Connect(r.Context(), req.Context)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// handleCluster reports the connection this process already has.
//
// It exists for the reload. A tab restored after a connect carries a live
// session cookie and no memory of which cluster this console is on, and the
// connection is single-assignment -- so POST /api/connect would answer 409 and
// leave the operator stuck on a Connect screen refusing to connect them to the
// cluster they are already connected to. This is how the SPA learns to skip it.
//
// Ungated, and 409 when there is no connection: this is one of the routes that
// has to answer before a cluster exists, because "is there one" is the
// question it is for.
func (s *Server) handleCluster(w http.ResponseWriter, _ *http.Request) {
	info, ok := s.cluster.Info()
	if !ok {
		writeErr(w, errNotConnected)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// Handler returns the fully routed http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Outside requireToken, because it is the exchange that produces what
	// requireToken checks for.
	mux.HandleFunc("POST /api/session", s.launch.exchange)

	// gated carries every route that needs a cluster; protected carries the
	// three that do not and delegates the rest to it through the connect gate.
	// Choosing a cluster and probing the session both have to work before a
	// connection exists -- the first two are how one comes to exist, and the
	// third answers a question about the session rather than the cluster.
	gated := http.NewServeMux()
	gated.HandleFunc("GET /api/events", s.handleEvents)
	gated.HandleFunc("GET /api/options", s.handleOptions)
	gated.HandleFunc("POST /api/runs", s.handleCreateRun)
	gated.HandleFunc("GET /api/runs/{id}", s.handleGetRun)
	gated.HandleFunc("POST /api/runs/{id}/decide", s.handleDecide)
	gated.HandleFunc("POST /api/runs/{id}/retry", s.handleRetry)
	gated.HandleFunc("POST /api/runs/{id}/stop", s.handleStop)
	gated.HandleFunc("POST /api/runs/{id}/reset", s.handleReset)
	gated.HandleFunc("DELETE /api/runs/{id}", s.handleDiscardRun)
	gated.HandleFunc("GET /api/runs/{id}/bundle", s.handleBundle)
	gated.HandleFunc("GET /api/runs/{id}/log", s.handleRunLog)

	protected := http.NewServeMux()
	protected.HandleFunc("GET /api/contexts", s.handleContexts)
	protected.HandleFunc("POST /api/connect", s.handleConnect)
	protected.HandleFunc("GET /api/cluster", s.handleCluster)
	// GET /api/session exists so the SPA can tell a dead console from a
	// network blip: EventSource surfaces no HTTP status on error, so without
	// this probe the console had no way to learn its session was no longer
	// recognized and stuck on "reconnecting..." forever. The cookie does not
	// expire any more, but the process that minted it can exit -- and the one
	// that replaces it will not know the old session. requireToken supplies
	// the 401; a live session just needs a 204.
	protected.HandleFunc("GET /api/session", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	protected.Handle("/api/", s.requireConnected(gated))
	mux.Handle("/api/", s.requireToken(protected))

	// Unrestricted method, not "GET /": http.ServeMux (Go 1.22+) treats "GET /"
	// and "/api/" as conflicting patterns — neither is a strict subset of the
	// other's matches (one is method-restricted, the other prefix-restricted)
	// — and panics at registration time. "/" with no method matches strictly
	// more than "/api/" does, so it can only ever apply as the fallback.
	mux.Handle("/", spaHandler(s.static))
	return securityHeaders(requireSameOrigin(s.requireNotDraining(mux)))
}

// requireSameOrigin rejects state-changing requests that did not originate
// from this same origin. SameSite=Strict does not cover this on its own:
// SameSite is computed from the registrable "site" (scheme + registrable
// domain), not the full origin, so two different ports on localhost — e.g.
// a console on :8080 and an unrelated dev server on :3000 — count as
// same-site, and the session cookie is sent on a cross-origin POST from one
// to the other regardless of SameSite. Wrapping the whole handler here,
// rather than checking inside each mutating handler, means a route added
// later can't forget the check. Safe methods are exempted unconditionally,
// so GET /api/events — which EventSource cannot attach custom headers to —
// keeps working.
func requireSameOrigin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			h.ServeHTTP(w, r)
			return
		}
		if !sameOrigin(r) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// sameOrigin reports whether r was issued by this same origin. It checks
// Sec-Fetch-Site first — sent by every browser since 2023 and not settable
// by page script — and falls back to comparing the Origin header's host
// against the request's Host for older clients. A request carrying neither
// header is ordinarily same-origin browser traffic or a non-browser client
// (curl, a health probe), and is allowed — UNLESS its Content-Type is one a
// plain HTML <form> can produce. A cross-site <form> submission is a
// navigation, not a fetch/XHR call, and needs neither JavaScript nor a
// readable response to work: it is the one request shape that can reach a
// mutating handler without either header ever being set.
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "":
		// Fall through to the Origin check.
	case "same-origin":
		return true
	default:
		return false
	}

	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		return err == nil && u.Host == r.Host
	}

	return !isFormContentType(r.Header.Get("Content-Type"))
}

// isFormContentType reports whether ct is one of the three content types a
// plain HTML <form> can submit without any script: application/x-www-form-
// urlencoded (the default), multipart/form-data, and text/plain.
func isFormContentType(ct string) bool {
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	switch mediaType {
	case "application/x-www-form-urlencoded", "multipart/form-data", "text/plain":
		return true
	default:
		return false
	}
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

// contentSecurityPolicy assumes a self-contained SPA: its own JS and CSS,
// same origin, no third-party origins. Verified against the real Task 5
// build (`cd web && npm run build`, output inspected under web/dist):
//   - style-src 'self' holds. The Vite production build extracts CSS to an
//     external, content-hashed stylesheet linked via <link rel="stylesheet">,
//     not inline <style> blocks.
//   - img-src needs data: in addition to 'self'. Vite inlines any imported
//     asset under its 4096-byte assetsInlineLimit as a data: URI directly in
//     the JS bundle rather than emitting a separate file; a same-origin-only
//     img-src would silently block that image at runtime. The current
//     bundle imports no such asset (grepping web/dist/assets/*.{js,css} for
//     data:image|font|application finds nothing today), but importing one
//     tiny SVG and rebuilding reproduced a `data:image/svg+xml,...` URI in
//     the output, confirming the failure mode this guards against before it
//     ever ships.
//
// If a future UI library injects inline styles at runtime (CSS-in-JS,
// inline style= attributes for dynamic positioning), add 'unsafe-inline' to
// style-src only — never to default-src or script-src.
const contentSecurityPolicy = "default-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'"

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		// Run state and session cookies must never be cached; static assets
		// (content-hashed, served from s.static) are deliberately excluded so
		// they stay cacheable.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		h.ServeHTTP(w, r)
	})
}
