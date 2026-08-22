package steps

import (
	"context"
	"encoding/json"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/applier"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
)

// ApplyConfig configures the deploy.sh invocation.
type ApplyConfig struct {
	Retries int
	DryRun  bool
}

type apply struct {
	applier *applier.Applier
	cfg     ApplyConfig
}

// NewApply returns the Apply step.
//
// It requires one decision, "apply", which is the console's confirm gate.
// The console installs the whole recipe with cluster-admin, so it must not
// begin mutating a cluster without an explicit click. This needs no new
// engine machinery: engine.awaitDecisions already parks the run in
// StateAwaitingDecision before a step whose Requires() is unsatisfied, and
// POST /api/runs/{id}/decide already supplies it. It does not break the
// spec's "exactly two decisions" promise -- intent and platform are
// choices, this is a confirmation -- and it is where the Review-and-verify
// screen lands when Phase 5 builds it.
func NewApply(a *applier.Applier, cfg ApplyConfig) engine.Step {
	return &apply{applier: a, cfg: cfg}
}

func (a *apply) Phase() engine.Phase { return engine.PhaseApply }
func (a *apply) Requires() []string  { return []string{"apply"} }

func (a *apply) Run(ctx context.Context, run *engine.Run, emit engine.Emit) error {
	dir := string(run.Artifacts["bundle.path"])
	if dir == "" {
		return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			"bundle.path artifact is missing -- Bundle must run before Apply")
	}

	emit(bus.Event{Kind: bus.KindLog, Message: "applying the bundle"})

	return a.applier.Apply(ctx, applier.Options{
		BundleDir: dir,
		Retries:   a.cfg.Retries,
		DryRun:    a.cfg.DryRun,
	}, trackComponents(run, emit))
}

// trackComponents wraps emit so every KindComponent event -- deploy.sh's
// header, installed, failed, and retrying markers, parsed by
// internal/applier/parse.go -- also upserts run.Components before the event
// reaches the bus. This is what makes run.Components a bounded projection
// rather than the raw event stream: the same component reported twice
// (started, then installed) updates its one row rather than appending a
// second (design doc, "Per-component state is persisted; the event stream
// is not"). engine.Engine.runStep's merge-back is what carries this step's
// copy of Components back into the current run, the same way it already
// does for Artifacts and Decisions.
func trackComponents(run *engine.Run, emit engine.Emit) engine.Emit {
	return func(ev bus.Event) {
		if ev.Kind == bus.KindComponent {
			var data applier.ComponentData
			if err := json.Unmarshal(ev.Data, &data); err == nil && data.Name != "" {
				upsertComponent(run, data)
			}
		}
		emit(ev)
	}
}

// upsertComponent keeps run.Components at one row per component, keyed by
// name. Index, Total and Namespace are only present on a component's header
// ("started") marker -- reInstalled/reFailed/reRetry in
// internal/applier/parse.go carry none of them -- so a later status update
// carries the header's values forward rather than zeroing them, matching
// web/src/pipeline.ts's deriveComponents, which does the same for the live
// (non-persisted) rendering. Namespace matters most here: it is the half of
// a helm release's identity Reset cannot reconstruct from anywhere else
// once the bundle's emptyDir is gone.
func upsertComponent(run *engine.Run, data applier.ComponentData) {
	for i := range run.Components {
		if run.Components[i].Name != data.Name {
			continue
		}
		row := &run.Components[i]
		row.Status = data.Status
		if data.Index != 0 {
			row.Index = data.Index
		}
		if data.Total != 0 {
			row.Total = data.Total
		}
		if data.Namespace != "" {
			row.Namespace = data.Namespace
		}
		return
	}
	run.Components = append(run.Components, engine.ComponentState{
		Name:      data.Name,
		Index:     data.Index,
		Total:     data.Total,
		Namespace: data.Namespace,
		Status:    data.Status,
	})
}
