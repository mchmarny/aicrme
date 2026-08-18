package bus

import "time"

// Severity orders cluster conditions so a row can show the one that matters
// most. It is deliberately separate from Level: Level is how loudly to render
// an event in the timeline, Severity is which condition wins a row.
type Severity int

const (
	SeverityInfo Severity = iota
	SeverityWarn
	SeverityError
)

// ClusterData is the typed payload of a KindCluster event. It exists instead
// of a formatted message string because the cockpit needs to compare and
// supersede conditions per row, and parsing prose to do that is how a stale
// ImagePullBackOff ends up pinned to a row forever.
//
// Kind/Namespace/Name/UID identify the resource. UID is the identity that
// matters: a Deployment deleted and recreated under the same name is a
// different resource, and its old conditions must not survive.
type ClusterData struct {
	Kind      string   `json:"kind"`
	Namespace string   `json:"namespace,omitempty"`
	Name      string   `json:"name"`
	UID       string   `json:"uid"`
	Container string   `json:"container,omitempty"`
	Reason    string   `json:"reason"`
	Ready     int32    `json:"ready,omitempty"`
	Desired   int32    `json:"desired,omitempty"`
	Severity  Severity `json:"severity"`
	// Resolved marks a condition clearing rather than arising. It does NOT
	// unconditionally outrank an unresolved condition on the same (UID,
	// Reason) -- Supersedes orders by At first, so a later recurrence can
	// re-arm a row that an earlier resolution would otherwise mask forever.
	// That used to be the rule ("a resolved condition always supersedes,
	// whatever the severity"), and it was wrong in the more dangerous
	// direction: stale-green (a resolved entry hiding a live failure) is
	// worse than stale-red (an unresolved entry lingering a beat too long).
	// Resolved now only breaks a tie between two events sharing the exact
	// same At.
	Resolved bool      `json:"resolved,omitempty"`
	At       time.Time `json:"at"`
}

// Supersedes reports whether d is a newer version of prev's own condition --
// same resource (UID), same Reason -- and should replace it. This is NOT the
// same question as what a row displays: a row holds one ClusterData per
// distinct (UID, Reason) it has seen and separately picks the
// highest-severity unresolved one across those to show. Supersedes never
// compares across Reasons; two different Reasons on the same resource (say
// RolloutProgress and ImagePullBackOff) are two distinct conditions that
// coexist, not two versions of one condition racing to replace each other.
//
// Ordering is by At first: whichever event happened later wins, full stop,
// regardless of Resolved or Severity. Resolved and Severity only decide a
// tie between two events sharing the identical At (a publisher, like
// Observer.publish, that stamps At itself will rarely if ever produce one).
// This is deliberately NOT "a resolution always wins" -- that rule stopped a
// stale failure from pinning a row, but its unexamined inverse was that a
// genuine RECURRENCE (an unresolved condition arriving after an earlier
// resolution on the same UID+Reason -- CrashLoopBackOff, then Running, then
// CrashLoopBackOff again) could never re-arm the row either. At-ordering
// fixes both directions with one rule: whatever happened most recently is
// what the row shows.
func (d ClusterData) Supersedes(prev ClusterData) bool {
	if d.UID != prev.UID || d.Reason != prev.Reason {
		return false
	}
	if !d.At.Equal(prev.At) {
		return d.At.After(prev.At)
	}
	if d.Resolved != prev.Resolved {
		return d.Resolved
	}
	return d.Severity > prev.Severity
}
