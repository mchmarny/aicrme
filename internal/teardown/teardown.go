// Package teardown removes what one run installed, and nothing else.
//
// Every decision here is made against the ownership evidence
// internal/steps recorded immediately before Apply (engine.Ownership).
// Anything that evidence cannot account for is skipped and named, never
// removed on a guess: this package runs with cluster-admin against a
// cluster an operator is about to be handed back, so a false positive
// deletes someone else's work and a false negative merely leaves a release
// behind for a human to remove deliberately.
package teardown

import (
	"context"
	"io"
	"slices"
	"time"

	"github.com/mchmarny/aicrme/internal/engine"
)

// Exec runs one command, streaming its merged output to out. Narrower than
// applier.Exec on purpose: this package needs an argv and nothing else, and
// widening it to carry a working directory and an environment would invite
// a caller to point a helm uninstall at a bundle directory that no longer
// exists. engine.Reset adapts applier.BashExec onto this.
type Exec interface {
	Run(ctx context.Context, argv []string, out io.Writer) error
}

// Options configures one teardown.
type Options struct {
	// Timeout is the per-release budget handed to helm's own --timeout,
	// covering the --wait that follows the uninstall.
	Timeout time.Duration
}

// ReleaseOutcome is what happened to one component's release. Exactly one
// of Skip and Err is non-empty, or neither -- neither means it was
// uninstalled.
type ReleaseOutcome struct {
	Name      string
	Namespace string
	// Skip is why this release was left alone: it is stated rather than
	// implied because a release Reset declines to touch is the operator's
	// problem now, and they cannot act on a silence.
	Skip string
	// Err is why the uninstall failed. The release may be partially
	// removed; the teardown does not stop.
	Err string
	// Objects is what was purged after a confirmed uninstall: the objects
	// this component's chart creates and then tells helm to keep, which
	// therefore outlive the release. Empty for every component with no entry
	// in componentPurges, which today is all of them but one -- see purge.go.
	Objects []ObjectOutcome
}

// Releases uninstalls each component's helm release in reverse install
// order, skipping every release this run cannot prove it created.
//
// Two contexts, and the difference is load-bearing (spec section 8). ctx
// runs each command and carries its deadline. cancel is checked only
// BETWEEN commands: internal/applier/exec.go's BashExec SIGTERMs the whole
// process group the instant its context is canceled, so running helm under
// the cancellable context would interrupt an uninstall mid-flight and
// strand the release half-removed -- the exact residue this package exists
// to eliminate. Operator cancellation is therefore cooperative: the
// in-flight uninstall finishes, the next one never starts.
//
// Reverse order because install order encodes dependency: cert-manager
// issues the certificates gpu-operator's webhooks present, and removing it
// first leaves the operator's own uninstall hooks unable to complete.
//
// A failure does not end the teardown. This inverts Apply's policy
// deliberately: Apply stops at the first failure because continuing builds
// on a broken foundation, while a teardown that stops early leaves strictly
// more residue than one that finishes.
//
// Returns one outcome per component, in the order they were considered,
// including the ones it skipped -- the result is the residue inventory, so
// a component missing from it would be a component nobody knows to check.
func Releases(ctx, cancel context.Context, e Exec, comps []engine.ComponentState,
	own engine.Ownership, opts Options, emit func(ReleaseOutcome)) []ReleaseOutcome {

	ordered := slices.Clone(comps)
	// Stable, so components that share an index (or have none, on a record
	// written before Apply reported one) keep a deterministic relative
	// order rather than depending on the sort's internal swaps.
	slices.SortStableFunc(ordered, func(a, b engine.ComponentState) int { return b.Index - a.Index })

	adopted := adoptedReleases(own)
	unprovable := unprovableNamespaces(own)

	outcomes := make([]ReleaseOutcome, 0, len(ordered))
	stopped := false
	for _, c := range ordered {
		out := ReleaseOutcome{Name: c.Name, Namespace: c.Namespace}
		switch {
		case stopped:
			out.Skip = "teardown was canceled before this release was reached"
		case c.Namespace == "":
			// A release is addressed as (name, namespace). Half of that
			// identity names nothing, and guessing the other half is how a
			// uninstall lands in the wrong namespace.
			out.Skip = "no namespace was recorded for this component, so its release cannot be identified"
		case unprovable[c.Namespace] != "":
			out.Skip = "what already existed in " + c.Namespace + " could not be recorded before the install (" +
				unprovable[c.Namespace] + "), so this release cannot be shown to be this run's"
		case adopted[engine.ReleaseRef{Name: c.Name, Namespace: c.Namespace}]:
			out.Skip = "this release already existed before the install, so it was adopted rather than created"
		default:
			if err := e.Run(ctx, uninstallArgv(c, opts.Timeout), io.Discard); err != nil {
				out.Err = err.Error()
			}
			// The purge follows a CONFIRMED removal and nothing else. Its
			// placement inside this branch is the ownership argument: every
			// other arm above is a release this run cannot prove it created,
			// and an object belonging to somebody else's release is equally
			// theirs. A failed uninstall is excluded too -- see purge's own
			// comment for why deleting objects out from under controllers
			// that may still be running is worse than leaving them.
			if out.Err == "" && cancel.Err() == nil {
				out.Objects = purge(ctx, cancel, e, c.Name)
			}
			// Checked after the command, not before the next iteration's
			// work, so the outcome of the command that WAS in flight is
			// recorded before anything stops.
			if cancel.Err() != nil {
				stopped = true
			}
		}
		outcomes = append(outcomes, out)
		emit(out)
	}
	return outcomes
}

// uninstallArgv is helm's own release-removal command.
//
// --ignore-not-found is what makes a second Reset clean rather than a wall
// of "release: not found" errors -- a re-Reset after a partial one is the
// normal recovery path, not an edge case.
//
// --wait, with an explicit --timeout, is what makes the namespace emptiness
// check that follows meaningful: without it helm returns as soon as the
// delete is accepted, and Namespaces would race the API server's cascade
// and find objects that are already on their way out.
func uninstallArgv(c engine.ComponentState, timeout time.Duration) []string {
	return []string{
		"helm", "uninstall", c.Name,
		"-n", c.Namespace,
		"--ignore-not-found",
		"--wait", "--timeout", timeout.String(),
	}
}

// adoptedReleases indexes the pre-Apply release list. Keyed on the whole
// ReleaseRef rather than the name: a release name is unique only within a
// namespace, so matching on name alone would strand a release this run
// really did create just because something shared its name elsewhere.
func adoptedReleases(own engine.Ownership) map[engine.ReleaseRef]bool {
	out := make(map[engine.ReleaseRef]bool, len(own.Releases))
	for _, r := range own.Releases {
		out[r] = true
	}
	return out
}

// unprovableNamespaces maps each namespace whose pre-Apply state could not
// be recorded to the reason. Every release in one is unprovable: without
// the "before" list there is no way to tell a release this run created from
// one it upgraded.
func unprovableNamespaces(own engine.Ownership) map[string]string {
	out := map[string]string{}
	for _, ns := range own.Namespaces {
		if ns.SnapshotErr != "" {
			out[ns.Name] = ns.SnapshotErr
		}
	}
	return out
}
