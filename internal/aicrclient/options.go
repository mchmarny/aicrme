package aicrclient

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"sort"
	"sync"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/fingerprint"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"gopkg.in/yaml.v3"
)

// platformAny mirrors the sentinel AICR's recipe package uses for an unset
// platform dimension (pkg/recipe.CriteriaPlatformAny stringifies to this).
// An overlay whose own Criteria.Platform is blank or this value applies
// regardless of the platform decision -- the "just the runtime, no specific
// platform" option (the product spec's fourth user-facing platform choice;
// see task-10-report.md for how "none", an earlier and invalid guess at this
// value, was ruled out).
const platformAny = "any"

// Options is the two decisions the console ever asks for.
type Options struct {
	// Intents and Platforms are the flattened projections of
	// PlatformsByIntent: Intents is its key set, Platforms the exact union
	// of its values. They answer "what values exist at all" and are
	// deliberately coarser than the pairwise truth -- a platform offered
	// for only one intent still appears in Platforms. A client that must
	// not offer a dead combination reads PlatformsByIntent instead.
	Intents   []string `json:"intents"`
	Platforms []string `json:"platforms"`

	// PlatformsByIntent[intent] is the set of platforms that pair with that
	// intent. When Provisional is false every listed pair has been verified
	// by actually resolving it against this cluster's own snapshot, so an
	// offered pair cannot dead-end in Recommend. When Provisional is true
	// the pairs are catalog-shaped candidates only -- an upper bound, see
	// Provisional.
	PlatformsByIntent map[string][]string `json:"platformsByIntent"`

	// Provisional reports that this answer was NOT verified against real
	// recipe resolution, because the cluster's own snapshot was not
	// available (or carried nothing the fingerprint could derive a service
	// from) at the time it was computed. A provisional answer is a widened,
	// catalog-wide upper bound: every genuinely resolvable pair is in it,
	// but so are pairs that will fail in Recommend -- most commonly because
	// the catalog's overlay for the pair demands an accelerator this
	// cluster does not have.
	//
	// A client MUST re-fetch /api/options once the run reaches
	// "awaiting_decision" rather than fetching once on mount and caching
	// the answer, and MUST NOT present a provisional set as final. Note
	// that "a run exists" is not proof of a verified answer: a snapshot
	// with no fingerprint-derivable service leaves Provisional true even
	// after Discover has completed.
	Provisional bool `json:"provisional"`
}

