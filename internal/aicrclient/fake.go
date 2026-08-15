package aicrclient

import (
	"context"
	"os"

	"github.com/NVIDIA/aicr/pkg/bundler/result"
	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
)

// Fake is a scripted API for step tests. It records call counts so tests can
// assert a step did not re-collect a snapshot it already had.
type Fake struct {
	Snapshot       *aicr.Snapshot
	Recipe         *aicr.RecipeResult
	Registry       *aicr.CriteriaRegistry
	CatalogEntries []aicr.CatalogEntry
	SnapshotErr    error
	ResolveErr     error
	CatalogErr     error

	SnapshotCalls     int
	ResolveCalls      int
	CatalogCalls      int
	LastCriteria      *aicr.Criteria
	LastAgentConfig   *aicr.AgentConfig
	LastCatalogFilter *aicr.Criteria

	Artifact  aicr.BundleArtifact
	BundleErr error

	BundleCalls   int
	LastBundleDir string
}

var _ API = (*Fake)(nil)

// CollectSnapshot records the call and the AgentConfig it was given, then
// returns the configured Snapshot, or a zero-value one when Snapshot is unset
// so callers can assert on a non-nil result without scripting every field.
// LastAgentConfig lets a test verify a step actually plumbed its
// DiscoverConfig through — namespace, image, timeout, privileged, and
// require-GPU are otherwise invisible to a fake that only returns a
// pre-scripted Snapshot regardless of what it was asked to collect.
func (f *Fake) CollectSnapshot(_ context.Context, cfg *aicr.AgentConfig) (*aicr.Snapshot, error) {
	f.SnapshotCalls++
	f.LastAgentConfig = cfg
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

// ListCatalog records the call and the filter it was given, then returns the
// configured CatalogEntries.
func (f *Fake) ListCatalog(_ context.Context, filter *aicr.Criteria) ([]aicr.CatalogEntry, error) {
	f.CatalogCalls++
	f.LastCatalogFilter = filter
	if f.CatalogErr != nil {
		return nil, f.CatalogErr
	}
	return f.CatalogEntries, nil
}

// Close is a no-op; Fake holds no resources to release.
func (f *Fake) Close() error { return nil }

// MakeBundle records the call and the OutputDir it was given, then returns
// the configured Artifact. When Artifact is unset it returns a zero-value
// one so callers can assert on a non-nil result without scripting it, and
// it creates OutputDir so a caller that then stats the directory sees what
// a real bundle run would have left behind.
func (f *Fake) MakeBundle(_ context.Context, _ *aicr.RecipeResult, opts aicr.BundleOptions) (aicr.BundleArtifact, error) {
	f.BundleCalls++
	f.LastBundleDir = opts.OutputDir
	if f.BundleErr != nil {
		return nil, f.BundleErr
	}
	if opts.OutputDir != "" {
		if err := os.MkdirAll(opts.OutputDir, 0o755); err != nil {
			return nil, err
		}
	}
	if f.Artifact == nil {
		return &result.Output{OutputDir: opts.OutputDir}, nil
	}
	return f.Artifact, nil
}
