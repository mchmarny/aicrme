package api

import "net/http"

// handleOptions returns the two decisions the console ever asks for --
// intent and platform -- filtered to what this cluster can actually run
// (spec §2: "filtered to those with an overlay matching this cluster's
// coordinates"). Everything else -- service, accelerator, OS, component set,
// versions, values -- is derived by the AICR recipe engine and never offered
// as a choice.
//
// The response body is aicrclient.Options. Read platformsByIntent, not the
// flat platforms list, to decide what to offer: the flat list is the union
// across intents and so still contains platforms that are dead for the
// intent currently selected.
//
// CLIENT CONTRACT -- this response is not safe to fetch once on mount and
// cache. Two states are possible:
//
//   - provisional=false. Every pair in platformsByIntent was verified by
//     actually resolving it against this run's snapshot, so no offered pair
//     can dead-end in Recommend. This is final; present it as such.
//
//   - provisional=true. No usable snapshot yet -- none stored, or it carried
//     nothing the fingerprint could turn into a service, or it was corrupt
//     and could not be parsed at all. The answer is then a widened
//     catalog-wide upper bound: it contains every pair that will eventually
//     be offered plus some that will fail -- typically because the catalog's
//     overlay for the pair needs an accelerator this cluster does not have.
//
// A corrupt snapshot.yaml deliberately degrades to provisional rather than
// erroring: this endpoint asks the console's only two questions, so it must
// stay available, and steps.Recommend refuses the same bytes loudly if the
// user proceeds. The parse failure is logged by aicrclient.AvailableOptions.
//
// A client that fetches at mount will normally get provisional=true, because
// the first request precedes Discover. It MUST re-fetch when the run enters
// StateAwaitingDecision -- the point at which the wizard actually needs the
// answer and the point at which snapshot.yaml exists -- and MUST NOT keep
// showing a provisional set once a verified one is available. Waiting for
// awaiting_decision is a client-side discipline, not something this handler
// can enforce: it answers whatever is true at request time.
//
// Results are memoized against the snapshot they were computed from (see
// aicrclient.OptionsCache), so the mandatory re-fetch costs a digest compare
// rather than a fresh catalog probe.
func (s *Server) handleOptions(w http.ResponseWriter, r *http.Request) {
	var raw []byte
	if run := s.engine.Current(); run != nil {
		raw = run.Artifacts["snapshot.yaml"]
	}

	opts, err := s.options.Get(r.Context(), s.aicr, raw)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, opts)
}
