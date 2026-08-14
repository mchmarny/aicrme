// Package aicrclient narrows the AICR facade to the operations the console
// uses, so every step is testable with a fake and the console's dependency on
// the pinned aicr module is visible in one file.
package aicrclient

import (
	"context"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"

	"github.com/mchmarny/aicrme/internal/version"
)

// Snapshotter deploys the AICR snapshot agent Job and returns the result.
type Snapshotter interface {
	CollectSnapshot(ctx context.Context, cfg *aicr.AgentConfig) (*aicr.Snapshot, error)
}

// Resolver turns a snapshot plus user criteria into a pinned recipe.
type Resolver interface {
	ResolveRecipeFromSnapshot(ctx context.Context, c *aicr.Criteria, s *aicr.Snapshot) (*aicr.RecipeResult, error)
}

// CriteriaRegistrar exposes the recipe catalog's criteria registry, used to
// filter the platform options offered to the user.
type CriteriaRegistrar interface {
	CriteriaRegistry() *aicr.CriteriaRegistry
}

// CatalogLister lists the recipe catalog's overlays, optionally narrowed by
// criteria. This is what makes /api/options honest: querying the live
// catalog for which (intent, platform) pairs have an overlay for a given
// cluster's coordinates, rather than offering a static list that can
// dead-end (see options.go's AvailableOptions).
type CatalogLister interface {
	ListCatalog(ctx context.Context, filter *aicr.Criteria) ([]aicr.CatalogEntry, error)
}

// API is the whole console-facing AICR surface.
type API interface {
	Snapshotter
	Resolver
	CriteriaRegistrar
	CatalogLister
	Close() error
}

// *aicr.Client satisfies API verbatim: CollectSnapshot, ResolveRecipeFromSnapshot,
// CriteriaRegistry, ListCatalog, and Close all match the signatures above exactly
// (confirmed against `go doc github.com/NVIDIA/aicr/pkg/client/v1 Client`), so no
// adapter is needed. This assertion makes a future AICR bump that renames or
// reshapes one of those methods fail to compile here rather than somewhere subtler.
var _ API = (*aicr.Client)(nil)

// New returns a client backed by the recipe catalog embedded in the pinned
// aicr module — no recipes/ tree ships in the console image.
func New() (API, error) {
	return aicr.NewClient(
		aicr.WithRecipeSource(aicr.EmbeddedSource()),
		aicr.WithVersion(version.Version),
	)
}
