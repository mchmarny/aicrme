package aicrclient_test

import (
	"context"
	"errors"
	"os"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/mchmarny/aicrme/internal/aicrclient"
)

// loadH100Raw reads the real simulated-H100 KWOK snapshot fixture
// internal/steps/recommend_test.go resolves against, so tests in this
// package can drive AvailableOptions's stage-2 verification path with a
// snapshot that genuinely fingerprints to service=kind -- fingerprint
// derivation runs off the YAML content alone and does not require the real
// client's loaded catalog, so a Fake can use these bytes too.
func loadH100Raw(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../steps/testdata/snapshot-kwok-h100.yaml")
	if err != nil {
		t.Fatalf("fixture read error = %v", err)
	}
	return raw
}

// TestAvailableOptionsBucketsByReturnedPlatformWhenProvisional exercises
// stage 1 (catalog candidates) in isolation: rawSnapshot=nil means there is
// no cluster coordinate to verify against, so AvailableOptions returns the
// catalog-shaped candidate set as-is, Provisional=true, and never calls
// ResolveRecipeFromSnapshot.
func TestAvailableOptionsBucketsByReturnedPlatformWhenProvisional(t *testing.T) {
	fake := &aicrclient.Fake{
		Registry: recipe.NewCriteriaRegistry(),
		CatalogEntries: []aicr.CatalogEntry{
			{Name: "a", Criteria: aicr.Criteria{Platform: "kubeflow"}},
			{Name: "b", Criteria: aicr.Criteria{Platform: ""}},         // unset -> "any"
			{Name: "c", Criteria: aicr.Criteria{Platform: "kubeflow"}}, // duplicate, deduped
		},
	}

	got, err := aicrclient.AvailableOptions(context.Background(), fake, nil)
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
	if fake.ResolveCalls != 0 {
		t.Errorf("ResolveCalls = %d, want 0 -- no snapshot means nothing to verify against", fake.ResolveCalls)
	}
	// Fake.ListCatalog ignores the query filter, so every candidate intent
	// sees the same three entries -- both "inference" and "training" end up
	// with the same per-intent breakdown as the flat union.
	for _, intent := range wantIntents {
		if !equalStrings(got.PlatformsByIntent[intent], wantPlatforms) {
			t.Errorf("PlatformsByIntent[%q] = %v, want %v", intent, got.PlatformsByIntent[intent], wantPlatforms)
		}
	}
	if !got.Provisional {
		t.Error("Provisional = false, want true for a nil snapshot")
	}
}

// TestAvailableOptionsProvisionalClearsWithARealSnapshot proves the
// provisional-vs-verified split is driven by whether a snapshot fingerprints
// to a real service, not by whether the catalog happens to have candidates.
// Fake.CatalogEntries is left unset here deliberately: even with zero
// candidates to verify, a snapshot that fingerprints to service=kind must
// still flip Provisional to false, because "verified, found nothing" and
// "never verified" are different claims a client acts on differently.
func TestAvailableOptionsProvisionalClearsWithARealSnapshot(t *testing.T) {
	fake := &aicrclient.Fake{Registry: recipe.NewCriteriaRegistry()}

	got, err := aicrclient.AvailableOptions(context.Background(), fake, nil)
	if err != nil {
		t.Fatalf("AvailableOptions() error = %v", err)
	}
	if !got.Provisional {
		t.Error("Provisional = false, want true for a nil snapshot")
	}

	got, err = aicrclient.AvailableOptions(context.Background(), fake, loadH100Raw(t))
	if err != nil {
		t.Fatalf("AvailableOptions() error = %v", err)
	}
	if got.Provisional {
		t.Error("Provisional = true, want false once the snapshot fingerprints to service=kind")
	}
}

