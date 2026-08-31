// Package aicrclient narrows the AICR facade to the operations the console
// uses, so every step is testable with a fake and the console's dependency on
// the pinned aicr module is visible in one file.
package aicrclient

import (
	"context"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
)

// AICRVersion is the version of github.com/NVIDIA/aicr this console is built
// against. `make check-aicr-pin` keeps it equal to .settings.yaml, go.mod, and
// the snapshot agent image tag.
//
// IT MUST BE AICR'S VERSION, NEVER THIS CONSOLE'S. AICR uses whatever
// WithVersion is given to rewrite its validator catalog's `:latest` images to a
// release tag (pkg/validator/catalog.ResolveImage), so passing aicrme's version
// makes every validator Job pull
// ghcr.io/nvidia/aicr-validators/deployment:<aicrme tag> -- an image that does
// not exist. Measured on real H100s 2026-08-29, one hour after aicrme's first
// tagged release: all five deployment validators ImagePullBackOff'd, each burned
// its full per-check deadline, and validation reported 0 of 5 passed after 24
// minutes of nothing.
//
// It was invisible before that release because AICR only rewrites the tag when
// the version LOOKS like a release (`^v?\d+\.\d+\.\d+`). Every dev build
// passed "dev" or a git SHA, which it ignores, leaving `:latest` to pull fine.
const AICRVersion = "v0.20.0"

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

// CatalogResolver resolves a recipe from criteria alone, with no cluster
// snapshot. That is the property Clear's survey needs: it runs at Connect,
// before any run exists, and must learn which charts AICR could install
// without first collecting a snapshot of the cluster it is about to describe.
type CatalogResolver interface {
	ResolveRecipeFromCriteria(ctx context.Context, criteria *aicr.Criteria) (*aicr.RecipeResult, error)
}

// Bundler generates the on-disk deployer bundle for a resolved recipe. Kept
// as its own single-method interface for the same reason as the others: a
// step that only bundles should not be able to collect a snapshot.
type Bundler interface {
	MakeBundle(ctx context.Context, r *aicr.RecipeResult, opts aicr.BundleOptions) (aicr.BundleArtifact, error)
}

// Validator runs the recipe's validation phases against the live cluster.
//
// A role interface of its own, like every other seam here, rather than a
// method bolted onto an existing one: Validate is the only caller, and a
// step that needs to validate should not have to accept a bundler.
type Validator interface {
	ValidateState(ctx context.Context, r *aicr.RecipeResult, s *aicr.Snapshot,
		opts ...aicr.ValidateOption) ([]*aicr.PhaseResult, error)
}

// API is the whole console-facing AICR surface.
type API interface {
	Snapshotter
	Resolver
	CriteriaRegistrar
	CatalogLister
	CatalogResolver
	Bundler
	Validator
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
		aicr.WithVersion(AICRVersion),
	)
}
