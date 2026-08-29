package steps_test

import (
	"context"
	"encoding/json"
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

	run := newRunWithSimulatedCluster(t)

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
	// WithValidationRunID would break the per-run namespace scoping, and
	// gaining an extra WithValidationPhases would add a phase Run() never
	// asked for. WithValidationCleanup(true) itself is a no-op today --
	// validator.New already defaults Cleanup to true -- but the count still
	// has to survive it being dropped in a refactor that later flips that
	// default, which is exactly the kind of change this count is here to
	// catch before it ships silently.
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
		// Finding 5 (final-review): this step has no ownership record for
		// Reset to clean up against, so a ValidateState error can leave the
		// aicr-validation namespace's RBAC and Jobs behind with nothing in
		// aicrme's own inventory pointing at them. No new cleanup code -- the
		// fix is that the operator reading the screen is told where to look.
		if !strings.Contains(run.Validation.Skipped, "aicr-validation") {
			t.Errorf("Skipped = %q, want it to name the aicr-validation namespace so the operator knows where to look for leftovers", run.Validation.Skipped)
		}
		if len(run.Validation.Phases) != 0 {
			t.Errorf("Phases = %+v, want none recorded when validation fails to run", run.Validation.Phases)
		}
		if run.Validation.ReportPath != "" {
			t.Errorf("ReportPath = %q, want empty when validation fails to run", run.Validation.ReportPath)
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

// The panel is driven by the payload, not the prose. Both the skip and the
// verdict have to arrive as data, or the console has nothing to render but a
// sentence it would have to parse.
func TestValidatePublishesTheVerdictAsData(t *testing.T) {
	decode := func(t *testing.T, events []bus.Event) engine.Validation {
		t.Helper()
		for i := len(events) - 1; i >= 0; i-- {
			if len(events[i].Data) == 0 {
				continue
			}
			var got engine.Validation
			if err := json.Unmarshal(events[i].Data, &got); err != nil {
				t.Fatalf("Unmarshal(%s) error = %v", events[i].Data, err)
			}
			return got
		}
		t.Fatal("no event carried a payload -- the panel would never populate")
		return engine.Validation{}
	}

	t.Run("a skip", func(t *testing.T) {
		var events []bus.Event
		fake := &aicrclient.Fake{Recipe: recipeFixture()}
		step := steps.NewValidate(fake, steps.ValidateConfig{WorkDir: t.TempDir()})
		run := newRunWithSimulatedCluster(t)

		if err := step.Run(context.Background(), run, func(e bus.Event) { events = append(events, e) }); err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
		got := decode(t, events)
		if got.Skipped != run.Validation.Skipped {
			t.Errorf("published Skipped = %q, want %q", got.Skipped, run.Validation.Skipped)
		}
		if len(got.Phases) != 0 {
			t.Errorf("published Phases = %+v, want none -- a skip is not a verdict", got.Phases)
		}
	})

	t.Run("a verdict", func(t *testing.T) {
		var events []bus.Event
		recipe := recipeFixture()
		fake := &aicrclient.Fake{
			Recipe: recipe,
			PhaseResults: []*aicr.PhaseResult{{
				Phase: aicr.PhaseDeployment, Status: "passed",
				Summary:   aicr.ReportSummary{Tests: 14, Passed: 14},
				RawReport: []byte(`{"results":{}}`),
			}},
		}
		step := steps.NewValidate(fake, steps.ValidateConfig{WorkDir: t.TempDir()})
		run := newRunWithRealCluster(t, recipe)

		if err := step.Run(context.Background(), run, func(e bus.Event) { events = append(events, e) }); err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
		got := decode(t, events)
		if got.Skipped != "" {
			t.Errorf("published Skipped = %q, want empty", got.Skipped)
		}
		if len(got.Phases) != 1 || got.Phases[0].Passed != 14 {
			t.Errorf("published Phases = %+v, want one phase with 14 passed", got.Phases)
		}
	})
}

// Finding 7 (final-review): the verdict used to publish at level "" (which
// bus.Publish normalizes to info) regardless of outcome, so "validation: 11
// of 14 checks passed, 3 failed" rendered in the same neutral ink as routine
// narration while every skip already went out at warn and the panel colors
// a failure red. The fix keys the verdict event's level off whether any
// phase actually failed.
func TestValidatePublishesAFailingVerdictAtWarnLevel(t *testing.T) {
	run := func(t *testing.T, failed int) []bus.Event {
		t.Helper()
		recipe := recipeFixture()
		status := "passed"
		if failed > 0 {
			status = "failed"
		}
		fake := &aicrclient.Fake{
			Recipe: recipe,
			PhaseResults: []*aicr.PhaseResult{{
				Phase: aicr.PhaseDeployment, Status: status,
				Summary:   aicr.ReportSummary{Tests: 14, Passed: 14 - failed, Failed: failed},
				RawReport: []byte(`{"results":{}}`),
			}},
		}
		step := steps.NewValidate(fake, steps.ValidateConfig{WorkDir: t.TempDir()})
		r := newRunWithRealCluster(t, recipe)

		var events []bus.Event
		if err := step.Run(context.Background(), r, func(e bus.Event) { events = append(events, e) }); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		return events
	}

	t.Run("a failure warns", func(t *testing.T) {
		events := run(t, 3)
		last := events[len(events)-1]
		if last.Level != bus.LevelWarn {
			t.Errorf("verdict event Level = %q, want %q for a run with 3 failed checks", last.Level, bus.LevelWarn)
		}
	})

	t.Run("a clean pass stays at info", func(t *testing.T) {
		events := run(t, 0)
		last := events[len(events)-1]
		if last.Level != "" {
			t.Errorf("verdict event Level = %q, want empty (bus.Publish normalizes empty to info) for a clean pass", last.Level)
		}
	})
}

// The e2e's KWOK cluster reports 32 GPUs across four fake nodes. Before this,
// skipReason saw a healthy GPU count and let validation run against fakes.
//
// The run otherwise carries everything resolve() needs to reach
// ValidateState -- approved recipe, snapshot, decisions -- so this proves
// skipReason itself is what stops the call. A run missing those artifacts
// would also show ValidateCalls == 0, but for the wrong reason, and would not
// catch a skipReason that stopped reading Simulated.
func TestValidateSkipsASimulatedClusterThatReportsGPUs(t *testing.T) {
	recipe := recipeFixture()
	fake := &aicrclient.Fake{Recipe: recipe}
	step := steps.NewValidate(fake, steps.ValidateConfig{WorkDir: t.TempDir()})

	run := newRunWithSimulatedClusterThatReportsGPUs(t, recipe)

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v, want nil -- Validate never fails the run", err)
	}
	if fake.ValidateCalls != 0 {
		t.Errorf("ValidateCalls = %d, want 0 -- a healthy GPU count on a simulated cluster must still skip", fake.ValidateCalls)
	}
	if run.Validation.Skipped == "" {
		t.Error("Skipped is empty -- a simulated cluster that reports GPUs must still be recorded as skipped")
	}
	if !strings.Contains(run.Validation.Skipped, "simulated") {
		t.Errorf("Skipped = %q, want it to name the cluster as simulated", run.Validation.Skipped)
	}
}

// newRunWithSimulatedCluster is a run whose artifacts describe a cluster
// WITHOUT GPUs, so skipReason refuses validation before ValidateState is
// ever called.
func newRunWithSimulatedCluster(t *testing.T) *engine.Run {
	t.Helper()
	run := newRun()
	run.Artifacts["capability.json"] = []byte(`{"totalGpus":0,"usableGpus":0,"analyzed":true}`)
	return run
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

// newRunWithSimulatedClusterThatReportsGPUs is newRunWithRealCluster's run --
// approved recipe, snapshot, decisions, everything resolve() needs -- with
// capability.json swapped for the shape the e2e's KWOK cluster actually
// produces: a healthy GPU count (four fake nodes at eight apiece) alongside
// simulated: true. skipReason must refuse on the simulated flag, not on the
// GPU count -- this is the exact fixture the totalGpus == 0 check got wrong.
func newRunWithSimulatedClusterThatReportsGPUs(t *testing.T, recipe *aicr.RecipeResult) *engine.Run {
	t.Helper()
	run := newRunWithRealCluster(t, recipe)
	run.Artifacts["capability.json"] = []byte(`{"totalGpus":32,"usableGpus":32,"analyzed":true,"simulated":true}`)
	return run
}
