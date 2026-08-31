package aicrclient_test

import (
	"context"
	"errors"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/mchmarny/aicrme/internal/aicrclient"
)

func entry(service string) aicr.CatalogEntry {
	return aicr.CatalogEntry{Name: service + "-overlay", Criteria: aicr.Criteria{Service: service}, IsLeaf: true}
}

func mkRecipe(refs ...aicr.ComponentRef) *aicr.RecipeResult {
	return &aicr.RecipeResult{Components: refs}
}

func twoEntries() *aicrclient.Fake {
	return &aicrclient.Fake{
		CatalogEntries: []aicr.CatalogEntry{entry("eks"), entry("gke")},
		Recipes: map[string]*aicr.RecipeResult{
			"eks": mkRecipe(
				aicr.ComponentRef{Name: "cert-manager", Chart: "cert-manager", Namespace: "cert-manager"},
				aicr.ComponentRef{Name: "aws-efa", Chart: "aws-efa", Namespace: "kube-system"},
			),
			"gke": mkRecipe(aicr.ComponentRef{Name: "cert-manager", Chart: "cert-manager", Namespace: "cert-manager"}),
		},
	}
}

// The union is the point. aws-efa exists only in EKS recipes, and a survey
// blind to it would report the release as unknown on the exact cluster it
// belongs to.
func TestUniverseUnionsEveryCatalogEntry(t *testing.T) {
	f := twoEntries()

	u, err := aicrclient.Universe(context.Background(), f)
	if err != nil {
		t.Fatalf("Universe() error = %v", err)
	}
	if !u.Complete {
		t.Error("Complete = false with every entry resolving")
	}
	for _, want := range []string{"aws-efa", "cert-manager"} {
		if _, ok := u.Charts[want]; !ok {
			t.Errorf("%s missing from the universe", want)
		}
	}
	if f.CriteriaResolves != 2 {
		t.Errorf("resolved %d times, want one per catalog entry (2)", f.CriteriaResolves)
	}
}

// THE TEST REVISION 1 GOT WRONG. It claimed to cover a failing entry and
// injected no error at all, so the fail-soft path was never exercised.
//
// One entry failing must NOT blank the universe -- a survey that recognized
// nothing would report a fully installed cluster as clean -- but it must also
// not pass silently. Complete=false is what stops anything being recommended
// on partial knowledge.
func TestUniverseIsIncompleteWhenAnEntryFailsToResolve(t *testing.T) {
	f := twoEntries()
	f.ResolveErrs = map[string]error{"gke": errors.New("overlay not resolvable by this client")}

	u, err := aicrclient.Universe(context.Background(), f)
	if err != nil {
		t.Fatalf("Universe() error = %v; a single bad entry must not be fatal", err)
	}
	if u.Complete {
		t.Error("Complete = true after an entry failed; nothing may be recommended on partial knowledge")
	}
	if u.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", u.Skipped)
	}
	if _, ok := u.Charts["aws-efa"]; !ok {
		t.Error("the surviving entry's charts were lost; an empty universe reads as a clean cluster")
	}
}

// Cancellation is NOT a skippable entry. Treating it as one would return a
// half-built universe as if it were merely incomplete, when the truth is that
// the caller has gone away and no answer should be produced at all.
func TestUniverseReturnsAnErrorWhenTheContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := aicrclient.Universe(ctx, twoEntries()); err == nil {
		t.Fatal("Universe succeeded on a canceled context")
	}
}

// A failing catalog is fatal: with no entries there is nothing to union, and
// an empty universe is the most dangerous wrong answer this can give.
func TestUniverseFailsWhenTheCatalogCannotBeListed(t *testing.T) {
	f := &aicrclient.Fake{CatalogErr: errors.New("catalog unavailable")}

	if _, err := aicrclient.Universe(context.Background(), f); err == nil {
		t.Fatal("Universe succeeded with no catalog")
	}
}

// Keyed by CHART, not by Name: the registry's defaultChart override lets them
// differ -- AICR calls it "nfd", the chart is "node-feature-discovery" -- and
// the cluster only ever shows the chart.
func TestUniverseIsKeyedByChartNotComponentName(t *testing.T) {
	f := &aicrclient.Fake{
		CatalogEntries: []aicr.CatalogEntry{entry("eks")},
		Recipes: map[string]*aicr.RecipeResult{
			"eks": mkRecipe(aicr.ComponentRef{Name: "nfd", Chart: "node-feature-discovery"}),
		},
	}

	u, err := aicrclient.Universe(context.Background(), f)
	if err != nil {
		t.Fatalf("Universe() error = %v", err)
	}
	if _, ok := u.Charts["nfd"]; ok {
		t.Error("keyed by component name; the cluster never shows that name")
	}
	got, ok := u.Charts["node-feature-discovery"]
	if !ok {
		t.Fatal("not keyed by chart")
	}
	if got.Name != "nfd" {
		t.Errorf("Name = %q, want AICR's identifier carried alongside the chart", got.Name)
	}
}

// Chart is optional and defaults to Name. Without the fallback every component
// with no defaultChart override silently drops out of the universe.
func TestUniverseFallsBackToNameWhenChartIsUnset(t *testing.T) {
	f := &aicrclient.Fake{
		CatalogEntries: []aicr.CatalogEntry{entry("eks")},
		Recipes:        map[string]*aicr.RecipeResult{"eks": mkRecipe(aicr.ComponentRef{Name: "nvsentinel"})},
	}

	u, err := aicrclient.Universe(context.Background(), f)
	if err != nil {
		t.Fatalf("Universe() error = %v", err)
	}
	if _, ok := u.Charts["nvsentinel"]; !ok {
		t.Error("component with no Chart dropped from the universe")
	}
}

// Order drives removal sequence in increment 2: cert-manager must come out
// after gpu-operator, whose uninstall hooks need its certificates. Recipes
// disagree on position, so the LATEST wins -- removing late is the safe
// direction when the answer is ambiguous.
func TestUniverseKeepsTheLatestOrderAcrossRecipes(t *testing.T) {
	f := &aicrclient.Fake{
		CatalogEntries: []aicr.CatalogEntry{entry("eks"), entry("gke")},
		Recipes: map[string]*aicr.RecipeResult{
			"eks": mkRecipe(
				aicr.ComponentRef{Name: "cert-manager", Chart: "cert-manager"},
				aicr.ComponentRef{Name: "gpu-operator", Chart: "gpu-operator"},
			),
			"gke": mkRecipe(
				aicr.ComponentRef{Name: "gpu-operator", Chart: "gpu-operator"},
				aicr.ComponentRef{Name: "kai-scheduler", Chart: "kai-scheduler"},
				aicr.ComponentRef{Name: "cert-manager", Chart: "cert-manager"},
			),
		},
	}

	u, err := aicrclient.Universe(context.Background(), f)
	if err != nil {
		t.Fatalf("Universe() error = %v", err)
	}
	if u.Charts["cert-manager"].Order <= u.Charts["gpu-operator"].Order {
		t.Errorf("cert-manager Order %d not after gpu-operator %d; removal would break gpu-operator's uninstall hooks",
			u.Charts["cert-manager"].Order, u.Charts["gpu-operator"].Order)
	}
}
