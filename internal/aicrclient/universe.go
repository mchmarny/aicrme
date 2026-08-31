package aicrclient

import (
	"context"
	"fmt"
)

// Component is one chart AICR could install, as the cluster would show it.
type Component struct {
	// Name is AICR's identifier, e.g. "nfd". Shown to a human; never matched
	// against a release, because the cluster does not carry it.
	Name string
	// Chart is the upstream chart name, e.g. "node-feature-discovery". The
	// only field a helm release can be matched on.
	Chart string
	// Namespace is the recipe's recommended install namespace, corroborating
	// evidence rather than a matching key.
	Namespace string
	// Order is the component's position in the recipe, driving removal
	// sequence in increment 2. Where recipes disagree the LATEST wins:
	// cert-manager issues the certificates gpu-operator's uninstall hooks
	// present, so removing it late is the safe direction when ambiguous.
	Order int
}

// ComponentUniverse is every chart AICR could install, and how sure of that
// this process is.
//
// Complete is load-bearing rather than informational. Chart matching is the
// only thing standing between "this release is AICR's" and "this release is
// somebody else's", so a universe missing entries produces false negatives --
// installed components reported as absent. Nothing may be recommended on a
// universe that is not Complete.
type ComponentUniverse struct {
	Charts   map[string]Component
	Complete bool
	// Skipped counts catalog entries that failed to resolve. Reported so the
	// operator sees "3 of 41 overlays could not be read" rather than a silent
	// shortfall.
	Skipped int
}

// Universe resolves every chart AICR could install, from the embedded catalog,
// with no cluster involvement.
//
// The survey runs at Connect -- before a run exists and before a snapshot has
// been collected -- and ResolveRecipeFromCriteria is the only entry point with
// that property; everything else in the client wants a Snapshot.
//
// Criteria are fed per entry rather than as one nil filter: the pinned client
// validates its inputs, and a catalog entry already carries the exact criteria
// that resolve it.
//
// THREE FAILURE MODES, DELIBERATELY DIFFERENT. A failing catalog is fatal:
// with no entries there is nothing to union, and an empty universe is the most
// dangerous wrong answer this can give. A canceled context is fatal: the
// caller has gone away and no answer should be produced. A single entry that
// will not resolve is neither -- the catalog grows overlays a pinned client
// may not know -- so it is counted, Complete goes false, and the charts that
// did resolve survive.
func Universe(ctx context.Context, c API) (ComponentUniverse, error) {
	if err := ctx.Err(); err != nil {
		return ComponentUniverse{}, err
	}
	entries, err := c.ListCatalog(ctx, nil)
	if err != nil {
		return ComponentUniverse{}, fmt.Errorf("listing the AICR catalog: %w", err)
	}
	u := ComponentUniverse{Charts: make(map[string]Component), Complete: true}
	for i := range entries {
		if err := ctx.Err(); err != nil {
			return ComponentUniverse{}, err
		}
		criteria := entries[i].Criteria
		r, err := c.ResolveRecipeFromCriteria(ctx, &criteria)
		if err != nil {
			// Cancellation reaches here as a resolve error too, and must not
			// be counted as a merely-unresolvable overlay.
			if ctx.Err() != nil {
				return ComponentUniverse{}, ctx.Err()
			}
			u.Skipped++
			u.Complete = false
			continue
		}
		if r == nil {
			u.Skipped++
			u.Complete = false
			continue
		}
		for pos, ref := range r.Components {
			chart := ref.Chart
			if chart == "" {
				// Chart defaults to Name when the registry leaves it unset.
				chart = ref.Name
			}
			if chart == "" {
				continue
			}
			prev, seen := u.Charts[chart]
			if seen && prev.Order >= pos {
				continue
			}
			ns := ref.Namespace
			if ns == "" {
				ns = prev.Namespace
			}
			u.Charts[chart] = Component{Name: ref.Name, Chart: chart, Namespace: ns, Order: pos}
		}
	}
	return u, nil
}