// TestAvailableOptionsVerificationFailureExcludesAllPairs proves stage 2 is
// load-bearing, not decorative: a candidate the catalog lists but that fails
// to actually resolve must not be offered, even though it was a candidate.
func TestAvailableOptionsVerificationFailureExcludesAllPairs(t *testing.T) {
	fake := &aicrclient.Fake{
		Registry: recipe.NewCriteriaRegistry(),
		CatalogEntries: []aicr.CatalogEntry{
			{Name: "a", Criteria: aicr.Criteria{Intent: "training", Platform: "kubeflow"}},
		},
		ResolveErr: errors.New("no recipe for these coordinates"),
	}

	got, err := aicrclient.AvailableOptions(context.Background(), fake, loadH100Raw(t))
	if err != nil {
		t.Fatalf("AvailableOptions() error = %v", err)
	}
	if got.Provisional {
		t.Error("Provisional = true, want false: a real snapshot was supplied and verification ran")
	}
	if len(got.Platforms) != 0 || len(got.PlatformsByIntent) != 0 {
		t.Errorf("got = %+v, want every candidate excluded after a resolve failure", got)
	}
	if fake.ResolveCalls == 0 {
		t.Error("ResolveCalls = 0, want at least 1 -- verification must actually run")
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

	got, err := aicrclient.AvailableOptions(context.Background(), client, nil)
	if err != nil {
		t.Fatalf("AvailableOptions() error = %v", err)
	}
	if !equalStrings(got.Intents, []string{"training"}) {
		t.Errorf("Intents = %v, want [training]", got.Intents)
	}
	if !equalStrings(got.Platforms, []string{"slurm"}) {
		t.Errorf("Platforms = %v, want [slurm]", got.Platforms)
	}
	if !equalStrings(got.PlatformsByIntent["training"], []string{"slurm"}) {
		t.Errorf(`PlatformsByIntent["training"] = %v, want [slurm]`, got.PlatformsByIntent["training"])
	}
	if _, ok := got.PlatformsByIntent["inference"]; ok {
		t.Errorf(`PlatformsByIntent["inference"] = %v, want no entry (no coverage)`, got.PlatformsByIntent["inference"])
	}
}

func TestAvailableOptionsPropagatesListCatalogError(t *testing.T) {
	fake := &aicrclient.Fake{Registry: recipe.NewCriteriaRegistry(), CatalogErr: errors.New("catalog unavailable")}
	_, err := aicrclient.AvailableOptions(context.Background(), fake, nil)
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
	_, err := aicrclient.AvailableOptions(context.Background(), fake, nil)
	if err == nil {
		t.Fatal("AvailableOptions() returned nil for a nil criteria registry")
	}
}

// TestAvailableOptionsAgainstRealCatalog pins AvailableOptions's output for
// the real simulated-H100 KWOK fixture against the real embedded v0.19.0
// catalog to exactly the matrix internal/steps/recommend_test.go's
// TestRecommendResolvesAgainstSimulatedH100Fixture already established by
// actually resolving: dynamo, kubeflow, slurm, and any are the only
// platforms with a service=kind overlay that this fixture's hardware can
// satisfy -- nim and runai never have one, regardless of intent. Because
// AvailableOptions now verifies each candidate by really calling
// ResolveRecipeFromSnapshot (not just bucketing catalog entries), this test
// and TestRecommendResolvesAgainstSimulatedH100Fixture are pinning the same
// fact through two independent code paths; internal/steps/options_cross_test.go
// checks them against each other directly.
func TestAvailableOptionsAgainstRealCatalog(t *testing.T) {
	client, err := aicrclient.New()
	if err != nil {
		t.Fatalf("aicrclient.New() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	got, err := aicrclient.AvailableOptions(context.Background(), client, loadH100Raw(t))
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
	// The per-pair breakdown: "dynamo" only resolves for "inference",
	// "kubeflow" and "slurm" only for "training" -- exactly the split
	// TestRecommendResolvesAgainstSimulatedH100Fixture pins by actually
	// resolving. A flat Platforms union alone cannot express this.
	wantByIntent := map[string][]string{
		"training":  {"any", "kubeflow", "slurm"},
		"inference": {"any", "dynamo"},
	}
	for intent, want := range wantByIntent {
		if !equalStrings(got.PlatformsByIntent[intent], want) {
			t.Errorf("PlatformsByIntent[%q] = %v, want %v", intent, got.PlatformsByIntent[intent], want)
		}
	}
	if got.Provisional {
		t.Error("Provisional = true, want false for a snapshot that fingerprints to service=kind")
	}
}

// TestAvailableOptionsUnconstrainedBeforeDiscover documents the pre-Discover
// default: rawSnapshot=nil widens the filter to catalog-wide candidates
// (unverified, Provisional=true) rather than failing. "runai" has zero
// overlays anywhere in the embedded catalog (verified via a throwaway
// diagnostic against the real client during this task), so it is excluded
// even unconstrained; "nim" is not (it has eks/ocp overlays), so it
// reappears here even though it is absent from the service=kind-narrowed
// result above.
func TestAvailableOptionsUnconstrainedBeforeDiscover(t *testing.T) {
	client, err := aicrclient.New()
	if err != nil {
		t.Fatalf("aicrclient.New() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	got, err := aicrclient.AvailableOptions(context.Background(), client, nil)
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
	if !got.Provisional {
		t.Error("Provisional = false, want true for a nil snapshot")
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

func TestServiceFromSnapshotDerivesKindFromTheH100Fixture(t *testing.T) {
	fake := &aicrclient.Fake{Registry: recipe.NewCriteriaRegistry()}
	got, err := aicrclient.ServiceFromSnapshot(fake, loadH100Raw(t))
	if err != nil {
		t.Fatalf("ServiceFromSnapshot() error = %v", err)
	}
	if got != "kind" {
		t.Errorf("ServiceFromSnapshot() = %q, want %q", got, "kind")
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
