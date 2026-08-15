package steps

import (
	"context"
	"encoding/json"
	"fmt"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
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
