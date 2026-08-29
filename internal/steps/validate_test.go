package steps_test

import (
	"context"
	"testing"

	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/steps"
)

// KWOK's fake nodes defeat the validator: it schedules with a blanket
// toleration, lands on a fake node, and KWOK fakes exit 0 without starting
// the container -- so every check reports "passed" having run nothing.
// Measured 2026-08-18: 14/14 false passes with nothing installed.
//
// The step therefore must not call ValidateState at all on a simulated
// cluster, and must record a skip rather than a pass.
func TestValidateSkipsASimulatedCluster(t *testing.T) {
	fake := &aicrclient.Fake{}
	step := steps.NewValidate(fake, steps.ValidateConfig{WorkDir: t.TempDir()})

	run := newRun()
	run.Artifacts["capability.json"] = []byte(`{"totalGpus":0,"usableGpus":0,"analyzed":true}`)

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v, want nil -- Validate never fails the run", err)
	}
	if fake.ValidateCalls != 0 {
		t.Errorf("ValidateCalls = %d, want 0 -- validating a simulated cluster produces a false pass", fake.ValidateCalls)
	}
	if run.Validation.Skipped == "" {
		t.Error("Skipped is empty -- a skipped validation must be recorded as skipped, never as a pass")
	}
	if len(run.Validation.Phases) != 0 {
		t.Errorf("Phases = %+v, want none recorded for a skipped run", run.Validation.Phases)
	}
}
