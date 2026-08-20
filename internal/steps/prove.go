package steps

import (
	"context"
	"errors"
	"fmt"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/prove"
)

// ProveConfig configures the Prove step.
type ProveConfig struct {
	// GangTimeout bounds how long Run waits for every gang member to be
	// scheduled before declaring the run a failure and cleaning up after
	// itself. Zero defaults to defaultGangTimeout.
	GangTimeout time.Duration
}

// defaultGangTimeout is the bound applied when ProveConfig.GangTimeout is
// left zero. Provisional (the design's own open question, resolved here as
// a ruling) -- to be revisited against a real `make demo` once gang
// placement latency on a live cluster is measured rather than guessed.
const defaultGangTimeout = 3 * time.Minute

// gangSize is the number of pods that must be placed together before the
// step considers the gang placed. Fixed at 2, matching workload.yaml's
// spec.completions/spec.parallelism: Prove ships exactly one reference
// workload with one fixed shape, not a configurable one, so this mirrors
// the same hardcoded 2 the manifest and internal/prove's own tests already
// carry rather than inventing a second, disconnected source of truth for a
// shape that has only ever had one value.
const gangSize = 2

// placementPollInterval bounds how quickly Run notices a pod has been
// scheduled, the same role prove.Client's own waitAbsentPollInterval plays
// for deletion.
const placementPollInterval = 20 * time.Millisecond

type proveStep struct {
	client *prove.Client
	cfg    ProveConfig
}

// NewProve returns the Prove step: applies the reference workload, waits for
// its gang to place, and leaves it running on success. It implements
// engine.ActiveStep, so a run that reaches this step and returns without
// error ends at StateActive rather than StateDone.
func NewProve(c *prove.Client, cfg ProveConfig) engine.Step {
	if cfg.GangTimeout == 0 {
		cfg.GangTimeout = defaultGangTimeout
	}
	return &proveStep{client: c, cfg: cfg}
}

func (p *proveStep) Phase() engine.Phase { return engine.PhaseProve }

// Requires adds no operator question -- Prove runs once every earlier gate
// has already been satisfied.
func (p *proveStep) Requires() []string { return nil }

// LeavesWorkloadRunning implements engine.ActiveStep: Run does not wait for
// the workload to complete, only for its gang to place, and returns with it
// still running.
func (p *proveStep) LeavesWorkloadRunning() bool { return true }

func (p *proveStep) Run(ctx context.Context, run *engine.Run, emit engine.Emit) error {
	// kube can be nil outside a pod (main.go's dev-mode fallback); every
	// other Client method dereferences it immediately and panics rather than
	// degrading, so this failed run -- not the whole process -- is the
	// outcome of reaching Prove without a live cluster.
	if !p.client.Ready() {
		return aicrerrors.New(aicrerrors.ErrCodeUnavailable,
			"prove: a live cluster client is required to run the reference workload")
	}

	if err := p.client.EnsureNamespace(ctx); err != nil {
		return err
	}

	emit(bus.Event{Kind: bus.KindLog, Message: "applying the reference workload"})
	if err := p.client.Apply(ctx, run.ID); err != nil {
		// A Create can fail client-side (timeout, connection reset, a proxy
		// 502) after the API server has already accepted it, leaving the
		// Job in the cluster with nothing telling this run it exists. Spec
		// section 8 row 1: a partial apply must still clean up. cleanup is
		// safe to call unconditionally here -- Client.Delete treats NotFound
		// as success, so a genuinely never-created Job costs one 404.
		return p.cleanup(ctx, run.ID, err)
	}

	if err := p.awaitGang(ctx, run.ID, emit); err != nil {
		return p.cleanup(ctx, run.ID, err)
	}

	// Recorded only once the gang has actually placed: a run that fails and
	// cleans up after itself has nothing left running to name. Labels
	// remain the source of truth (prove.Client's own doc comment) -- this
	// is a display convenience for the console and Task 8's fallback.
	run.Workload = engine.Workload{
		Namespace: prove.Namespace,
		Kind:      "Job",
		Name:      prove.WorkloadName(run.ID),
	}
	emit(bus.Event{Kind: bus.KindLog, Message: "gang placed; reference workload running"})
	return nil
}

