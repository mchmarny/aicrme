package aicrclient_test

import (
	"context"
	"errors"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"

	"github.com/mchmarny/aicrme/internal/aicrclient"
)

// TestNewUsesEmbeddedCatalog proves EmbeddedSource() genuinely loads recipe
// data with no recipes/ directory on disk. CriteriaRegistry() alone is not
// enough: aicr.Client.CriteriaRegistry lazily returns a non-nil-but-empty
// registry immediately after NewClient, regardless of whether any catalog
// data has ever been walked — only a resolve (or the LoadCatalog method this
// console-facing API deliberately does not expose) walks the embedded
// overlays and populates the catalog. So this test also lists the catalog
// through the concrete client and asserts it is non-empty and embedded, which
// only a real load of the catalog packaged into the aicr module can produce.
func TestNewUsesEmbeddedCatalog(t *testing.T) {
	client, err := aicrclient.New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if client.CriteriaRegistry() == nil {
		t.Error("CriteriaRegistry() returned nil — embedded catalog did not load")
	}

	real, ok := client.(*aicr.Client)
	if !ok {
		t.Fatalf("New() returned %T, want *aicr.Client", client)
	}

	entries, err := real.ListCatalog(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListCatalog() error = %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("ListCatalog() returned no entries — embedded catalog did not load")
	}

	var sawEmbedded bool
	for _, e := range entries {
		if e.Source == aicr.CatalogSourceEmbedded {
			sawEmbedded = true
			break
		}
	}
	if !sawEmbedded {
		t.Errorf("ListCatalog() entries carry no %q source — embedded catalog did not load", aicr.CatalogSourceEmbedded)
	}
}

func TestFakeSatisfiesAPI(t *testing.T) {
	var api aicrclient.API = &aicrclient.Fake{}
	if _, err := api.CollectSnapshot(context.Background(), nil); err != nil {
		t.Errorf("Fake.CollectSnapshot() error = %v", err)
	}
}

func TestFakeCollectSnapshot(t *testing.T) {
	wantErr := errors.New("boom")
	wantSnapshot := &aicr.Snapshot{APIVersion: "v1"}

	tests := []struct {
		name     string
		fake     *aicrclient.Fake
		wantErr  error
		wantNil  bool
		wantSelf *aicr.Snapshot
	}{
		{
			name:    "default snapshot when unset",
			fake:    &aicrclient.Fake{},
			wantNil: false,
		},
		{
			name:     "configured snapshot returned as-is",
			fake:     &aicrclient.Fake{Snapshot: wantSnapshot},
			wantSelf: wantSnapshot,
		},
		{
			name:    "configured error short-circuits",
			fake:    &aicrclient.Fake{SnapshotErr: wantErr},
			wantErr: wantErr,
			wantNil: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.fake.CollectSnapshot(context.Background(), nil)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantNil && got != nil {
				t.Fatalf("got = %v, want nil", got)
			}
			if !tc.wantNil && got == nil {
				t.Fatal("got = nil, want non-nil")
			}
			if tc.wantSelf != nil && got != tc.wantSelf {
				t.Fatalf("got = %v, want %v", got, tc.wantSelf)
			}
			if tc.fake.SnapshotCalls != 1 {
				t.Errorf("SnapshotCalls = %d, want 1", tc.fake.SnapshotCalls)
			}
		})
	}
}

func TestFakeCollectSnapshotCapturesAgentConfig(t *testing.T) {
	f := &aicrclient.Fake{}
	cfg := &aicr.AgentConfig{Namespace: "aicrme", Image: "ghcr.io/nvidia/aicr:v1", Privileged: true, RequireGPU: true}

	if _, err := f.CollectSnapshot(context.Background(), cfg); err != nil {
		t.Fatalf("CollectSnapshot() error = %v", err)
	}
	if f.LastAgentConfig != cfg {
		t.Fatalf("LastAgentConfig = %v, want %v", f.LastAgentConfig, cfg)
	}
}

func TestFakeResolveRecipeFromSnapshot(t *testing.T) {
	wantErr := errors.New("boom")
	wantRecipe := &aicr.RecipeResult{}
	wantCriteria := &aicr.Criteria{Service: "eks"}

	tests := []struct {
		name    string
		fake    *aicrclient.Fake
		wantErr error
		want    *aicr.RecipeResult
	}{
		{
			name: "returns configured recipe",
			fake: &aicrclient.Fake{Recipe: wantRecipe},
			want: wantRecipe,
		},
		{
			name:    "configured error short-circuits",
			fake:    &aicrclient.Fake{ResolveErr: wantErr},
			wantErr: wantErr,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.fake.ResolveRecipeFromSnapshot(context.Background(), wantCriteria, nil)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("got = %v, want %v", got, tc.want)
			}
			if tc.fake.LastCriteria != wantCriteria {
				t.Errorf("LastCriteria = %v, want %v", tc.fake.LastCriteria, wantCriteria)
			}
			if tc.fake.ResolveCalls != 1 {
				t.Errorf("ResolveCalls = %d, want 1", tc.fake.ResolveCalls)
			}
		})
	}
}

func TestFakeCriteriaRegistryAndClose(t *testing.T) {
	f := &aicrclient.Fake{}
	if got := f.CriteriaRegistry(); got != nil {
		t.Errorf("CriteriaRegistry() = %v, want nil for zero-value Fake", got)
	}
	if err := f.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
}

// The Fake must satisfy the same aggregate the real client does, or a step
// written against API cannot be unit tested at all.
func TestFakeRecordsValidateCalls(t *testing.T) {
	want := []*aicr.PhaseResult{{Phase: aicr.PhaseDeployment, Status: "passed"}}
	f := &aicrclient.Fake{PhaseResults: want}

	got, err := f.ValidateState(context.Background(), nil, nil,
		aicr.WithValidationPhases(aicr.PhaseDeployment))
	if err != nil {
		t.Fatalf("ValidateState() error = %v", err)
	}
	if len(got) != 1 || got[0].Status != "passed" {
		t.Errorf("ValidateState() = %+v, want the configured results", got)
	}
	if f.ValidateCalls != 1 {
		t.Errorf("ValidateCalls = %d, want 1", f.ValidateCalls)
	}
	if len(f.LastValidateOpts) != 1 {
		t.Errorf("LastValidateOpts = %d, want the options recorded for assertion", len(f.LastValidateOpts))
	}
}
