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

// maxPlacementPollInterval caps how long awaitGang waits between placement
// reads, and minPlacementPollInterval floors it so a tiny test budget can
// never produce a zero-duration ticker.
const (
	maxPlacementPollInterval = 2 * time.Second
	minPlacementPollInterval = time.Millisecond
)

// placementPollInterval returns how often awaitGang re-reads placement for a
// given gang budget: a tenth of it, clamped.
//
// This was a flat 20ms -- mirroring prove.Client's own waitAbsentPollInterval,
// which is correct for a wait measured in milliseconds and abusive for one
// measured in minutes. Against the three-minute default it issues roughly
// 9000 List calls at ~50/second, and client-go's own rate limiter (5 QPS by
// default) starts refusing them long before the deadline. That is not a
// theoretical cost: on a real KWOK cluster the run failed with "client rate
// limiter Wait returned an error: rate: Wait(n=1) would exceed context
// deadline" as its entire recorded error, hiding the fact that nothing had
// been placed at all. No fake-clientset test can see it -- the fake has no
// rate limiter.
//
// A tenth of the budget keeps a 50ms test responsive and a 3-minute wait
// cheap (90 calls, ~0.5 QPS), without adding a knob a caller would have to
// know to set.
func placementPollInterval(budget time.Duration) time.Duration {
	d := budget / 10
	if d > maxPlacementPollInterval {
		return maxPlacementPollInterval
	}
	if d < minPlacementPollInterval {
		return minPlacementPollInterval
	}
	return d
}

type proveStep struct {
	client *prove.Client
	cfg    ProveConfig
}

// NewProve returns the Prove step: applies the reference workload, waits for
// its gang to place, and leaves it running on success. It implements
// engine.ActiveStep, so a run that reaches this step and returns without
// error ends at StateActive rather than StateDone.
//
// WHAT THIS STEP DOES NOT PROVE, on the only substrate it has today
// Three gaps, measured on the KWOK demo cluster rather than reasoned about,
// and repeated in DEMO.md and docs/phase-2-handoff.md because they bound
// what the console may claim on a simulated cluster:
//
//   - The workload body never executes. KWOK marks a gang pod Succeeded in
//     the same second it binds it, without starting the container, so
//     workload.yaml's command is never run and its image is never pulled.
//     "Gang placed" is the entire claim; nothing computed anything.
//   - The gang never holds its GPUs simultaneously. Each member's resources
//     are released the instant KWOK completes it, which is why both members
//     routinely land on the SAME simulated node. Co-location here is a
//     property of the substrate, not a scheduling fault.
//   - DRA is not exercised at all. The workload requests scalar
//     nvidia.com/gpu and the simulated nodes publish no ResourceSlices, so
//     the DRA driver the recipe installs is never asked to bind a device.
//
// All three close only on real hardware (Phase 4). test/e2e/prove.sh asserts
// what IS provable here -- that a GPU-aware scheduler, itself running on a
// node that really executes, admitted a gang and bound every member of it --
// and deliberately asserts nothing beyond it.
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

	ticker := time.NewTicker(placementPollInterval(p.cfg.GangTimeout))
	defer ticker.Stop()

	placed := make(map[string]string, gangSize)
	for {
		nodes, err := p.client.PlacedNodes(timeoutCtx, runID)
		if err != nil {
			// A read that fails because the budget ran out mid-call is this
			// wait ending, not the cluster failing, and the operator needs
			// the placement count rather than the plumbing that noticed. The
			// real KWOK run this fix came from reported client-go's rate
			// limiter refusing a call as the run's whole error, which named
			// neither the timeout nor the 0/2 placement that was the actual
			// diagnosis.
			if timeoutCtx.Err() != nil {
				return p.gangTimeoutErr(runID, len(placed))
			}
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
			return p.gangTimeoutErr(runID, len(placed))
		case <-ticker.C:
		}
	}
}

// gangTimeoutErr is what an expired placement budget reports, from either of
// the two places the expiry can be noticed (a read that straddles the
// deadline, or the wait itself). One function, so both say the same thing:
// how long was allowed and how much of the gang made it, which is what an
// operator needs to tell "the cluster is full" apart from "nothing is
// scheduling at all".
func (p *proveStep) gangTimeoutErr(runID string, placed int) error {
	return aicrerrors.New(aicrerrors.ErrCodeTimeout,
		fmt.Sprintf("gang for run %s did not place within %s (%d/%d members placed)",
			runID, p.cfg.GangTimeout, placed, gangSize))
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
// satisfies errors.Is against either.
//
// On success, cause is wrapped with engine.ErrCleanupConfirmed instead of
// returned bare -- fix round 2's N2. runStep's guard is sticky (only
// ErrUnconfirmedCleanup and ErrCleanupConfirmed move it; anything else
// leaves a prior determination untouched), so a cleanup that genuinely ran
// and confirmed the workload absent has to say so positively, or a run
// whose cleanup was never reached at all (client.Ready() false, an
// EnsureNamespace error, above) would be indistinguishable from one that
// just cleanly confirmed absence -- which is exactly the ambiguity that let
// a retry silently clear Ruling 12's guard over a still-live orphan.
// confirmedCleanupError, not errors.Join: Ruling 13 requires this path's
// Error() text to read identically to an ordinary failure (an operator, or
// a future console alert, must not see "cleanup" mentioned over a cleanup
// that succeeded), and errors.Join would prepend ErrCleanupConfirmed's own
// message ahead of cause's.
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
	return &confirmedCleanupError{cause: cause}
}

// confirmedCleanupError wraps cause so errors.Is can detect a confirmed-
// clean cleanup (engine.ErrCleanupConfirmed) without changing the
// operator-facing text at all. Error() delegates entirely to cause; Is
// recognizes only engine.ErrCleanupConfirmed itself, and Unwrap exposes
// cause so errors.Is/As chains through it exactly as if this wrapper were
// not there for any other target.
type confirmedCleanupError struct {
	cause error
}

func (e *confirmedCleanupError) Error() string { return e.cause.Error() }
func (e *confirmedCleanupError) Unwrap() error { return e.cause }
func (e *confirmedCleanupError) Is(target error) bool {
	return target == engine.ErrCleanupConfirmed
}
