package steps

import (
	"context"
	"encoding/json"
	"fmt"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"gopkg.in/yaml.v3"
)

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
	client aicrclient.Resolver
}

// NewRecommend returns the Recommend step. It gates on the only two decisions
// the console asks for: intent and platform. Service, accelerator, OS,
// component set, versions, and values are all derived by AICR.
func NewRecommend(c aicrclient.Resolver) engine.Step {
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
	// does not itself reject zero-specificity criteria -- empirically,
	// resolving &aicr.Criteria{} against a real snapshot succeeds and
	// returns a generic, non-representative fallback recipe instead of
	// erroring (see task-10-report.md). A blank intent or platform would
	// silently reproduce exactly that failure mode, so Recommend guards it
	// here rather than trusting the facade to.
	intent := run.Decisions["intent"]
	platform := run.Decisions["platform"]
	if intent == "" || platform == "" {
		return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			"intent and platform decisions must be non-empty")
	}

	// Only intent and platform come from the user. Every other criteria
	// dimension is left unset here; AICR resolves the recipe from whatever
	// overlays the catalog covers for this combination and evaluates
	// constraints against the snapshot along the way.
	criteria := &aicr.Criteria{Intent: intent, Platform: platform}

	emit(bus.Event{Kind: bus.KindLog, Message: fmt.Sprintf(
		"resolving recipe for intent=%s platform=%s", criteria.Intent, criteria.Platform)})

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
// warns about (see task-10-report.md for the empirical reproduction).
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
