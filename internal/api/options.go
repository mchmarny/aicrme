package api

import (
	"log/slog"
	"net/http"

	"github.com/mchmarny/aicrme/internal/aicrclient"
)

// handleOptions returns the two decisions the console ever asks for --
// intent and platform -- filtered to what the AICR catalog can actually
// resolve (spec §2: "filtered to those with an overlay matching this
// cluster's coordinates"). Everything else -- service, accelerator, OS,
// component set, versions, values -- is derived by the AICR recipe engine
// and never offered as a choice.
//
// The filter is keyed on the most recently started run's own snapshot, once
// Discover has produced one: aicrclient.AvailableOptions asks the live
// catalog which pairs have an overlay for that cluster's own detected
// service, rather than a value hardcoded in this handler. Before any run has
// produced a snapshot -- including before the very first run starts --
// there is no cluster-specific coordinate yet, so the filter runs
// unconstrained by service: still a real, catalog-verified answer, just not
// yet narrowed to this cluster. See aicrclient.AvailableOptions for the full
// reasoning and internal/aicrclient/options_test.go and
// internal/steps/options_cross_test.go for what pins it against real
// resolution.
func (s *Server) handleOptions(w http.ResponseWriter, r *http.Request) {
	var service string
	if run := s.engine.Current(); run != nil {
		if raw := run.Artifacts["snapshot.yaml"]; len(raw) > 0 {
			svc, err := aicrclient.ServiceFromSnapshot(s.aicr, raw)
			if err != nil {
				slog.Warn("options: deriving cluster service from snapshot failed, filtering catalog-wide",
					"run", run.ID, "error", err)
			} else {
				service = svc
			}
		}
	}

	opts, err := aicrclient.AvailableOptions(r.Context(), s.aicr, service)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, opts)
}
