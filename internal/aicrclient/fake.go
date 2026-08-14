package aicrclient

import (
	"context"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
)

// Fake is a scripted API for step tests. It records call counts so tests can
// assert a step did not re-collect a snapshot it already had.
type Fake struct {
	Snapshot    *aicr.Snapshot
	Recipe      *aicr.RecipeResult
	Registry    *aicr.CriteriaRegistry
	SnapshotErr error
	ResolveErr  error

	SnapshotCalls int
	ResolveCalls  int
	LastCriteria  *aicr.Criteria
}

var _ API = (*Fake)(nil)

// CollectSnapshot records the call and returns the configured Snapshot, or a
// zero-value one when Snapshot is unset so callers can assert on a non-nil
// result without scripting every field.
func (f *Fake) CollectSnapshot(_ context.Context, _ *aicr.AgentConfig) (*aicr.Snapshot, error) {
	f.SnapshotCalls++
	if f.SnapshotErr != nil {
		return nil, f.SnapshotErr
	}
	if f.Snapshot == nil {
		return &aicr.Snapshot{}, nil
	}
	return f.Snapshot, nil
}

// ResolveRecipeFromSnapshot records the call and the criteria it was given,
// then returns the configured Recipe.
func (f *Fake) ResolveRecipeFromSnapshot(_ context.Context, c *aicr.Criteria, _ *aicr.Snapshot) (*aicr.RecipeResult, error) {
	f.ResolveCalls++
	f.LastCriteria = c
	if f.ResolveErr != nil {
		return nil, f.ResolveErr
	}
	return f.Recipe, nil
}

// CriteriaRegistry returns the configured Registry, which may be nil.
func (f *Fake) CriteriaRegistry() *aicr.CriteriaRegistry { return f.Registry }

// Close is a no-op; Fake holds no resources to release.
func (f *Fake) Close() error { return nil }
