package aicrclient

import (
	"context"
	"sort"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/fingerprint"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"gopkg.in/yaml.v3"
)

// platformAny mirrors the sentinel AICR's recipe package uses for an unset
// platform dimension (pkg/recipe.CriteriaPlatformAny stringifies to this).
// An overlay whose own Criteria.Platform is blank or this value applies
// regardless of the platform decision -- the "just the runtime, no specific
// platform" option (the product spec's fourth user-facing platform choice;
// see task-10-report.md for how "none", an earlier and invalid guess at this
// value, was ruled out).
const platformAny = "any"

// Options is the two decisions the console ever asks for.
type Options struct {
	Intents   []string `json:"intents"`
	Platforms []string `json:"platforms"`
}

// AvailableOptions asks the live catalog which intents and platforms have at
// least one overlay for service, so /api/options honors spec §2's "filtered
// to those with an overlay matching this cluster's coordinates" instead of
// offering a static list that can dead-end. Empirically (task-10-report.md,
// pinned by internal/steps/recommend_test.go's
// TestRecommendResolvesAgainstSimulatedH100Fixture and
// TestRecommendKWOKGPUlessFixtureMatrix): only 5 of 12 (intent, platform)
// pairs resolve on a service=kind cluster. The other 7 fail because the
// catalog has no service=kind overlay combining that intent and platform at
// all -- a permanent, catalog-shaped gap, independent of accelerator -- and
// offering them anyway would be a guaranteed dead end in the wizard.
//
// service should be the fingerprint-derived Criteria.Service of the
// cluster's own snapshot (see ServiceFromSnapshot), known only once Discover
// has completed for some run. Pass "" before that -- including before the
// very first run ever starts: ListCatalog's filter treats an empty Service
// as unconstrained (see pkg/recipe/catalog.go's matchesCatalogFilter), so
// the result widens to catalog-wide coverage. That is still a real,
// catalog-verified answer -- a platform with zero overlays anywhere in the
// whole catalog (e.g. "runai" in the embedded v0.19.0 catalog, verified via
// a throwaway diagnostic against the real client during this task) is still
// excluded -- just not yet narrowed to this specific cluster. Recommend
// itself remains the backstop: it fails loudly and specifically (see
// internal/steps/recommend.go) if a pre-Discover-widened option turns out
// not to fit the cluster Discover eventually finds.
//
// The candidate intents come from client.CriteriaRegistry().AllIntentTypes(),
// itself seeded from the loaded catalog, not a literal in this file.
// Platforms are read off the Criteria.Platform of whatever overlays
// ListCatalog actually returns for service+intent, rather than probed
// against a hardcoded platform list. Both stay correct across a catalog
// change (a new overlay, a retired platform) with no edit here -- exactly
// the "do not duplicate catalog knowledge" requirement this function exists
// to satisfy.
func AvailableOptions(ctx context.Context, client API, service string) (Options, error) {
	reg := client.CriteriaRegistry()
	if reg == nil {
		return Options{}, aicrerrors.New(aicrerrors.ErrCodeUnavailable, "criteria registry unavailable")
	}

	intents := make(map[string]struct{})
	platforms := make(map[string]struct{})

	for _, intent := range reg.AllIntentTypes() {
		entries, err := client.ListCatalog(ctx, &aicr.Criteria{Service: service, Intent: intent})
		if err != nil {
			return Options{}, aicrerrors.PropagateOrWrap(err, aicrerrors.ErrCodeUnavailable, "catalog lookup failed")
		}
		for _, e := range entries {
			intents[intent] = struct{}{}
			platform := e.Criteria.Platform
			if platform == "" {
				platform = platformAny
			}
			platforms[platform] = struct{}{}
		}
	}

	return Options{Intents: sortedKeys(intents), Platforms: sortedKeys(platforms)}, nil
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ServiceFromSnapshot derives just the Criteria.Service dimension from raw
// snapshot bytes, the same way steps.Recommend derives its full criteria:
// fingerprint.FromMeasurements(...).ToCriteria(reg) (see
// internal/steps/recommend.go's buildCriteria). The derivation is
// duplicated narrowly here rather than exported from steps, because
// /api/options needs only this one dimension and steps' decodeSnapshot and
// buildCriteria are private to that package's Step implementation --
// exporting them would widen steps' surface for a caller outside the run
// pipeline.
//
// Returns ("", nil) for an empty snapshot (nothing collected yet, not an
// error) and a wrapped error for one present but unparseable, so a caller
// can choose to degrade AvailableOptions to its unconstrained default
// rather than fail the whole request either way.
func ServiceFromSnapshot(client API, raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var snap snapshotter.Snapshot
	if err := yaml.Unmarshal(raw, &snap); err != nil {
		return "", aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "stored snapshot is unparseable", err)
	}

	reg := client.CriteriaRegistry()
	if reg == nil {
		// Mirrors buildCriteria's own fallback in internal/steps/recommend.go:
		// client.CriteriaRegistry() only returns nil for a nil Client, but
		// Fake's zero value also returns nil, and fp.ToCriteria would need a
		// non-nil registry.
		reg = recipe.NewCriteriaRegistry()
	}

	fp := fingerprint.FromMeasurements(snap.Measurements)
	criteria := fp.ToCriteria(reg)
	return string(criteria.Service), nil
}
