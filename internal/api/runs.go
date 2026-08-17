package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	// context.WithoutCancel: the run outlives this request by design --
	// Apply alone takes 10-20 minutes on real hardware. Handing the engine
	// the cancellable request context means the browser closing the tab
	// (or a proxy timing out) cancels the run mid-install, and a
	// store.Save failure under a canceled context would leave e.current
	// live and permanently 409 every new run.
	run, err := s.engine.Start(context.WithoutCancel(r.Context()))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	// context.WithoutCancel, same rationale as handleCreateRun: Retry can
	// relaunch a step that runs up to 20 minutes (Apply), so this run must
	// survive the request that kicked it off too.
	run, err := s.engine.Retry(context.WithoutCancel(r.Context()), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	// engine.Get does no long-running work of its own -- its only I/O is a
	// single store.Load fallback for a run this process didn't start -- so
	// the request's own context is threaded through directly, unlike
	// handleCreateRun/handleRetry's execution contexts.
	run, err := s.engine.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleDecide(w http.ResponseWriter, r *http.Request) {
	var decisions map[string]string
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&decisions); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	// r.Context() threads through directly: unlike Retry, Decide does not
	// launch any execution that outlives this request, so there is no
	// execution context to detach here. Decide itself detaches only the
	// cancellation of its own checkpoint save -- see engine.Decide's doc
	// comment and decideSaveTimeout for why an operator's already-acked
	// decision must not be rolled back by a closed browser tab.
	if err := s.engine.Decide(r.Context(), r.PathValue("id"), decisions); err != nil {
		writeErr(w, err)
		return
	}
	run, err := s.engine.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleDiscardRun(w http.ResponseWriter, r *http.Request) {
	// r.Context() threads through directly: Discard's only I/O is a single
	// store.Delete, and e.current is already cleared in-memory (under e.mu,
	// synchronously) before that call, so a canceled request loses at worst
	// the persisted ConfigMap deletion, not any in-memory state -- unlike
	// Decide, there is no acknowledged operator state this could roll back.
	if err := s.engine.Discard(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr maps AICR structured error codes onto HTTP status codes so the
// console's error contract matches AICR's.
func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	var se *aicrerrors.StructuredError
	if errors.As(err, &se) {
		switch se.Code {
		case aicrerrors.ErrCodeNotFound:
			code = http.StatusNotFound
		case aicrerrors.ErrCodeInvalidRequest:
			code = http.StatusBadRequest
		case aicrerrors.ErrCodeConflict:
			code = http.StatusConflict
		case aicrerrors.ErrCodeTimeout:
			code = http.StatusGatewayTimeout
		case aicrerrors.ErrCodeUnavailable:
			code = http.StatusServiceUnavailable
		case aicrerrors.ErrCodeUnauthorized:
			code = http.StatusUnauthorized
		case aicrerrors.ErrCodeRateLimitExceeded:
			code = http.StatusTooManyRequests
		case aicrerrors.ErrCodeMethodNotAllowed:
			code = http.StatusMethodNotAllowed
		case aicrerrors.ErrCodeInternal, aicrerrors.ErrCodeCanceled:
			code = http.StatusInternalServerError
		}
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
