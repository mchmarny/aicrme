package aicrclient_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/mchmarny/aicrme/internal/aicrclient"
)

func cacheFake() *aicrclient.Fake {
	return &aicrclient.Fake{
		Registry: recipe.NewCriteriaRegistry(),
		CatalogEntries: []aicr.CatalogEntry{
			{Name: "a", Criteria: aicr.Criteria{Platform: "kubeflow"}},
		},
	}
}

// TestOptionsCacheComputesOnceForTheSameSnapshot is the whole point of the
// cache: /api/options is fetched repeatedly during a run (at minimum once on
// mount and again at awaiting_decision), and the answer cannot change while
// the snapshot does not, so the catalog probe must not run again.
func TestOptionsCacheComputesOnceForTheSameSnapshot(t *testing.T) {
	fake := cacheFake()
	var cache aicrclient.OptionsCache

	first, err := cache.Get(context.Background(), fake, nil)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	afterFirst := fake.CatalogCalls
	if afterFirst == 0 {
		t.Fatal("CatalogCalls = 0, want the first Get to actually compute")
	}

	second, err := cache.Get(context.Background(), fake, nil)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if fake.CatalogCalls != afterFirst {
		t.Errorf("CatalogCalls = %d, want %d -- the second Get must be a cache hit",
			fake.CatalogCalls, afterFirst)
	}
	if !equalStrings(first.Platforms, second.Platforms) {
		t.Errorf("cached Platforms = %v, want %v", second.Platforms, first.Platforms)
	}
	if first.Provisional != second.Provisional {
		t.Errorf("cached Provisional = %v, want %v", second.Provisional, first.Provisional)
	}
}

// TestOptionsCacheRecomputesWhenTheSnapshotChanges pins the invalidation
// rule. Without it the console would keep serving the pre-Discover
// provisional answer for the rest of the run -- exactly the stale-cache
// failure the Provisional contract tells clients to avoid.
func TestOptionsCacheRecomputesWhenTheSnapshotChanges(t *testing.T) {
	fake := cacheFake()
	var cache aicrclient.OptionsCache

	before, err := cache.Get(context.Background(), fake, nil)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !before.Provisional {
		t.Fatal("Provisional = false, want true with no snapshot")
	}
	afterFirst := fake.CatalogCalls

	after, err := cache.Get(context.Background(), fake, loadH100Raw(t))
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if fake.CatalogCalls == afterFirst {
		t.Error("CatalogCalls unchanged, want a recompute for a different snapshot")
	}
	if after.Provisional {
		t.Error("Provisional = true, want false once a real snapshot arrives")
	}
}

// TestOptionsCacheResultIsNotAliased guards the cached entry against a caller
// that mutates what it was handed: without a deep copy, one handler sorting
// or appending to the returned slice would corrupt every later response.
func TestOptionsCacheResultIsNotAliased(t *testing.T) {
	fake := cacheFake()
	var cache aicrclient.OptionsCache

	first, err := cache.Get(context.Background(), fake, nil)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	want := append([]string(nil), first.Platforms...)

	first.Platforms[0] = "mutated"
	first.PlatformsByIntent["training"] = []string{"mutated"}
	first.Intents[0] = "mutated"

	second, err := cache.Get(context.Background(), fake, nil)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !equalStrings(second.Platforms, want) {
		t.Errorf("Platforms = %v, want %v -- caller mutation leaked into the cache", second.Platforms, want)
	}
	if containsString(second.PlatformsByIntent["training"], "mutated") {
		t.Errorf("PlatformsByIntent = %v, want the map value unshared", second.PlatformsByIntent)
	}
	if containsString(second.Intents, "mutated") {
		t.Errorf("Intents = %v, want the slice unshared", second.Intents)
	}
}

// TestOptionsCacheDoesNotCacheErrors keeps a transient catalog outage from
// becoming permanent: the next request must retry rather than be served a
// memoized failure.
func TestOptionsCacheDoesNotCacheErrors(t *testing.T) {
	fake := cacheFake()
	fake.CatalogErr = errors.New("catalog unavailable")
	var cache aicrclient.OptionsCache

	if _, err := cache.Get(context.Background(), fake, nil); err == nil {
		t.Fatal("Get() returned nil for a failing catalog")
	}

	fake.CatalogErr = nil
	got, err := cache.Get(context.Background(), fake, nil)
	if err != nil {
		t.Fatalf("Get() error = %v, want the outage not to be cached", err)
	}
	if len(got.Platforms) == 0 {
		t.Error("Platforms is empty, want the retry to have really recomputed")
	}
}

// TestOptionsCacheConcurrentGets exercises the cache under -race; concurrent
// /api/options requests are ordinary for a browser with two tabs open, and
// the two raws alternate so hits and misses interleave.
//
// This uses the real client rather than Fake: Fake's call counters are plain
// ints by design (it is a single-goroutine stub for step tests), so racing on
// it would report the stub's race instead of the cache's. *aicr.Client
// documents concurrent resolves as safe.
func TestOptionsCacheConcurrentGets(t *testing.T) {
	client, err := aicrclient.New()
	if err != nil {
		t.Fatalf("aicrclient.New() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	var cache aicrclient.OptionsCache
	raws := [][]byte{nil, loadH100Raw(t)}

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := cache.Get(context.Background(), client, raws[i%len(raws)])
			if err != nil {
				t.Errorf("Get() error = %v", err)
				return
			}
			if len(got.PlatformsByIntent) == 0 {
				t.Error("PlatformsByIntent is empty, want a real answer from either branch")
			}
		}(i)
	}
	wg.Wait()
}
