package api

import (
	"context"
	"net/http"
)

// handleStop is the only way an operator can move a run out of StateActive
// -- see engine.Stop's doc comment for the full contract (idempotent,
// foreground deletion, waits for absence, never automatic). Detached the
// same way handleCreateRun and handleRetry detach their execution context:
// Stop's own Delete-then-WaitAbsent round trip can run for as long as
// stopWaitAbsentTimeout allows, so this call must survive the browser tab
// that issued it exactly as an install already does, rather than being cut
// short by a closed tab into an ambiguous state the operator cannot see.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.Stop(context.WithoutCancel(r.Context()), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	// r.Context() here, not the detached one above: this is a plain read
	// with no execution to outlive the request, the same reasoning
	// handleDecide's own follow-up Get uses (Stop, like Decide, returns
	// only an error, so both handlers fetch the run afterward to answer
	// with its current state).
	run, err := s.engine.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}