// AvailableOptions reports which (intent, platform) pairs this cluster can
// actually run, honoring spec §2's "filtered to those with an overlay
// matching this cluster's coordinates" instead of offering a static list
// that can dead-end.
//
// It works in two stages, and the split matters:
//
//  1. Candidates. For each intent the criteria registry knows,
//     ListCatalog(&Criteria{Service, Intent}) yields the overlays that exist,
//     and their own Criteria.Platform values (blank meaning "any") are the
//     candidate platforms. No platform list is hardcoded here, so a catalog
//     that gains or retires one stays correct with no edit.
//
//  2. Verification. Each candidate pair is then actually resolved against
//     rawSnapshot via the same ResolveRecipeFromSnapshot call steps.Recommend
//     makes. Only pairs that resolve are offered.
//
// Stage 2 is not redundant. matchesCatalogFilter (pkg/recipe/catalog.go) is
// exact equality on the dimensions the filter states and no constraint at all
// on the ones it omits, so a Service+Intent query returns overlays regardless
// of the accelerator and OS they demand. Concretely, on the embedded v0.19.0
// catalog, ListCatalog(service=kind, intent=training) returns
// h100-kind-training-kubeflow -- an overlay that requires accelerator=h100.
// Bucketing that entry by its Platform field claims "training+kubeflow works
// here", which is true on a KWOK cluster with simulated H100 nodes and false
// on a GPU-less one, where the fingerprint derives no accelerator and only
// inference+any resolves. "An overlay mentions this platform" and "this pair
// resolves" are different claims; only resolution proves the second.
//
// Constraining the query instead of verifying cannot substitute for stage 2:
// matchesCatalogFilter does no wildcard promotion, so adding accelerator=h100
// to the filter drops kind-inference (the accelerator-agnostic overlay) and
// would hide a pair that does resolve. Resolution is also cheap -- the whole
// candidate matrix is ~12 resolves at roughly 0.1ms each against the embedded
// catalog -- and needs no cluster, only the stored snapshot bytes.
//
// rawSnapshot is the run's snapshot.yaml artifact. Pass nil before Discover
// has produced one: with no cluster coordinate there is nothing to resolve
// against, so the result is the stage-1 candidate set alone and Provisional
// is set to say so. A rawSnapshot that cannot be parsed is treated the same
// way -- logged and degraded to that provisional set, never an error, because
// this endpoint asks the console's only two questions and must not go dark on
// a corrupt artifact.
func AvailableOptions(ctx context.Context, client API, rawSnapshot []byte) (Options, error) {
	reg := client.CriteriaRegistry()
	if reg == nil {
		return Options{}, aicrerrors.New(aicrerrors.ErrCodeUnavailable, "criteria registry unavailable")
	}

	var snap *aicr.Snapshot
	var base *aicr.Criteria
	var service string
	if len(rawSnapshot) > 0 {
		decoded, err := decodeSnapshot(rawSnapshot)
		if err != nil {
			// A corrupt snapshot must not brick the wizard. This endpoint
			// supplies the only two questions the console ever asks, so an
			// unreadable artifact degrades to the widened, unverified
			// candidate set (Provisional=true) instead of failing the
			// request. steps.Recommend still refuses the same bytes loudly
			// and specifically if the user proceeds -- the same division of
			// responsibility the rest of this design relies on, where the
			// run pipeline is the backstop and the options endpoint stays
			// available.
			slog.WarnContext(ctx,
				"options: snapshot unparseable, degrading to provisional catalog-wide options",
				"error", err, "snapshotBytes", len(rawSnapshot))
		} else {
			snap = decoded
			base = aicr.WrapCriteria(fingerprint.FromMeasurements(snap.Unwrap().Measurements).ToCriteria(reg))
			service = base.Service
		}
	}

	candidates, err := catalogCandidates(ctx, client, reg, service)
	if err != nil {
		return Options{}, err
	}

	// An unstated or wildcard service means the fingerprint found no
	// cluster coordinate to resolve against, so stage 2 has nothing to
	// verify with -- even if a snapshot was supplied.
	if service == "" || service == platformAny {
		return newOptions(candidates, true), nil
	}

	verified := make(map[string][]string, len(candidates))
	for intent, platforms := range candidates {
		for _, platform := range platforms {
			// A canceled context makes every remaining resolve fail,
			// which would silently shrink the offered set into a wrong
			// answer. Fail the whole call instead.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return Options{}, aicrerrors.Wrap(aicrerrors.ErrCodeTimeout,
					"catalog coverage probe canceled", ctxErr)
			}
			if pairResolves(ctx, client, reg, snap, base, intent, platform) {
				verified[intent] = append(verified[intent], platform)
			}
		}
	}
	return newOptions(verified, false), nil
}

// catalogCandidates buckets the overlays that exist for service by the intent
// they were queried under and the Platform they themselves carry. This is an
// upper bound on what can resolve, never a proof -- see AvailableOptions.
func catalogCandidates(
	ctx context.Context, client API, reg *aicr.CriteriaRegistry, service string,
) (map[string][]string, error) {

	candidates := make(map[string][]string)
	for _, intent := range reg.AllIntentTypes() {
		entries, err := client.ListCatalog(ctx, &aicr.Criteria{Service: service, Intent: intent})
		if err != nil {
			return nil, aicrerrors.PropagateOrWrap(err, aicrerrors.ErrCodeUnavailable, "catalog lookup failed")
		}
		seen := make(map[string]struct{}, len(entries))
		for _, e := range entries {
			platform := e.Criteria.Platform
			if platform == "" {
				platform = platformAny
			}
			seen[platform] = struct{}{}
		}
		if len(seen) > 0 {
			candidates[intent] = sortedKeys(seen)
		}
	}
	return candidates, nil
}

// pairResolves reports whether this (intent, platform) pair produces a real
// recipe for this snapshot -- the same question steps.Recommend answers, asked
// the same way.
//
// The criteria derivation is deliberately a second, independent
// implementation of steps.buildCriteria rather than a shared helper: making
// the options endpoint and the run pipeline agree is the job of the
// cross-check in internal/steps/options_cross_test.go, and sharing the code
// would make that test tautological instead of load-bearing.
//
// An unparseable intent or platform is not offerable, because Recommend
// rejects it at the same registry call, so a parse failure is a false result
// rather than an error.
func pairResolves(
	ctx context.Context,
	client API,
	reg *aicr.CriteriaRegistry,
	snap *aicr.Snapshot,
	base *aicr.Criteria,
	intent, platform string,
) bool {

	parsedIntent, err := reg.ParseIntent(intent)
	if err != nil {
		return false
	}
	parsedPlatform, err := reg.ParsePlatform(platform)
	if err != nil {
		return false
	}

	criteria := *base
	criteria.Intent = string(parsedIntent)
	criteria.Platform = string(parsedPlatform)

	result, err := client.ResolveRecipeFromSnapshot(ctx, &criteria, snap)
	return err == nil && result != nil
}

