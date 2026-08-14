package aicrclient_test

import (
	"context"
	"errors"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/mchmarny/aicrme/internal/aicrclient"
)

func TestAvailableOptionsBucketsByReturnedPlatform(t *testing.T) {
	fake := &aicrclient.Fake{
		Registry: recipe.NewCriteriaRegistry(),
		CatalogEntries: []aicr.CatalogEntry{
			{Name: "a", Criteria: aicr.Criteria{Platform: "kubeflow"}},
			{Name: "b", Criteria: aicr.Criteria{Platform: ""}},         // unset -> "any"
			{Name: "c", Criteria: aicr.Criteria{Platform: "kubeflow"}}, // duplicate, deduped
		},
	}

	got, err := aicrclient.AvailableOptions(context.Background(), fake, "kind")
	if err != nil {
		t.Fatalf("AvailableOptions() error = %v", err)
	}

	wantPlatforms := []string{"any", "kubeflow"}
	if !equalStrings(got.Platforms, wantPlatforms) {
		t.Errorf("Platforms = %v, want %v", got.Platforms, wantPlatforms)
	}
	wantIntents := []string{"inference", "training"}
	if !equalStrings(got.Intents, wantIntents) {
		t.Errorf("Intents = %v, want %v", got.Intents, wantIntents)
	}
	if fake.CatalogCalls != len(wantIntents) {
		t.Errorf("CatalogCalls = %d, want %d (one per candidate intent)", fake.CatalogCalls, len(wantIntents))
	}
}

func TestAvailableOptionsExcludesIntentWithNoCoverage(t *testing.T) {
	callCount := 0
	fake := &aicrclient.Fake{Registry: recipe.NewCriteriaRegistry()}
	// Fake.ListCatalog can't itself vary its answer per call, so drive this
	// through a tiny wrapper that returns entries only for "training".
	client := scriptedByIntent{
		Fake: fake,
		byIntent: map[string][]aicr.CatalogEntry{
			"training": {{Name: "a", Criteria: aicr.Criteria{Intent: "training", Platform: "slurm"}}},
		},
		calls: &callCount,
	}

	got, err := aicrclient.AvailableOptions(context.Background(), client, "kind")
	if err != nil {
		t.Fatalf("AvailableOptions() error = %v", err)
	}
	if !equalStrings(got.Intents, []string{"training"}) {
		t.Errorf("Intents = %v, want [training]", got.Intents)
	}
	if !equalStrings(got.Platforms, []string{"slurm"}) {
		t.Errorf("Platforms = %v, want [slurm]", got.Platforms)
	}
}

func TestAvailableOptionsPropagatesListCatalogError(t *testing.T) {
	fake := &aicrclient.Fake{Registry: recipe.NewCriteriaRegistry(), CatalogErr: errors.New("catalog unavailable")}
	_, err := aicrclient.AvailableOptions(context.Background(), fake, "kind")
	if err == nil {
		t.Fatal("AvailableOptions() returned nil for a ListCatalog error")
	}
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) {
		t.Errorf("error = %v, want a StructuredError", err)
	}
}

func TestAvailableOptionsRejectsNilRegistry(t *testing.T) {
	fake := &aicrclient.Fake{} // zero-value Registry is nil
	_, err := aicrclient.AvailableOptions(context.Background(), fake, "kind")
	if err == nil {
		t.Fatal("AvailableOptions() returned nil for a nil criteria registry")
	}
}

// TestAvailableOptionsAgainstRealCatalog pins AvailableOptions's output for
// service=kind against the real embedded v0.19.0 catalog to exactly the
// matrix internal/steps/recommend_test.go's
// TestRecommendResolvesAgainstSimulatedH100Fixture and
// TestRecommendKWOKGPUlessFixtureMatrix already established empirically:
// dynamo, kubeflow, slurm, and any are the only platforms with a
// service=kind overlay for some intent -- nim and runai never have one,
// regardless of intent. If AICR's catalog changes this, this test fails
// rather than /api/options silently offering (or hiding) an option.
// internal/steps/options_cross_test.go goes one step further and
// cross-checks this same output against real Recommend resolution.
func TestAvailableOptionsAgainstRealCatalog(t *testing.T) {
	client, err := aicrclient.New()
	if err != nil {
		t.Fatalf("aicrclient.New() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	got, err := aicrclient.AvailableOptions(context.Background(), client, "kind")
	if err != nil {
		t.Fatalf("AvailableOptions() error = %v", err)
	}

	wantIntents := []string{"inference", "training"}
	if !equalStrings(got.Intents, wantIntents) {
		t.Errorf("Intents = %v, want %v", got.Intents, wantIntents)
	}
	wantPlatforms := []string{"any", "dynamo", "kubeflow", "slurm"}
	if !equalStrings(got.Platforms, wantPlatforms) {
		t.Errorf("Platforms = %v, want %v (nim and runai have no service=kind overlay for any intent)",
			got.Platforms, wantPlatforms)
	}
}

// TestAvailableOptionsUnconstrainedBeforeDiscover documents the pre-Discover
// default: service="" widens the filter to catalog-wide coverage rather
// than failing. "runai" has zero overlays anywhere in the embedded catalog
// (verified via a throwaway diagnostic against the real client during this
// task), so it is excluded even unconstrained; "nim" is not (it has
// eks/ocp overlays), so it reappears here even though it is absent from the
// service=kind-narrowed result above.
func TestAvailableOptionsUnconstrainedBeforeDiscover(t *testing.T) {
	client, err := aicrclient.New()
	if err != nil {
		t.Fatalf("aicrclient.New() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	got, err := aicrclient.AvailableOptions(context.Background(), client, "")
	if err != nil {
		t.Fatalf("AvailableOptions() error = %v", err)
	}
	for _, want := range []string{"any", "dynamo", "kubeflow", "nim", "slurm"} {
		if !containsString(got.Platforms, want) {
			t.Errorf("Platforms = %v, want it to contain %q", got.Platforms, want)
		}
	}
	if containsString(got.Platforms, "runai") {
		t.Errorf("Platforms = %v, want it to exclude %q (zero overlays anywhere in the catalog)", got.Platforms, "runai")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

func TestServiceFromSnapshotEmptyRawIsNotAnError(t *testing.T) {
	fake := &aicrclient.Fake{}
	got, err := aicrclient.ServiceFromSnapshot(fake, nil)
	if err != nil {
		t.Fatalf("ServiceFromSnapshot() error = %v, want nil for empty input", err)
	}
	if got != "" {
		t.Errorf("ServiceFromSnapshot() = %q, want empty", got)
	}
}

func TestServiceFromSnapshotUnparseableRawErrors(t *testing.T) {
	fake := &aicrclient.Fake{}
	_, err := aicrclient.ServiceFromSnapshot(fake, []byte("- this\n- is\n- a list, not a Snapshot\n"))
	if err == nil {
		t.Fatal("ServiceFromSnapshot() returned nil for unparseable input")
	}
}

// scriptedByIntent wraps a *aicrclient.Fake so ListCatalog's response can
// vary by the query's Intent -- something Fake itself deliberately does not
// do (it is a dumb stub everywhere else in this codebase).
type scriptedByIntent struct {
	*aicrclient.Fake
	byIntent map[string][]aicr.CatalogEntry
	calls    *int
}

func (s scriptedByIntent) ListCatalog(_ context.Context, filter *aicr.Criteria) ([]aicr.CatalogEntry, error) {
	*s.calls++
	return s.byIntent[filter.Intent], nil
}
