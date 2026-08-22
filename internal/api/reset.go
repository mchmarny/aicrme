package api

import (
	"context"
	"encoding/json"
	"net/http"
)

// resetConfirmation is the body POST /api/runs/{id}/reset requires. The
// field is not a checkbox but a literal word the SPA sends only after the
// operator has been shown, and clicked past, the list of what will be
// removed and what will be skipped.
//
// It exists because Reset is the one operation in this console that
// destroys rather than creates, and because the URL alone would otherwise
// make it a one-request action any stray retry could trigger. Nothing in
// this process ever calls Reset on the operator's behalf (see engine.Reset)
// -- this is the HTTP half of that same rule.
type resetConfirmation struct {
	Confirm string `json:"confirm"`
}

// resetConfirmWord is the exact value the body must carry.
const resetConfirmWord = "reset"

// handleReset tears down what one run installed. Confirmation required.
//
// Detached the same way handleStop and handleCreateRun detach theirs, and
// for a stronger version of the same reason: the teardown runs for minutes
// (a helm uninstall per component, each with --wait), so it must outlive
// the browser tab that started it. A tab closed mid-teardown would
// otherwise cut the operation into exactly the half-removed state Reset
// exists to eliminate.
//
// Answers as soon as the teardown has been accepted and its state
// persisted, not when it finishes -- engine.Reset backgrounds the work the
// same way Start does. The SPA follows the outcome on the event stream.
func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	var body resetConfirmation
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	if body.Confirm != resetConfirmWord {
		http.Error(w, `reset requires a confirmation body: {"confirm":"reset"}`, http.StatusBadRequest)
		return
	}
	if err := s.engine.Reset(context.WithoutCancel(r.Context()), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	// r.Context() here, not the detached one above: a plain read with no
	// execution to outlive the request, the same split handleStop makes.
	run, err := s.engine.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}
