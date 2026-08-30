package api

import (
	"net/http"

	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/version"
)

// BuildInfo is who this console is and what it is built against.
//
// Served from the PRE-CONNECT route group on purpose. It hangs off nothing in
// the cluster, so it answers before a context is chosen -- which is the whole
// point: the AICR version used to ride on ClusterInfo and was therefore absent
// on the one screen an operator sees first.
type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	// Date is the COMMIT date, not a build wall-clock. See version.Date.
	Date string `json:"date,omitempty"`
	// Digest is the sha256 of the running binary, empty when unreadable. It is
	// what ties the process on screen to an archive checked with
	// `gh attestation verify`.
	Digest string `json:"digest,omitempty"`
	// AICR is the AICR release this binary is built against. Every recipe
	// decision and validation verdict on screen comes from it.
	AICR string `json:"aicr"`
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, BuildInfo{
		Version: version.Version,
		Commit:  version.Commit,
		Date:    version.Date,
		Digest:  version.Digest(),
		AICR:    aicrclient.AICRVersion,
	})
}
