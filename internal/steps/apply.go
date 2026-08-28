package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/applier"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"k8s.io/client-go/kubernetes"
)

// ApplyConfig configures the deploy.sh invocation.
type ApplyConfig struct {
	Retries int
	DryRun  bool
	// Kubeconfig is the frozen session kubeconfig every tool deploy.sh runs
	// must read -- see applier.Options.Kubeconfig.
	Kubeconfig string
	// Helm and Kube are the two seams the pre-Apply ownership snapshot
	// reads (see snapshotOwnership). Both are nil outside a cluster, and
	// both being nil is handled rather than guarded against: the snapshot
	// records a per-namespace failure, Reset proves nothing, and Reset
	// removes nothing. Config fields rather than NewApply parameters to
	// match this package's existing shape, where every step takes its
	// client plus one config struct.
	Helm HelmLister
	Kube kubernetes.Interface
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

	// Before anything is installed, and only here: `helm upgrade --install`
	// and `--create-namespace` both destroy the created-vs-adopted
	// distinction the instant they run, so this is the last moment the
	// answer exists. Never fails the step -- see snapshotOwnership.
	//
	// Taken ONCE per run, and a retry keeps the original. Apply re-runs on
	// Retry, and by then this run's own partial install is on the cluster --
	// so re-snapshotting would record a run's own releases as things that
	// pre-existed it, handing them to teardown as somebody else's and
	// leaving them behind at Reset. The first answer is the only true one.
	first := len(run.Ownership.Namespaces) == 0
	if first {
		run.Ownership = snapshotOwnership(ctx, a.cfg.Helm, a.cfg.Kube, recipeNamespaces(run))
	}

	// Refuse a second install over the first, and only on the first attempt:
	// a Retry is this run finishing its own work, not a new run climbing on
	// top of another's. See alreadyInstalled for what the second install
	// does to kai-scheduler, and why the purge cannot clean up after it.
	if first {
		if clash := alreadyInstalled(run.Ownership, recipeReleaseNames(run)); len(clash) > 0 {
			return aicrerrors.New(aicrerrors.ErrCodeConflict, installedAlreadyMessage(clash))
		}
	}

	if unprovable := unsnapshotted(run.Ownership); len(unprovable) > 0 {
		// Said out loud, during Apply, because the alternative is an
		// operator who finds out at Reset time -- after the demo, with a
		// cluster to hand back -- that some of what this run installed can
		// never be proven to be its own and will be left behind.
		emit(bus.Event{
			Kind: bus.KindLog, Level: bus.LevelWarn,
			Message: "could not record what already existed in " + strings.Join(unprovable, ", ") +
				"; releases installed there will be left in place by a later Reset",
		})
	}

	emit(bus.Event{Kind: bus.KindLog, Message: "applying the bundle"})

	err := a.applier.Apply(ctx, applier.Options{
		BundleDir:  dir,
		Retries:    a.cfg.Retries,
		DryRun:     a.cfg.DryRun,
		Kubeconfig: a.cfg.Kubeconfig,
	}, trackComponents(run, emit))

	return err
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

// installedAlreadyMessage names what is in the way and both ways out.
//
// It names releases rather than counting them because the operator has to act
// on this, and it offers the manual command as well as Reset: a run that was
// discarded no longer exists to be Reset, and its releases are then nobody's
// -- refusing without saying how to clear them would strand the operator on a
// cluster the console will not install into.
func installedAlreadyMessage(clash []engine.ReleaseRef) string {
	shown := clash
	if len(shown) > 3 {
		shown = shown[:3]
	}
	names := make([]string, 0, len(shown))
	for _, r := range shown {
		names = append(names, r.Name+" in "+r.Namespace)
	}
	list := strings.Join(names, ", ")
	if len(clash) > len(shown) {
		list = fmt.Sprintf("%s, and %d more", list, len(clash)-len(shown))
	}
	return fmt.Sprintf(
		"%d of this recipe's releases are already installed on this cluster (%s). "+
			"Installing over them would appear to succeed and then not work: kai-scheduler's "+
			"SchedulingShard survives a reinstall by design, the scheduler Deployment it owns is "+
			"never recreated, and the gang fails to place twenty minutes from now. "+
			"Reset the run that installed them first. If no run owns them any more, remove them "+
			"with `helm uninstall`.",
		len(clash), list)
}