// awaitGang polls for gang placement until every member has been scheduled
// or cfg.GangTimeout elapses, emitting one event per pod newly placed.
//
// Placement is read off Pod.Spec.NodeName (prove.Client.PlacedNodes) -- the
// field the scheduler itself writes the instant it binds a pod. That is
// what makes it trustworthy against a fake clientset in tests: NodeName
// means "scheduled" whether or not any controller is actually running to
// produce it, so a test can simulate a placement decision by setting the
// same field a live cluster's scheduler would set, rather than this step
// relying on a synthetic signal (a Pod phase, a custom annotation) that only
// happens to exist in a fake.
func (p *proveStep) awaitGang(ctx context.Context, runID string, emit engine.Emit) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, p.cfg.GangTimeout)
	defer cancel()

	ticker := time.NewTicker(placementPollInterval)
	defer ticker.Stop()

	placed := make(map[string]string, gangSize)
	for {
		nodes, err := p.client.PlacedNodes(timeoutCtx, runID)
		if err != nil {
			return fmt.Errorf("prove: checking gang placement for run %s: %w", runID, err)
		}
		for pod, node := range nodes {
			if _, ok := placed[pod]; ok {
				continue
			}
			placed[pod] = node
			emit(bus.Event{Kind: bus.KindCluster,
				Message: fmt.Sprintf("gang member %s placed on node %s", pod, node)})
		}
		if len(placed) >= gangSize {
			return nil
		}

		select {
		case <-timeoutCtx.Done():
			return aicrerrors.New(aicrerrors.ErrCodeTimeout,
				fmt.Sprintf("gang for run %s did not place within %s (%d/%d members placed)",
					runID, p.cfg.GangTimeout, len(placed), gangSize))
		case <-ticker.C:
		}
	}
}

// cleanup deletes the workload this step just created and failed to place,
// then waits for its absence before returning -- a pending gang can still
// place after a timeout has already been declared, so Delete's foreground
// propagation merely starting the cascade is not enough on its own
// (prove.Client's own doc comment on Delete/WaitAbsent). If either call
// fails, the returned error names the cleanup failure distinctly from
// cause (for the operator, in its Error() text) AND wraps
// engine.ErrUnconfirmedCleanup (for the engine, via errors.Is) -- fix round
// 1's C3: the engine used to key Ruling 12's guard off a substring of this
// message's text, and a one-character reword of that text (verified by the
// reviewer) left the guard silently dead with the whole suite green. The
// sentinel is what runStep now checks instead, on this error's actual typed
// value, before it is ever reduced to a string. errors.Join, not a bare
// Cause reassignment: aicrerrors.Wrap already uses its cause argument for
// the operator-facing chain (Unwrap), and the sentinel has to be reachable
// from the SAME chain without displacing it -- Join's multi-error Unwrap
// satisfies errors.Is against either. cause itself is returned unchanged,
// carrying no sentinel, once cleanup succeeds: a confirmed-clean cleanup is
// exactly the case Ruling 12 must NOT block on.
func (p *proveStep) cleanup(ctx context.Context, runID string, cause error) error {
	if err := p.client.Delete(ctx, runID); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeUnavailable,
			fmt.Sprintf("run %s failed (%v); cleanup failed deleting the workload", runID, cause),
			errors.Join(engine.ErrUnconfirmedCleanup, err))
	}
	if err := p.client.WaitAbsent(ctx, runID, p.cfg.GangTimeout); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeUnavailable,
			fmt.Sprintf("run %s failed (%v); cleanup failed waiting for the workload to be gone", runID, cause),
			errors.Join(engine.ErrUnconfirmedCleanup, err))
	}
	return cause
}
