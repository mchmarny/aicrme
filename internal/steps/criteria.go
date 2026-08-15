package steps

// Criteria derivation is shared by Recommend and Bundle. Bundle re-resolves
// the recipe rather than receiving a *aicr.RecipeResult handle from
// Recommend (MakeBundle requires a Client-owned one, which cannot travel
// through Run.Artifacts' []byte values), so both steps must derive criteria
// identically or the re-resolve could silently produce a different recipe
// than the one the user approved. steps.Bundle's assertMatchesApproved is
// the backstop that proves they did not.

import (
	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/fingerprint"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/mchmarny/aicrme/internal/aicrclient"
	"gopkg.in/yaml.v3"
)

// criteriaAny is the sentinel AICR's recipe package uses for an unset
// criteria dimension (pkg/recipe.CriteriaServiceAny et al. all stringify to
// this). Duplicated here as a plain string, rather than importing the typed
// constants, because facade Criteria fields are plain strings, not the
// recipe package's enum types.
const criteriaAny = "any"

// decodeSnapshot rebuilds the facade Snapshot from the raw agent bytes
// Discover stored verbatim in Run.Artifacts["snapshot.yaml"]. Discover always
// runs before Recommend in the wired pipeline, so a missing artifact means
// something upstream broke -- Recommend fails loudly here rather than
// silently degrading to a criteria-only resolve. Two reasons: the real
// aicr.Client already rejects a nil snapshot outright ("snapshot is required
// (got nil)"), so silently proceeding would only defer this failure past
// every unit test in this file (aicrclient.Fake does not validate its
// arguments) to the first real run; and a criteria-only resolve is exactly
// the zero-specificity, generic-fallback failure mode AICR issue #1888
// warns about.
//
// The reconstruction itself must go through snapshotter.Snapshot and
// aicr.WrapSnapshot, not a bare &aicr.Snapshot{Raw: raw} literal: Snapshot's
// measurement payload lives in an unexported field WrapSnapshot is the only
// way to populate, and a Raw-only literal parses without error while
// Unwrap() silently yields zero measurements.
func decodeSnapshot(raw []byte) (*aicr.Snapshot, error) {
	if len(raw) == 0 {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			"snapshot.yaml artifact is missing -- Discover must run before Recommend")
	}
	var inner snapshotter.Snapshot
	if err := yaml.Unmarshal(raw, &inner); err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "stored snapshot is unparseable", err)
	}
	wrapped := aicr.WrapSnapshot(&inner)
	wrapped.Raw = raw
	return wrapped, nil
}

// buildCriteria derives Service, Accelerator, OS, and Nodes from the
// snapshot's own measurements via AICR's fingerprint package, then overlays
// the user's intent and platform decisions on top. This mirrors what AICR's
// own CLI does for `aicr recipe --snapshot`
// (pkg/cli/query.go: `fingerprint.FromMeasurements(snap.Measurements).ToCriteria(reg)`,
// then CLI-flag criteria layered on top via applyCriteriaOverrides) rather
// than reimplementing that mapping: intent and platform are, in
// fingerprint.Fingerprint.ToCriteria's own words, "recipe-author choices the
// cluster cannot reveal" -- they always come back "any" from the fingerprint
// alone, so this console's two decisions are the only way they are ever set.
//
// Everything else -- service, accelerator, OS, node count -- comes from
// whatever the snapshot's measurements actually contain. On a cluster with
// no GPU hardware (e.g. the KWOK demo path), the accelerator and OS
// dimensions may come back unset: a real GPU cannot be invented from a
// snapshot that never observed one, and ResolveRecipeFromSnapshot will
// reject the resulting criteria if the catalog's overlays require an
// accelerator this combination doesn't supply. That is a limitation of the
// hardware being simulated, not of this derivation --
// TestRecommendKWOKGPUlessFixtureMatrix and
// TestRecommendResolvesAgainstSimulatedH100Fixture pin both sides of that
// line against real fixtures.
func buildCriteria(client aicrclient.CriteriaRegistrar, snap *aicr.Snapshot, intent, platform string) (*aicr.Criteria, error) {
	reg := client.CriteriaRegistry()
	if reg == nil {
		// Client.CriteriaRegistry() only returns nil for a nil Client; kept
		// here because aicrclient.Fake's zero value also returns nil, and
		// reg.ParseIntent/ParsePlatform below would panic on a nil receiver
		// for any value that isn't one of the hardcoded fast-path strings
		// (unlike fingerprint.Fingerprint.ToCriteria, which already guards
		// this internally).
		reg = recipe.NewCriteriaRegistry()
	}

	fp := fingerprint.FromMeasurements(snap.Unwrap().Measurements)
	criteria := aicr.WrapCriteria(fp.ToCriteria(reg))

	parsedIntent, err := reg.ParseIntent(intent)
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "invalid intent", err)
	}
	parsedPlatform, err := reg.ParsePlatform(platform)
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "invalid platform", err)
	}
	criteria.Intent = string(parsedIntent)
	criteria.Platform = string(parsedPlatform)
	return criteria, nil
}

// specificity counts how many of criteria's six dimensions are stated (not
// blank and not the "any" wildcard) -- the facade-side equivalent of
// pkg/recipe.Criteria.Specificity(), which is unavailable here because the
// facade Criteria carries plain strings, not that package's enum types.
func specificity(c *aicr.Criteria) int {
	n := 0
	for _, v := range []string{c.Service, c.Accelerator, c.Intent, c.OS, c.Platform} {
		if v != "" && v != criteriaAny {
			n++
		}
	}
	if c.Nodes != 0 {
		n++
	}
	return n
}
