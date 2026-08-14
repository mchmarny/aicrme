package steps_test

import (
	"context"
	"testing"

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

// TestAvailableOptionsMatchesRealResolution is the test Task 11 requires:
// one that fails if the options endpoint's offered set ever diverges from
// what Recommend actually resolves. It cross-validates
// aicrclient.AvailableOptions(ctx, client, "kind") -- exactly what
// /api/options computes once a run's snapshot has been fingerprinted to
// service=kind -- against live steps.NewRecommend resolution for every
// (intent, platform) pair, using the same real client and the same
// simulated-H100 KWOK fixture as
// TestRecommendResolvesAgainstSimulatedH100Fixture. Two invariants:
//
//  1. Every platform AvailableOptions offers actually resolves for at least
//     one intent (no dead options -- the whole point of this task).
//  2. Every (intent, platform) pair that actually resolves has its platform
//     in the offered set (no hiding a real option; the intent/platform
//     split of the offered set is coarser than the full pairwise matrix by
//     construction, see AvailableOptions's own doc comment, so this checks
//     the platform side of that invariant, not full pairwise coverage).
func TestAvailableOptionsMatchesRealResolution(t *testing.T) {
	client, err := aicrclient.New()
	if err != nil {
		t.Fatalf("aicrclient.New() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	snap := loadH100Snapshot(t)

	opts, err := aicrclient.AvailableOptions(context.Background(), client, "kind")
	if err != nil {
		t.Fatalf("AvailableOptions() error = %v", err)
	}

	offeredPlatform := func(p string) bool {
		for _, v := range opts.Platforms {
			if v == p {
				return true
			}
		}
		return false
	}
	offeredIntent := func(i string) bool {
		for _, v := range opts.Intents {
			if v == i {
				return true
			}
		}
		return false
	}

	resolves := func(intent, platform string) bool {
		step := steps.NewRecommend(client)
		run := newRun()
		run.Decisions["intent"] = intent
		run.Decisions["platform"] = platform
		run.Artifacts["snapshot.yaml"] = snap.Raw
		return step.Run(context.Background(), run, func(bus.Event) {}) == nil
	}

	for _, intent := range kwokIntents {
		for _, platform := range kwokPlatforms {
			if !resolves(intent, platform) {
				continue
			}
			if !offeredPlatform(platform) {
				t.Errorf("intent=%s platform=%s resolves for real, but AvailableOptions did not offer platform %q",
					intent, platform, platform)
			}
			if !offeredIntent(intent) {
				t.Errorf("intent=%s platform=%s resolves for real, but AvailableOptions did not offer intent %q",
					intent, platform, intent)
			}
		}
	}

	for _, platform := range opts.Platforms {
		var anyResolves bool
		for _, intent := range kwokIntents {
			if resolves(intent, platform) {
				anyResolves = true
				break
			}
		}
		if !anyResolves {
			t.Errorf("AvailableOptions offered platform %q but no intent actually resolves it -- a dead option", platform)
		}
	}
}
