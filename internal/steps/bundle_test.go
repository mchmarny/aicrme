package steps_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/steps"
)

// approvedFrom renders the recipe.json artifact Recommend would have
// written for r, so a Bundle test can assert the match guard against the
// same shape the real pipeline produces.
func approvedFrom(t *testing.T, r *aicr.RecipeResult) []byte {
	t.Helper()
	summary := steps.RecipeSummary{
		Name: r.Name, Version: r.Version, ComponentCount: len(r.Components),
		Components: make([]steps.ComponentSummary, 0, len(r.Components)),
	}
	for _, c := range r.Components {
		summary.Components = append(summary.Components, steps.ComponentSummary{
			Name: c.Name, Kind: c.Kind, Version: c.Version,
			Namespace: c.Namespace, Chart: c.Chart, Source: c.Source,
		})
	}
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal approved summary error = %v", err)
	}
	return encoded
}

func TestBundleGatesOnNoDecisions(t *testing.T) {
	step := steps.NewBundle(&aicrclient.Fake{}, steps.BundleConfig{WorkDir: t.TempDir()})
	if got := step.Requires(); len(got) != 0 {
		t.Errorf("Requires() = %v, want none -- Bundle runs automatically after Recommend", got)
	}
}

func TestBundleWritesBundlePathArtifact(t *testing.T) {
	recipe := recipeFixture()
	fake := &aicrclient.Fake{Recipe: recipe}
	workDir := t.TempDir()
	step := steps.NewBundle(fake, steps.BundleConfig{WorkDir: workDir})

	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	run.Artifacts["snapshot.yaml"] = []byte(minimalSnapshot)
	run.Artifacts["recipe.json"] = approvedFrom(t, recipe)

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	want := filepath.Join(workDir, "runs", run.ID, "bundle")
	if got := string(run.Artifacts["bundle.path"]); got != want {
		t.Errorf("bundle.path = %q, want %q", got, want)
	}
	if fake.LastBundleDir != want {
		t.Errorf("MakeBundle OutputDir = %q, want %q", fake.LastBundleDir, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("bundle dir not created: %v", err)
	}
}

// The whole point of re-resolving instead of threading a handle from
// Recommend is that the user approved a specific component list. If the
// re-resolve drifts, bundling it anyway would install something the user
// never saw.
func TestBundleFailsClosedWhenReresolveDiffersFromApproved(t *testing.T) {
	approved := recipeFixture()
	drifted := recipeFixture()
	drifted.Components = drifted.Components[:1]

	fake := &aicrclient.Fake{Recipe: drifted}
	step := steps.NewBundle(fake, steps.BundleConfig{WorkDir: t.TempDir()})

	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	run.Artifacts["snapshot.yaml"] = []byte(minimalSnapshot)
	run.Artifacts["recipe.json"] = approvedFrom(t, approved)

	err := step.Run(context.Background(), run, func(bus.Event) {})
	if err == nil {
		t.Fatal("Run() error = nil, want a drift error")
	}
	if !strings.Contains(err.Error(), "component count") {
		t.Errorf("Run() error = %v, want it to name the count mismatch", err)
	}
	if fake.BundleCalls != 0 {
		t.Errorf("MakeBundle called %d times, want 0 -- must not bundle a drifted recipe", fake.BundleCalls)
	}
}

func TestBundleFailsClosedOnVersionDrift(t *testing.T) {
	approved := recipeFixture()
	drifted := recipeFixture()
	drifted.Components[0].Version = "v0.0.0-drifted"

	fake := &aicrclient.Fake{Recipe: drifted}
	step := steps.NewBundle(fake, steps.BundleConfig{WorkDir: t.TempDir()})

	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	run.Artifacts["snapshot.yaml"] = []byte(minimalSnapshot)
	run.Artifacts["recipe.json"] = approvedFrom(t, approved)

	err := step.Run(context.Background(), run, func(bus.Event) {})
	if err == nil || !strings.Contains(err.Error(), "gpu-operator") {
		t.Fatalf("Run() error = %v, want it to name the drifted component", err)
	}
}

func TestBundleRequiresRecommendToHaveRun(t *testing.T) {
	fake := &aicrclient.Fake{Recipe: recipeFixture()}
	step := steps.NewBundle(fake, steps.BundleConfig{WorkDir: t.TempDir()})

	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	run.Artifacts["snapshot.yaml"] = []byte(minimalSnapshot)
	// recipe.json deliberately absent.

	err := step.Run(context.Background(), run, func(bus.Event) {})
	if err == nil || !strings.Contains(err.Error(), "recipe.json") {
		t.Fatalf("Run() error = %v, want it to name the missing artifact", err)
	}
}
