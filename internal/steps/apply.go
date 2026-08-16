package steps

import (
	"context"

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
	}, emit)
}
