package steps_test

import (
	"context"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/steps"
)

// TestServiceFromSnapshotDerivesKindFromTheH100Fixture confirms
// aicrclient.ServiceFromSnapshot -- what /api/options uses to turn a
// completed run's snapshot into a catalog filter -- actually derives
// "kind" from the same fixture TestRecommendResolvesAgainstSimulatedH100Fixture
// resolves against, rather than silently degrading to "" the way it does
// for a snapshot with no measurements at all.
func TestServiceFromSnapshotDerivesKindFromTheH100Fixture(t *testing.T) {
	client, err := aicrclient.New()
	if err != nil {
		t.Fatalf("aicrclient.New() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	snap := loadH100Snapshot(t)
	got, err := aicrclient.ServiceFromSnapshot(client, snap.Raw)
	if err != nil {
		t.Fatalf("ServiceFromSnapshot() error = %v", err)
	}
	if got != "kind" {
		t.Errorf("ServiceFromSnapshot() = %q, want %q", got, "kind")
	}
}

// crossCheckFixtures is both snapshots the repo has real resolution pins for.
//
// Running the cross-checks over both is what makes them bite. Both fingerprint
// to service=kind, so a catalog-only filter offers them the identical set --
// but the H100 fixture resolves 5 of 12 pairs and the GPU-less one resolves
// exactly 1 (inference/any), because every other service=kind overlay demands
// an accelerator a control-plane-only cluster does not have. Checking only the
// H100 fixture cannot distinguish "verified against this cluster" from
// "bucketed from the catalog"; checking the GPU-less one cannot be passed by
// any filter that ignores accelerator.
func crossCheckFixtures(t *testing.T) map[string]*aicr.Snapshot {
	t.Helper()
	return map[string]*aicr.Snapshot{
		"simulated-h100": loadH100Snapshot(t),
		"gpuless":        loadSnapshot(t),
	}
}

// resolvesFor answers the ground-truth question -- does this pair actually
// produce a recipe -- through the real Recommend step, the same code path a
// real run takes.
func resolvesFor(t *testing.T, client aicrclient.API, snap *aicr.Snapshot, intent, platform string) bool {
	t.Helper()
	step := steps.NewRecommend(client, nil)
	run := newRun()
	run.Decisions["intent"] = intent
	run.Decisions["platform"] = platform
	run.Artifacts["snapshot.yaml"] = snap.Raw
	return step.Run(context.Background(), run, func(bus.Event) {}) == nil
}

// TestAvailableOptionsMatchesRealResolution is the flat-list half of the
// cross-validation Task 11 requires: it fails if /api/options' offered
// intents/platforms ever diverge from what Recommend actually resolves.
// Two invariants, both directions:
//
//  1. Every platform offered resolves for at least one intent (no dead
//     option), and likewise every intent offered.
//  2. Every (intent, platform) pair that resolves has its platform and its
//     intent in the offered set (no hidden option).
//
// The flat lists are the union across intents by construction, so invariant 1
// is the strongest claim statable over them -- the pairwise claim lives in
// TestAvailableOptionsPlatformsByIntentMatchesRealResolution.
func TestAvailableOptionsMatchesRealResolution(t *testing.T) {
	client, err := aicrclient.New()
	if err != nil {
		t.Fatalf("aicrclient.New() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	for name, snap := range crossCheckFixtures(t) {
		t.Run(name, func(t *testing.T) {
			opts, err := aicrclient.AvailableOptions(context.Background(), client, snap.Raw)
			if err != nil {
				t.Fatalf("AvailableOptions() error = %v", err)
			}
			if opts.Provisional {
				t.Fatal("Provisional = true for a fixture that fingerprints to service=kind")
			}

			contains := func(hay []string, needle string) bool {
				for _, v := range hay {
					if v == needle {
						return true
					}
				}
				return false
			}

			for _, intent := range kwokIntents {
				for _, platform := range kwokPlatforms {
					if !resolvesFor(t, client, snap, intent, platform) {
						continue
					}
					if !contains(opts.Platforms, platform) {
						t.Errorf("intent=%s platform=%s resolves for real, but Platforms %v omits %q",
							intent, platform, opts.Platforms, platform)
					}
					if !contains(opts.Intents, intent) {
						t.Errorf("intent=%s platform=%s resolves for real, but Intents %v omits %q",
							intent, platform, opts.Intents, intent)
					}
				}
			}

			for _, platform := range opts.Platforms {
				var live bool
				for _, intent := range kwokIntents {
					if resolvesFor(t, client, snap, intent, platform) {
						live = true
						break
					}
				}
				if !live {
					t.Errorf("Platforms offers %q but no intent resolves it -- a dead option", platform)
				}
			}

			for _, intent := range opts.Intents {
				var live bool
				for _, platform := range kwokPlatforms {
					if resolvesFor(t, client, snap, intent, platform) {
						live = true
						break
					}
				}
				if !live {
					t.Errorf("Intents offers %q but no platform resolves it -- a dead option", intent)
				}
			}
		})
	}
}

// TestAvailableOptionsPlatformsByIntentMatchesRealResolution is the pairwise
// half, and the one that protects Task 12's Recommend wizard: it checks the
// exact set /api/options tells the UI to render, pair by pair, against live
// resolution. Every offered pair must resolve, and every resolving pair must
// be offered -- so neither a dead combination nor a hidden working one can
// survive a catalog change without failing CI.
func TestAvailableOptionsPlatformsByIntentMatchesRealResolution(t *testing.T) {
	client, err := aicrclient.New()
	if err != nil {
		t.Fatalf("aicrclient.New() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	for name, snap := range crossCheckFixtures(t) {
		t.Run(name, func(t *testing.T) {
			opts, err := aicrclient.AvailableOptions(context.Background(), client, snap.Raw)
			if err != nil {
				t.Fatalf("AvailableOptions() error = %v", err)
			}
			if opts.Provisional {
				t.Fatal("Provisional = true for a fixture that fingerprints to service=kind")
			}

			offered := func(intent, platform string) bool {
				for _, p := range opts.PlatformsByIntent[intent] {
					if p == platform {
						return true
					}
				}
				return false
			}

			for _, intent := range kwokIntents {
				for _, platform := range kwokPlatforms {
					wantResolves := resolvesFor(t, client, snap, intent, platform)
					gotOffered := offered(intent, platform)
					if gotOffered && !wantResolves {
						t.Errorf("intent=%s platform=%s: PlatformsByIntent offers this pair, but it does not resolve for real -- a dead pair",
							intent, platform)
					}
					if wantResolves && !gotOffered {
						t.Errorf("intent=%s platform=%s: resolves for real, but PlatformsByIntent does not offer it",
							intent, platform)
					}
				}
			}
		})
	}
}

// TestAvailableOptionsFlatListsAgreeWithPlatformsByIntent pins the structural
// invariant the two halves above assume: the flat lists are exactly the
// projections of the pairwise map, so a client reading either cannot be told
// two different stories.
func TestAvailableOptionsFlatListsAgreeWithPlatformsByIntent(t *testing.T) {
	client, err := aicrclient.New()
	if err != nil {
		t.Fatalf("aicrclient.New() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	for name, snap := range crossCheckFixtures(t) {
		t.Run(name, func(t *testing.T) {
			opts, err := aicrclient.AvailableOptions(context.Background(), client, snap.Raw)
			if err != nil {
				t.Fatalf("AvailableOptions() error = %v", err)
			}

			union := make(map[string]struct{})
			for intent, platforms := range opts.PlatformsByIntent {
				var found bool
				for _, i := range opts.Intents {
					if i == intent {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("PlatformsByIntent has intent %q that Intents %v omits", intent, opts.Intents)
				}
				for _, p := range platforms {
					union[p] = struct{}{}
				}
			}
			if len(union) != len(opts.Platforms) {
				t.Errorf("Platforms = %v, want the exact union of PlatformsByIntent %v",
					opts.Platforms, opts.PlatformsByIntent)
			}
			for _, p := range opts.Platforms {
				if _, ok := union[p]; !ok {
					t.Errorf("Platforms offers %q, absent from every PlatformsByIntent entry %v",
						p, opts.PlatformsByIntent)
				}
			}
		})
	}
}
