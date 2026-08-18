package engine

import "encoding/json"

// Attribution is the small, self-consistent view the observer needs to label
// a cluster event against the deployment action currently installing. It is
// read once per watch event, so it must stay cheap: one lock acquisition, a
// small value copy, no artifact clone, no store I/O -- Engine.CurrentID is
// the shape to match; Engine.Current(), which deep-copies every artifact, is
// exactly what this path must avoid.
//
// It exists as its own value rather than as an accessor over e.current
// because e.current.Components is assigned only after a step RETURNS (see
// runStep's merge-back in engine.go), so an accessor over e.current would
// read stale state for the entire duration of Apply -- precisely the window
// this feature exists to narrate. A spike observed this directly: the
// progress line printed exactly once, at the very end, with all 14
// deployment actions already "installed". The first draft of this design
// recorded that as a finding and proposed the stale accessor anyway; this
// snapshot is what replaces it.
type Attribution struct {
	// RunID identifies the run this snapshot describes, or empty before any
	// run has started. Composed from e.current.ID at read time (see
	// Engine.Attribution), not mirrored in the engine's stored snapshot,
	// because e.current.ID never changes after Start -- there is nothing
	// about it that needs its own transition tracking.
	RunID string

	// NOTE (Ruling 2): Namespaces deliberately does NOT live here. They come
	// from parsing recipe.json into steps.RecipeSummary, and internal/steps
	// imports internal/engine -- so the engine deriving them here would be an
	// import cycle. main composes instead: it already holds both the engine
	// and 2b-ii's cached namespace parsing (newRunScopeFn), and will hand the
	// observer ONE accessor returning the combined view (Task 3). Do not
	// "simplify" the two sources together here; that is the cycle.
	Phase Phase

	// ActiveAction is the deployment action deploy.sh is currently
	// installing, or empty between actions and outside Apply. It is a
	// TEMPORAL cursor, not a claim of ownership: deploy.sh.tmpl's own note
	// (~line 488) warns that cluster convergence continues asynchronously
	// after the script exits -- Nodewright alone can take 10-20 minutes on
	// fresh nodes. The honest label is "cluster activity observed while
	// ActiveAction installs"; this field never claims the activity belongs
	// to it.
	ActiveAction string
	ActiveIndex  int
	ActiveTotal  int

	// Generation advances on every ActiveAction transition, so a consumer can
	// tell a stale read from a current one without comparing every field.
	Generation uint64
}

// componentMarker decodes the subset of applier.ComponentData
// (internal/applier/parse.go) this package needs to recognize a component's
// header marker. It is its own type rather than an import of applier's,
// because internal/applier exists to translate deploy.sh's output for
// internal/steps -- reaching for its type here would make the engine depend
// on one step's parsing details instead of the other way around. The wire
// shape is pinned by TestDeployTemplateUnchanged (internal/applier), so
// decoding it a second time here is safe against drift.
type componentMarker struct {
	Name   string `json:"name"`
	Index  int    `json:"index,omitempty"`
	Total  int    `json:"total,omitempty"`
	Status string `json:"status"`
}

// componentStatusStarted mirrors applier.StatusStarted: the only status that
// carries Index/Total, i.e. the header marker that begins a new deployment
// action. Duplicated as a constant for the same reason componentMarker
// duplicates ComponentData's shape -- see its doc comment.
const componentStatusStarted = "started"

// Attribution returns a consistent snapshot of the current run's attribution
// state. Cheap by construction: one lock acquisition over a handful of
// scalar fields, no artifact clone, no store I/O.
func (e *Engine) Attribution() Attribution {
	e.mu.Lock()
	defer e.mu.Unlock()
	a := e.attribution
	if e.current != nil {
		a.RunID = e.current.ID
		a.Phase = e.current.Phase
	}
	return a
}

// setActiveAction records the deployment action deploy.sh has just started
// installing. Called from runStep's emit closure (engine.go), immediately
// AFTER the header event reaches the bus -- never before it. That ordering
// is the contract, not an incidental consequence of statement order: update
// this any earlier and a concurrent reader of Attribution() could label a
// cluster event with an action whose header has not reached the bus yet.
func (e *Engine) setActiveAction(name string, index, total int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attribution.ActiveAction = name
	e.attribution.ActiveIndex = index
	e.attribution.ActiveTotal = total
	e.attribution.Generation++
}

// clearActiveAction removes the active action, leaving RunID and Phase
// untouched (they are composed from e.current, not stored here). Called when
// the run leaves Apply -- a step returning, a step failing -- and
// defensively once more at every terminal state (finish), so the snapshot
// never keeps claiming cluster activity is attributable once nothing is
// actively installing.
func (e *Engine) clearActiveAction() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attribution.ActiveAction = ""
	e.attribution.ActiveIndex = 0
	e.attribution.ActiveTotal = 0
	e.attribution.Generation++
}

// applyComponentMarker inspects a KindComponent event's payload and advances
// the active-action snapshot when it is a header marker. Non-header markers
// (installed, failed, retrying) name the same component the header already
// set and carry no new Index/Total, so they deliberately leave ActiveAction
// untouched -- deploy.sh installs actions strictly one at a time, and the
// next header is what actually advances the cursor. A malformed or
// non-header payload is silently ignored: this path runs on every Apply
// marker, and a parse miss here must not fail the run over an attribution
// nicety.
func applyComponentMarker(e *Engine, data json.RawMessage) {
	var m componentMarker
	if err := json.Unmarshal(data, &m); err != nil || m.Name == "" || m.Status != componentStatusStarted {
		return
	}
	e.setActiveAction(m.Name, m.Index, m.Total)
}
