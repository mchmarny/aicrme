package steps_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/mchmarny/aicrme/internal/applier"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/steps"
)

type recordingExec struct {
	transcript string
	err        error
	lastSpec   applier.Spec
}

func (r *recordingExec) Run(_ context.Context, spec applier.Spec, out io.Writer) error {
	r.lastSpec = spec
	_, _ = io.WriteString(out, r.transcript)
	return r.err
}

// The confirm gate: the console's demo path installs 13 components across
// 14 deploy.sh steps with cluster-admin, so the run must park for an
// explicit click first.
func TestApplyGatesOnTheApplyDecision(t *testing.T) {
	step := steps.NewApply(applier.New(&recordingExec{}), steps.ApplyConfig{})
	got := step.Requires()
	if len(got) != 1 || got[0] != "apply" {
		t.Fatalf("Requires() = %v, want [apply]", got)
	}
}

func TestApplyRunsDeployScriptFromTheBundleDir(t *testing.T) {
	exec := &recordingExec{transcript: "✓ Pre-flight checks passed\n"}
	step := steps.NewApply(applier.New(exec), steps.ApplyConfig{Retries: 5})

	run := newRun()
	run.Decisions["apply"] = "yes"
	run.Artifacts["bundle.path"] = []byte("/var/lib/aicrme/runs/abc/bundle")

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if exec.lastSpec.Dir != "/var/lib/aicrme/runs/abc/bundle" {
		t.Errorf("Dir = %q, want the bundle path artifact", exec.lastSpec.Dir)
	}
}

func TestApplyRequiresBundleToHaveRun(t *testing.T) {
	step := steps.NewApply(applier.New(&recordingExec{}), steps.ApplyConfig{})

	run := newRun()
	run.Decisions["apply"] = "yes"
	// bundle.path deliberately absent.

	err := step.Run(context.Background(), run, func(bus.Event) {})
	if err == nil || !strings.Contains(err.Error(), "bundle.path") {
		t.Fatalf("Run() error = %v, want it to name the missing artifact", err)
	}
}

func TestApplyPropagatesDeployFailure(t *testing.T) {
	exec := &recordingExec{
		transcript: "└─ ✗ kai-scheduler FAILED (after 2 attempts)\n",
		err:        errors.New("exit status 1"),
	}
	step := steps.NewApply(applier.New(exec), steps.ApplyConfig{})

	run := newRun()
	run.Decisions["apply"] = "yes"
	run.Artifacts["bundle.path"] = []byte("/b")

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err == nil {
		t.Fatal("Run() error = nil, want the deploy failure propagated so the engine fails the run")
	}
}

// TestApplyMaintainsComponentState pins the component-state projection
// Task 6 adds: every parsed marker (internal/applier/parse.go) that carries
// a component name upserts a row in run.Components, and a component's
// status moves from its header ("started") to its terminal marker
// (installed or failed) on that same row. Index/Total are only present on
// the header marker, so they must survive onto the terminal row too --
// otherwise a recovered run would redraw the pipeline with the numerator
// and denominator missing.
func TestApplyMaintainsComponentState(t *testing.T) {
	transcript := strings.Join([]string{
		"┌─ [1/2] gpu-operator  →  gpu-operator",
		"└─ ✓ gpu-operator installed",
		"┌─ [2/2] kai-scheduler  →  kai-scheduler",
		"└─ ✗ kai-scheduler FAILED (after 2 attempts)",
	}, "\n") + "\n"
	exec := &recordingExec{transcript: transcript, err: errors.New("exit status 1")}
	step := steps.NewApply(applier.New(exec), steps.ApplyConfig{})

	run := newRun()
	run.Decisions["apply"] = "yes"
	run.Artifacts["bundle.path"] = []byte("/b")

	_ = step.Run(context.Background(), run, func(bus.Event) {})

	if len(run.Components) != 2 {
		t.Fatalf("Components = %+v, want exactly 2 rows, one per component", run.Components)
	}
	byName := map[string]engine.ComponentState{}
	for _, c := range run.Components {
		byName[c.Name] = c
	}
	if got := byName["gpu-operator"]; got.Status != applier.StatusInstalled || got.Index != 1 || got.Total != 2 {
		t.Errorf("gpu-operator row = %+v, want status=%q index=1 total=2", got, applier.StatusInstalled)
	}
	if got := byName["kai-scheduler"]; got.Status != applier.StatusFailed || got.Index != 2 || got.Total != 2 {
		t.Errorf("kai-scheduler row = %+v, want status=%q index=2 total=2", got, applier.StatusFailed)
	}
}

// TestApplyComponentStateIsAProjectionNotALog pins the property the design
// calls out explicitly: the same component reported twice updates its one
// row in place. This is what keeps the persisted record bounded against
// the ConfigMap cap -- an append-only implementation would still pass a
// test that only checked the final status, so this asserts the row COUNT
// stays at 1 as the deciding signal, which an append bug cannot satisfy.
func TestApplyComponentStateIsAProjectionNotALog(t *testing.T) {
	transcript := strings.Join([]string{
		"┌─ [1/1] gpu-operator  →  gpu-operator",
		"└─ ✓ gpu-operator installed",
	}, "\n") + "\n"
	exec := &recordingExec{transcript: transcript}
	step := steps.NewApply(applier.New(exec), steps.ApplyConfig{})

	run := newRun()
	run.Decisions["apply"] = "yes"
	run.Artifacts["bundle.path"] = []byte("/b")
	// Pre-seed a row for the same component, as if it had already reported
	// once. A projection overwrites this row; a log would append a second.
	run.Components = []engine.ComponentState{
		{Name: "gpu-operator", Index: 1, Total: 1, Status: applier.StatusStarted},
	}

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if len(run.Components) != 1 {
		t.Fatalf("Components = %+v, want exactly 1 row -- a projection overwrites in place, it does not append", run.Components)
	}
	if run.Components[0].Status != applier.StatusInstalled {
		t.Errorf("Components[0].Status = %q, want %q", run.Components[0].Status, applier.StatusInstalled)
	}
}