// newOptions derives the flat projections from the pairwise map so the two
// can never disagree: Platforms is exactly the union of the map's values.
func newOptions(platformsByIntent map[string][]string, provisional bool) Options {
	intents := make([]string, 0, len(platformsByIntent))
	union := make(map[string]struct{})
	for intent, platforms := range platformsByIntent {
		intents = append(intents, intent)
		for _, p := range platforms {
			union[p] = struct{}{}
		}
	}
	sort.Strings(intents)
	for _, platforms := range platformsByIntent {
		sort.Strings(platforms)
	}
	return Options{
		Intents:           intents,
		Platforms:         sortedKeys(union),
		PlatformsByIntent: platformsByIntent,
		Provisional:       provisional,
	}
}

func (o Options) clone() Options {
	out := o
	out.Intents = append([]string(nil), o.Intents...)
	out.Platforms = append([]string(nil), o.Platforms...)
	out.PlatformsByIntent = make(map[string][]string, len(o.PlatformsByIntent))
	for k, v := range o.PlatformsByIntent {
		out.PlatformsByIntent[k] = append([]string(nil), v...)
	}
	return out
}

// OptionsCache memoizes the most recent AvailableOptions result, keyed on the
// snapshot bytes it was computed from. The answer changes only when the
// snapshot does, so a single slot covers the access pattern this console
// actually has -- one run at a time, every /api/options request during that
// run asking about the same snapshot -- while staying bounded, unlike a map
// that would grow once per run for the process's lifetime.
//
// The zero value is ready to use. Safe for concurrent use.
type OptionsCache struct {
	mu     sync.Mutex
	key    [sha256.Size]byte
	opts   Options
	cached bool
}

// Get returns the options for rawSnapshot, computing them only on a miss.
// The result is a deep copy, so a caller cannot mutate the cached entry.
func (c *OptionsCache) Get(ctx context.Context, client API, rawSnapshot []byte) (Options, error) {
	key := sha256.Sum256(rawSnapshot)

	c.mu.Lock()
	hit := c.cached && c.key == key
	opts := c.opts
	c.mu.Unlock()
	if hit {
		return opts.clone(), nil
	}

	// Computed outside the lock: a duplicate concurrent computation is
	// harmless and idempotent, whereas holding the lock across a
	// cancellable catalog probe would let one slow request block others.
	fresh, err := AvailableOptions(ctx, client, rawSnapshot)
	if err != nil {
		return Options{}, err
	}

	c.mu.Lock()
	c.key, c.opts, c.cached = key, fresh, true
	c.mu.Unlock()
	return fresh.clone(), nil
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// decodeSnapshot rebuilds the facade Snapshot from the raw agent bytes
// Discover stored verbatim in Run.Artifacts["snapshot.yaml"]. Callers check
// len(raw) == 0 themselves before calling this: "nothing stored yet" is the
// valid pre-Discover state, not a decode failure, and must not share this
// function's error return with a genuine parse failure -- keeping that check
// out of this function (rather than returning (nil, nil) for it) is also
// what lets decodeSnapshot's own two return paths, error or a populated
// snapshot, stay unambiguous to every caller.
//
// The reconstruction must go through snapshotter.Snapshot and
// aicr.WrapSnapshot rather than a bare &aicr.Snapshot{Raw: raw} literal: the
// measurement payload lives in an unexported field WrapSnapshot is the only
// way to populate, and a Raw-only literal parses without error while Unwrap()
// silently yields zero measurements.
func decodeSnapshot(raw []byte) (*aicr.Snapshot, error) {
	var inner snapshotter.Snapshot
	if err := yaml.Unmarshal(raw, &inner); err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "stored snapshot is unparseable", err)
	}
	wrapped := aicr.WrapSnapshot(&inner)
	wrapped.Raw = raw
	return wrapped, nil
}

// ServiceFromSnapshot derives just the Criteria.Service dimension from raw
// snapshot bytes, the same way steps.Recommend derives its full criteria:
// fingerprint.FromMeasurements(...).ToCriteria(reg) (see
// internal/steps/recommend.go's buildCriteria).
//
// Returns ("", nil) for an empty snapshot (nothing collected yet, not an
// error) and a wrapped error for one present but unparseable.
func ServiceFromSnapshot(client API, raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	snap, err := decodeSnapshot(raw)
	if err != nil {
		return "", err
	}

	reg := client.CriteriaRegistry()
	if reg == nil {
		// Mirrors buildCriteria's own fallback in internal/steps/recommend.go:
		// client.CriteriaRegistry() only returns nil for a nil Client, but
		// Fake's zero value also returns nil, and fp.ToCriteria would need a
		// non-nil registry.
		reg = recipe.NewCriteriaRegistry()
	}

	fp := fingerprint.FromMeasurements(snap.Unwrap().Measurements)
	return string(fp.ToCriteria(reg).Service), nil
}
