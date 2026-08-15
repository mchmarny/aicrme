package api

import (
	"encoding/json"
	"errors"
	"net/http"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.engine.Start(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.engine.Get(r.PathValue("id"))
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
	if err := s.engine.Decide(r.PathValue("id"), decisions); err != nil {
		writeErr(w, err)
		return
	}
	run, err := s.engine.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
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
