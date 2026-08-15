package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
)

// BundleConfig configures where generated bundles land.
type BundleConfig struct {
	// WorkDir is the writable scratch root -- an emptyDir in the chart. Each
	// run gets <WorkDir>/runs/<runID>/bundle. Nothing here needs to outlive
	// the pod: the bundle is regenerated from the pinned, embedded catalog.
	WorkDir string
}

type bundle struct {
	client aicrclient.API
	cfg    BundleConfig
}

// NewBundle returns the Bundle step. It gates on no decisions: Recommend has
// already collected the only two the console asks for, and Bundle runs
// automatically so the confirm gate on Apply has a real bundle to show.
//
// Bundle RE-RESOLVES the recipe rather than receiving Recommend's
// *aicr.RecipeResult. aicr.Client.MakeBundle requires a Client-owned result
// (it calls assertOwns and reads unexported state), which cannot travel
// through engine.Run.Artifacts' []byte values. The alternative -- a holder
// shared between the two steps -- would be in-memory state the ConfigMap
// store in Phase 2b then has to lose across a pod restart, which is the
// exact case that store exists to fix. Re-resolving survives a restart for
// free: every input is persisted (the raw snapshot bytes, the two
// decisions) and the catalog is embedded in the pinned aicr module, so the
// resolve is deterministic. assertMatchesApproved proves it.
func NewBundle(c aicrclient.API, cfg BundleConfig) engine.Step {
	return &bundle{client: c, cfg: cfg}
}

func (b *bundle) Phase() engine.Phase { return engine.PhaseBundle }
func (b *bundle) Requires() []string  { return nil }

func (b *bundle) Run(ctx context.Context, run *engine.Run, emit engine.Emit) error {
	approved := run.Artifacts["recipe.json"]
	if len(approved) == 0 {
		return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			"recipe.json artifact is missing -- Recommend must run before Bundle")
	}

	snap, err := decodeSnapshot(run.Artifacts["snapshot.yaml"])
	if err != nil {
		return err
	}

	criteria, err := buildCriteria(b.client, snap, run.Decisions["intent"], run.Decisions["platform"])
	if err != nil {
		return err
	}

	result, err := b.client.ResolveRecipeFromSnapshot(ctx, criteria, snap)
	if err != nil {
		return aicrerrors.PropagateOrWrap(err, aicrerrors.ErrCodeInvalidRequest, "recipe re-resolution failed")
	}
	if result == nil {
		return aicrerrors.New(aicrerrors.ErrCodeInternal, "recipe re-resolution returned no result")
	}
	if err = assertMatchesApproved(result, approved); err != nil {
		return err
	}

	dir := filepath.Join(b.cfg.WorkDir, "runs", run.ID, "bundle")
	emit(bus.Event{Kind: bus.KindLog, Message: fmt.Sprintf("generating bundle for %d components", len(result.Components))})

	art, err := b.client.MakeBundle(ctx, result, aicr.BundleOptions{OutputDir: dir})
	if err != nil {
		return aicrerrors.PropagateOrWrap(err, aicrerrors.ErrCodeInternal, "bundle generation failed")
	}
	if art == nil {
		return aicrerrors.New(aicrerrors.ErrCodeInternal, "bundle generation returned no artifact")
	}
	// HasErrors covers per-bundler failures that Make itself reports as
	// non-fatal. Applying a partially generated bundle would fail later and
	// less legibly, so fail here instead.
	if art.HasErrors() {
		return aicrerrors.New(aicrerrors.ErrCodeInternal,
			fmt.Sprintf("bundle generation reported errors: %v", art.Errors))
	}

	run.Artifacts["bundle.path"] = []byte(dir)

	emit(bus.Event{
		Kind:    bus.KindLog,
		Message: fmt.Sprintf("bundle ready: %d files, %d bytes", art.TotalFiles, art.TotalSize),
	})
	return nil
}

// assertMatchesApproved proves the re-resolved recipe is the one the user
// approved on the Recommend screen. Bundle re-resolves rather than carrying
// a handle forward (see NewBundle), so this is the guard that makes that
// choice safe rather than merely convenient: without it, a catalog or
// derivation change between the two resolves would silently install a
// component set the user never saw.
func assertMatchesApproved(result *aicr.RecipeResult, approved []byte) error {
	var summary RecipeSummary
	if err := json.Unmarshal(approved, &summary); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "stored recipe.json is unparseable", err)
	}
	// Bounded by len(summary.Components), the slice the loop below actually
	// indexes -- not the self-reported summary.ComponentCount. recipe.json is
	// Phase 2b's persisted, kubectl-editable ConfigMap state: a
	// ComponentCount that disagrees with the real length of Components would
	// pass this check and then panic on the out-of-range index below, and a
	// panic here runs on engine.Engine's step goroutine, which has no
	// recover -- it takes the whole console process down, not just the run.
	if len(result.Components) != len(summary.Components) {
		return aicrerrors.New(aicrerrors.ErrCodeInternal, fmt.Sprintf(
			"re-resolved recipe drifted from the approved one: component count %d, approved %d",
			len(result.Components), len(summary.Components)))
	}
	for i, c := range result.Components {
		a := summary.Components[i]
		if c.Name != a.Name || c.Version != a.Version {
			return aicrerrors.New(aicrerrors.ErrCodeInternal, fmt.Sprintf(
				"re-resolved recipe drifted from the approved one at position %d: %s %s, approved %s %s",
				i, c.Name, c.Version, a.Name, a.Version))
		}
	}
	return nil
}
