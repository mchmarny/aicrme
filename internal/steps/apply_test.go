package steps_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/mchmarny/aicrme/internal/applier"
	"github.com/mchmarny/aicrme/internal/bus"
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

// The confirm gate: the console installs sixteen charts with cluster-admin,
// so the run must park for an explicit click first.
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
