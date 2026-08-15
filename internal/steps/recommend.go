package steps

import (
	"context"
	"encoding/json"
	"fmt"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/fingerprint"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"gopkg.in/yaml.v3"
)

// criteriaAny is the sentinel AICR's recipe package uses for an unset
// criteria dimension (pkg/recipe.CriteriaServiceAny et al. all stringify to
// this). Duplicated here as a plain string, rather than importing the typed
// constants, because facade Criteria fields are plain strings, not the
// recipe package's enum types.
const criteriaAny = "any"

// ComponentSummary is one reviewable component in the resolved recipe.
type ComponentSummary struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Version   string `json:"version"`
	Namespace string `json:"namespace"`
	Chart     string `json:"chart,omitempty"`
	Source    string `json:"source,omitempty"`
}

// RecipeSummary is the folded component list shown on the Recommend screen.
type RecipeSummary struct {
	Name           string             `json:"name"`
	Version        string             `json:"version"`
	ComponentCount int                `json:"componentCount"`
	Components     []ComponentSummary `json:"components"`
}

type recommend struct {
	client aicrclient.API
}

// NewRecommend returns the Recommend step. It gates on the only two decisions
// the console asks for: intent and platform. Service, accelerator, and OS are
// derived from the snapshot's own fingerprint (see buildCriteria); component
// set, versions, and values are all then derived by AICR from the resolved
// recipe.
func NewRecommend(c aicrclient.API) engine.Step {
	return &recommend{client: c}
}

func (r *recommend) Phase() engine.Phase { return engine.PhaseRecommend }
func (r *recommend) Requires() []string  { return []string{"intent", "platform"} }

func (r *recommend) Run(ctx context.Context, run *engine.Run, emit engine.Emit) error {
	snap, err := decodeSnapshot(run.Artifacts["snapshot.yaml"])
	if err != nil {
		return err
	}

	// Requires() only gates key presence in Run.Decisions, not that the
	// values are non-blank (see engine.Engine.awaitDecisions). AICR's facade
	// does not itself reject zero-specificity criteria -- resolving
	// &aicr.Criteria{} against a real snapshot succeeds and returns a
	// generic, non-representative fallback recipe instead of erroring
	// (reproduced against the embedded v0.19.0 catalog; AICR issue #1888). A
	// blank intent or platform would silently reproduce exactly that failure
	// mode, so Recommend guards it here rather than trusting the facade to.
	intent := run.Decisions["intent"]
	platform := run.Decisions["platform"]
	if intent == "" || platform == "" {
		return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			"intent and platform decisions must be non-empty")
	}

	criteria, err := buildCriteria(r.client, snap, intent, platform)
	if err != nil {
		return err
	}

	// Belt-and-suspenders alongside the blank-decision check above: that
	// check catches an empty string, not an explicit "any" (or a snapshot
	// that fingerprints to nothing at all). AICR's facade does not itself
	// guard against zero-specificity criteria: resolving one succeeds with a
	// generic, non-representative fallback recipe instead of erroring (AICR
	// issue #1888; TestRecommendFailsOnZeroSpecificityCriteria pins this
	// console's behavior). Recommend fails closed here the way AICR's own
	// CLI does in pkg/cli/query.go, rather than trusting the facade to.
	if specificity(criteria) == 0 {
		return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			"criteria(any): the snapshot and the intent/platform decisions identify no service, "+
				"accelerator, os, or platform to resolve against")
	}

	emit(bus.Event{Kind: bus.KindLog, Message: fmt.Sprintf(
		"resolving recipe for intent=%s platform=%s (snapshot-derived service=%s accelerator=%s os=%s nodes=%d)",
		criteria.Intent, criteria.Platform, criteria.Service, criteria.Accelerator, criteria.OS, criteria.Nodes)})

	result, err := r.client.ResolveRecipeFromSnapshot(ctx, criteria, snap)
	if err != nil {
		return aicrerrors.PropagateOrWrap(err, aicrerrors.ErrCodeInvalidRequest, "recipe resolution failed")
	}
	if result == nil {
		return aicrerrors.New(aicrerrors.ErrCodeInternal, "recipe resolution returned no result")
	}

	summary := RecipeSummary{
		Name:           result.Name,
		Version:        result.Version,
		ComponentCount: len(result.Components),
		// Non-nil even for a zero-component recipe: a nil slice marshals to
		// `"components":null`, and the SPA types this field as an array and
		// maps over it unguarded (web/src/components/Wizard.tsx's
		// ResolvedRecommend), so a null blanks the screen.
		Components: make([]ComponentSummary, 0, len(result.Components)),
	}
	for _, c := range result.Components {
		summary.Components = append(summary.Components, ComponentSummary{
			Name: c.Name, Kind: c.Kind, Version: c.Version,
			Namespace: c.Namespace, Chart: c.Chart, Source: c.Source,
		})
	}

	encoded, err := json.Marshal(summary)
	if err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "encoding recipe summary failed", err)
	}
	run.Artifacts["recipe.json"] = encoded

	emit(bus.Event{
		Kind: bus.KindLog, Data: encoded,
		Message: fmt.Sprintf("%d components, every version pinned", summary.ComponentCount),
	})
	return nil
}

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
