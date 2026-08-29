package steps_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
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

// The happy path. It cannot assert what the five ValidateOptions actually
// DO -- aicr.ValidateOption is func(*validateConfig) over an unexported
// struct with no accessor, so nothing outside package aicr can inspect an
// option's contents, including whether it carries PhaseDeployment or
// PhasePerformance (the latter would saturate the GPUs Prove needs). The
// count is the strongest guard available from here: it catches an option
// silently dropped or duplicated. The five themselves --
// WithValidationPhases(PhaseDeployment), WithValidationKubeconfig,
// WithValidationRunID, WithValidationCleanup, WithValidationTimeout -- are
// pinned by reading validate.go's Run, not by this test.
func TestValidateRunsTheDeploymentPhaseAndRecordsIt(t *testing.T) {
	dir := t.TempDir()
	recipe := recipeFixture()
	fake := &aicrclient.Fake{
		Recipe: recipe,
		PhaseResults: []*aicr.PhaseResult{{
			Phase:    aicr.PhaseDeployment,
			Status:   "passed",
			Duration: 92 * time.Second,
			Summary:  aicr.ReportSummary{Tests: 14, Passed: 14},
			// A real aicr.Client always populates RawReport ("the marshaled
			// CTRF JSON", per its own doc); the fixture must too, or
			// writeReport has nothing to write and ReportPath stays empty.
			RawReport: []byte(`{"results":{"summary":{"tests":14,"passed":14}}}`),
		}},
	}
	step := steps.NewValidate(fake, steps.ValidateConfig{WorkDir: dir, Kubeconfig: "/tmp/kubeconfig"})

	run := newRunWithRealCluster(t, recipe)

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if fake.ValidateCalls != 1 {
		t.Fatalf("ValidateCalls = %d, want 1", fake.ValidateCalls)
	}
	// Cannot inspect what each option DOES (see the test's doc comment), but
	// a dropped or duplicated one changes this count -- e.g. losing
	// WithValidationCleanup(true) would leave validator Jobs behind, and
	// gaining an extra WithValidationPhases would add a phase Run() never
	// asked for.
	if want := 5; len(fake.LastValidateOpts) != want {
		t.Errorf("len(LastValidateOpts) = %d, want %d -- phases, kubeconfig, run ID, cleanup, timeout",
			len(fake.LastValidateOpts), want)
	}
	if run.Validation.Skipped != "" {
		t.Errorf("Skipped = %q, want empty on a real cluster", run.Validation.Skipped)
	}
	if len(run.Validation.Phases) != 1 {
		t.Fatalf("Phases = %+v, want one", run.Validation.Phases)
	}
	got := run.Validation.Phases[0]
	if got.Phase != "deployment" || got.Status != "passed" || got.Passed != 14 || got.Seconds != 92 {
		t.Errorf("PhaseSummary = %+v, want the flattened AICR result", got)
	}
	if run.Validation.ReportPath == "" {
		t.Error("ReportPath is empty -- the CTRF report was not written")
	}
	if _, err := os.Stat(run.Validation.ReportPath); err != nil {
		t.Errorf("report file missing: %v", err)
	}
}

// A validation that errors, and a validation that reports failures, are two
// different things and NEITHER may fail the run. The install succeeded; the
// report is a report. Prove still has to run, because placement is the claim
// the demo is built on.
func TestValidateNeverFailsTheRun(t *testing.T) {
	t.Run("the call errors", func(t *testing.T) {
		recipe := recipeFixture()
		fake := &aicrclient.Fake{Recipe: recipe, ValidateErr: errors.New("apiserver said no")}
		step := steps.NewValidate(fake, steps.ValidateConfig{WorkDir: t.TempDir()})
		run := newRunWithRealCluster(t, recipe)

		if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
		if run.Validation.Skipped == "" {
			t.Error("an errored validation must record why, or the screen shows nothing at all")
		}
	})

	t.Run("checks fail", func(t *testing.T) {
		recipe := recipeFixture()
		fake := &aicrclient.Fake{
			Recipe: recipe,
			PhaseResults: []*aicr.PhaseResult{{
				Phase: aicr.PhaseDeployment, Status: "failed",
				Summary: aicr.ReportSummary{Tests: 14, Passed: 11, Failed: 3},
			}},
		}
		step := steps.NewValidate(fake, steps.ValidateConfig{WorkDir: t.TempDir()})
		run := newRunWithRealCluster(t, recipe)

		if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
			t.Fatalf("Run() error = %v, want nil -- a failing check is a finding, not a broken run", err)
		}
		if run.Validation.Phases[0].Failed != 3 {
			t.Errorf("Failed = %d, want 3 recorded", run.Validation.Phases[0].Failed)
		}
		if run.Validation.Skipped != "" {
			t.Errorf("Skipped = %q, want empty -- validation ran, it just found problems", run.Validation.Skipped)
		}
	})
}

// Drift means this console cannot prove the recipe it would validate is the
// one that was installed. Refusing is the honest outcome; validating anyway
// would attest to the wrong thing.
func TestValidateRefusesADriftedRecipe(t *testing.T) {
	// The fake re-resolves recipeFixture(), but the run's approved recipe.json
	// describes a DIFFERENT component version -- the shape of an operator who
	// upgraded the binary between install and validate.
	recipe := recipeFixture()
	drifted := recipeFixture()
	drifted.Components[0].Version = "v9.9.9-not-what-was-installed"

	fake := &aicrclient.Fake{Recipe: recipe}
	step := steps.NewValidate(fake, steps.ValidateConfig{WorkDir: t.TempDir()})
	run := newRunWithRealCluster(t, drifted)

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if fake.ValidateCalls != 0 {
		t.Errorf("ValidateCalls = %d, want 0 -- a drifted recipe must not be validated", fake.ValidateCalls)
	}
	if !strings.Contains(run.Validation.Skipped, "drifted") {
		t.Errorf("Skipped = %q, want it to name the drift", run.Validation.Skipped)
	}
}

// newRunWithRealCluster is a run whose artifacts describe a cluster WITH
// GPUs, so skipReason lets validation proceed, and whose approved recipe
// matches what the fake will re-resolve, so assertMatchesApproved passes.
func newRunWithRealCluster(t *testing.T, recipe *aicr.RecipeResult) *engine.Run {
	t.Helper()
	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	run.Artifacts["capability.json"] = []byte(`{"totalGpus":16,"usableGpus":16,"analyzed":true}`)
	run.Artifacts["snapshot.yaml"] = []byte(minimalSnapshot)
	run.Artifacts["recipe.json"] = approvedFrom(t, recipe)
	return run
}
