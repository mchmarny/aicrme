package api

import (
	"net/http"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// handleSurvey reports the AICR components already on the connected cluster.
//
// Read-only, and in the PRE-CONNECT group beside GET /api/cluster. That
// placement is the requirement, not a convenience: the survey is offered at
// Connect, before any run exists, and a gated route would answer 409 on
// exactly the screen this feature is for.
//
// The cluster UID comes from the connection this process holds, never from the
// request. It is echoed into the response so a later removal can be refused if
// the console has since been pointed somewhere else, and a client-supplied
// value would defeat the guard entirely.
//
// A failure is surfaced rather than smoothed into an empty result. An empty
// survey renders as "this cluster is clean", which is the one wrong answer an
// operator would act on destructively.
func (s *Server) handleSurvey(w http.ResponseWriter, r *http.Request) {
	if s.surveyor == nil {
		writeErr(w, aicrerrors.New(aicrerrors.ErrCodeUnavailable,
			"this console was built without a cluster survey"))
		return
	}
	uid, ok := s.connectedClusterUID()
	if !ok {
		writeErr(w, errNotConnected)
		return
	}
	survey, err := s.surveyor.Survey(r.Context(), uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, survey)
}
