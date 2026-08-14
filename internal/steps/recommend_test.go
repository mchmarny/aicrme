package steps_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/steps"
)

func recipeFixture() *aicr.RecipeResult {
	return &aicr.RecipeResult{
		Name:    "h100-eks-ubuntu-training",
		Version: "0.19.0",
		Components: []aicr.ComponentRef{
			{Name: "gpu-operator", Kind: "Helm", Version: "v26.3.3", Namespace: "gpu-operator",
				Chart: "gpu-operator", Source: "https://helm.ngc.nvidia.com/nvidia"},
			{Name: "kai-scheduler", Kind: "Helm", Version: "v0.14.1", Namespace: "kai-scheduler",
				Chart: "kai-scheduler", Source: "oci://ghcr.io/kai-scheduler/kai-scheduler"},
		},
	}
}

// minimalSnapshot is syntactically-valid, content-free Snapshot YAML for
// tests where only the presence of the artifact matters, not its
// measurements. TestRecommendResolveAgainstRealFixture below is what
// exercises the real captured measurements.
const minimalSnapshot = "apiVersion: aicr.nvidia.com/v1\nkind: Snapshot\n"

func TestRecommendRequiresExactlyTwoDecisions(t *testing.T) {
	step := steps.NewRecommend(&aicrclient.Fake{})
	got := step.Requires()
	want := []string{"intent", "platform"}
	if len(got) != len(want) {
		t.Fatalf("Requires() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Requires()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRecommendMapsDecisionsToCriteria(t *testing.T) {
	fake := &aicrclient.Fake{Recipe: recipeFixture()}
	step := steps.NewRecommend(fake)

	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	run.Artifacts["snapshot.yaml"] = []byte(minimalSnapshot)

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if fake.LastCriteria == nil {
		t.Fatal("resolver was called without criteria")
	}
	if fake.LastCriteria.Intent != "training" {
		t.Errorf("Criteria.Intent = %q, want %q", fake.LastCriteria.Intent, "training")
	}
	if fake.LastCriteria.Platform != "kubeflow" {
		t.Errorf("Criteria.Platform = %q, want %q", fake.LastCriteria.Platform, "kubeflow")
	}
}

func TestRecommendStoresComponentSummary(t *testing.T) {
	fake := &aicrclient.Fake{Recipe: recipeFixture()}
	step := steps.NewRecommend(fake)

	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	run.Artifacts["snapshot.yaml"] = []byte(minimalSnapshot)

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	var summary steps.RecipeSummary
	if err := json.Unmarshal(run.Artifacts["recipe.json"], &summary); err != nil {
		t.Fatalf("recipe.json decode error = %v", err)
	}
	if summary.ComponentCount != 2 {
		t.Errorf("ComponentCount = %d, want 2", summary.ComponentCount)
	}
	if summary.Components[0].Version == "" {
		t.Error("component version not carried — every version must be shown pinned")
	}
}

func TestRecommendPropagatesResolveFailure(t *testing.T) {
	fake := &aicrclient.Fake{ResolveErr: errors.New("no recipe for these coordinates")}
	step := steps.NewRecommend(fake)

	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	run.Artifacts["snapshot.yaml"] = []byte(minimalSnapshot)

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err == nil {
		t.Fatal("Run() returned nil on a resolve failure")
	}
}

func TestRecommendFailsOnNilResult(t *testing.T) {
	// A Resolver returning (nil, nil) is not something aicr.Client does in
	// practice, but Resolver is an interface Recommend depends on, not a
	// concrete type — this guards against a future or alternate
	// implementation doing so silently.
	fake := &aicrclient.Fake{}
	step := steps.NewRecommend(fake)

	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	run.Artifacts["snapshot.yaml"] = []byte(minimalSnapshot)

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err == nil {
		t.Fatal("Run() returned nil for a nil recipe result")
	}
}

func TestRecommendPhase(t *testing.T) {
	if got := steps.NewRecommend(&aicrclient.Fake{}).Phase(); got != engine.PhaseRecommend {
		t.Errorf("Phase() = %q, want %q", got, engine.PhaseRecommend)
	}
}

// TestRecommendFailsLoudlyWithoutSnapshot asserts the judgement call made in
// decodeSnapshot: Discover always runs before Recommend in the wired
// pipeline, so a missing snapshot.yaml artifact means something upstream
// broke, not a legitimate "no cluster info" case. Recommend must say so
// rather than silently resolving criteria-only against a nil snapshot,
// which the real aicr.Client rejects anyway ("snapshot is required (got
// nil)") — proceeding here would just defer that failure past this whole
// test file, since aicrclient.Fake never validates its arguments.
func TestRecommendFailsLoudlyWithoutSnapshot(t *testing.T) {
	fake := &aicrclient.Fake{Recipe: recipeFixture()}
	step := steps.NewRecommend(fake)

	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	// snapshot.yaml deliberately left unset.

	err := step.Run(context.Background(), run, func(bus.Event) {})
	if err == nil {
		t.Fatal("Run() returned nil with no snapshot.yaml artifact")
	}
	if fake.ResolveCalls != 0 {
		t.Error("resolver was called despite a missing snapshot")
	}
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeInvalidRequest {
		t.Errorf("error = %v, want a StructuredError with ErrCodeInvalidRequest", err)
	}
}

func TestRecommendFailsOnUnparseableSnapshot(t *testing.T) {
	fake := &aicrclient.Fake{Recipe: recipeFixture()}
	step := steps.NewRecommend(fake)

	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	run.Artifacts["snapshot.yaml"] = []byte("- this\n- is\n- a list, not a Snapshot\n")

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err == nil {
		t.Fatal("Run() returned nil for an unparseable snapshot artifact")
	}
	if fake.ResolveCalls != 0 {
		t.Error("resolver was called with an unparseable snapshot")
	}
}

// TestRecommendRejectsEmptyDecisionValues covers the other half of the
// specificity judgement call: engine.Engine.awaitDecisions only checks that
// "intent" and "platform" are present as keys in Run.Decisions, never that
// their values are non-blank. AICR's facade does not itself guard against
// zero-specificity criteria (see TestRecommendResolveAgainstRealFixture),
// so Recommend is the only place left that can catch a blank decision
// before it reaches resolve().
func TestRecommendRejectsEmptyDecisionValues(t *testing.T) {
	tests := []struct {
		name     string
		intent   string
		platform string
	}{
		{"empty intent", "", "kubeflow"},
		{"empty platform", "training", ""},
		{"both empty", "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &aicrclient.Fake{Recipe: recipeFixture()}
			step := steps.NewRecommend(fake)

			run := newRun()
			run.Decisions["intent"] = tc.intent
			run.Decisions["platform"] = tc.platform
			run.Artifacts["snapshot.yaml"] = []byte(minimalSnapshot)

			if err := step.Run(context.Background(), run, func(bus.Event) {}); err == nil {
				t.Fatal("Run() returned nil for a blank decision value")
			}
			if fake.ResolveCalls != 0 {
				t.Error("resolver was called with a blank decision value")
			}
		})
	}
}

// TestRecommendResolveAgainstRealFixture proves the real, load-bearing
// behavior against the captured KWOK cluster: what AICR's facade actually
// does with Recommend's exact Criteria shape (Intent+Platform only, nothing
// else derived from the snapshot). aicrclient.Fake never rejects anything,
// so a regression here would pass every other test in this file while
// failing on every real run.
//
// Empirically (aicr v0.19.0, embedded catalog): Criteria{Intent:"training",
// Platform:"kubeflow"} resolved against the KWOK fixture fails — but not
// because Specificity()==0 as the brief predicted (it's 2: both fields are
// set). It fails AICR's criteria-coverage post-condition (issue #1542),
// which is STRICT on this path: intent=training and platform=kubeflow each
// require a covering (service, accelerator, os) combination that this
// console never supplies, because Recommend does not derive
// Service/Accelerator/OS from the snapshot fingerprint the way
// `aicr recipe --snapshot` does internally in AICR's own pkg/cli/query.go.
//
// The observable contract the brief asked for — an error surfaces, not a
// silent generic recipe — does hold. See task-10-report.md for why the
// mechanism differs from the brief's prediction and what it means for
// Task 13's KWOK end-to-end run.
func TestRecommendResolveAgainstRealFixture(t *testing.T) {
	client, err := aicrclient.New()
	if err != nil {
		t.Fatalf("aicrclient.New() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	step := steps.NewRecommend(client)
	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	snap := loadSnapshot(t)
	run.Artifacts["snapshot.yaml"] = snap.Raw

	runErr := step.Run(context.Background(), run, func(bus.Event) {})
	if runErr == nil {
		t.Fatal("Run() succeeded resolving intent+platform alone against the KWOK fixture — " +
			"if AICR's behavior changed, update this test and the Task 13 plan")
	}

	var se *aicrerrors.StructuredError
	if !errors.As(runErr, &se) || se.Code != aicrerrors.ErrCodeInvalidRequest {
		t.Errorf("error = %v, want a StructuredError with ErrCodeInvalidRequest", runErr)
	}
	t.Logf("observed resolve error against the real KWOK fixture: %v", runErr)
}
