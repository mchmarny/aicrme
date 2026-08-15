package applier

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/bus"
)

// tailLines bounds the raw-output ring attached to a failure event. Large
// enough to hold deploy.sh's own hook-Job and kai-scheduler diagnostic
// dumps (each up to ~50 lines of describe output) plus the helm error that
// preceded them; small enough that the failure event stays a readable
// payload rather than a log file.
const tailLines = 200

// Options configures one Apply.
type Options struct {
	// BundleDir is the generated bundle root -- the directory holding
	// deploy.sh, written by steps.Bundle.
	BundleDir string
	// Retries is deploy.sh's per-component retry budget. Its own backoff is
	// quadratic (5s, 20s, 45s, 80s, 120s cap), and each attempt surfaces as
	// a warn event so the wait is visible rather than silent.
	Retries int
	// DryRun renders every component through helm without installing
	// anything. Used by the CI end-to-end test, which exercises the real
	// script and the real helm binary on a cluster with no GPUs.
	DryRun bool
}

// Applier runs one bundle's deploy.sh.
type Applier struct {
	exec Exec
}

// New returns an Applier over the given process seam.
func New(e Exec) *Applier { return &Applier{exec: e} }

// Apply runs deploy.sh to completion, publishing one event per recognized
// marker and, on failure, a single KindError carrying the failing component
// and a bounded tail of raw output.
//
// deploy.sh runs WITHOUT --best-effort. The first component to exhaust its
// retries ends the run. Continuing past a failure would finish on a cluster
// that looks installed and is not, and would convert a clear applier
// failure into a confusing Validate or Prove failure one phase later. The
// recovery path is engine.Retry re-running this whole step, which is safe
// because every install.sh is `helm upgrade --install` and deploy.sh's
// preflight and hook-Job cleanup run again.
func (a *Applier) Apply(ctx context.Context, opts Options, emit func(bus.Event)) error {
	tail := newRing(tailLines)

	// Written and read only from inside the lineWriter callback, which
	// lineWriter invokes under its own mutex, and read again after
	// exec.Run returns -- Cmd.Wait (called by cmd.Run) joins all of its
	// internal copy goroutines before returning, so that post-Run read
	// happens-after every callback write regardless of how many goroutines
	// copied stdout/stderr.
	var lastComponent string

	out := newLineWriter(func(line string) {
		tail.add(line)
		// Every line reaches the pod log even though most never reach the
		// bus, so `kubectl logs` retains the complete transcript.
		slog.Debug("deploy.sh", "line", line)

		ev, ok := parseLine(line)
		if !ok {
			return
		}
		if ev.Component != "" {
			lastComponent = ev.Component
		}
		emit(ev)
	})

	spec := Spec{
		Dir:  opts.BundleDir,
		Argv: []string{"bash", "deploy.sh", "--retries", strconv.Itoa(opts.Retries)},
		Env:  a.env(opts),
	}

	err := a.exec.Run(ctx, spec, out)
	out.Flush()
	if err == nil {
		return nil
	}

	// FailureData holds only strings, so Marshal cannot fail.
	data, _ := json.Marshal(FailureData{
		Component: lastComponent,
		ExitError: err.Error(),
		Tail:      tail.lines(),
	})
	emit(bus.Event{
		Kind:      bus.KindError,
		Level:     bus.LevelError,
		Component: lastComponent,
		Message:   "deploy.sh failed: " + err.Error(),
		Data:      data,
	})
	return aicrerrors.Wrap(aicrerrors.ErrCodeUnavailable, "bundle apply failed", err)
}

// env builds the variables deploy.sh and each install.sh read. The three
// *_FLAG variables are exported unconditionally, empty by default: the
// script's own `${DRY_RUN_FLAG:-}` expansions tolerate unset, but setting
// them explicitly means an operator's stray environment cannot leak a
// --dry-run (or a --debug) into a real customer install.
func (a *Applier) env(opts Options) []string {
	dryRun := ""
	if opts.DryRun {
		dryRun = "--dry-run"
	}
	return []string{
		"NO_COLOR=1",
		"DRY_RUN_FLAG=" + dryRun,
		"KUBECONFIG_FLAG=",
		"HELM_DEBUG_FLAG=",
	}
}
