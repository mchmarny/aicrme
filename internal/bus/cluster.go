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
	// Resolved marks a condition clearing rather than arising. A resolved
	// condition always supersedes the unresolved one it clears, whatever the
	// severity -- otherwise a row keeps showing a failure that has gone away.
	Resolved bool      `json:"resolved,omitempty"`
	At       time.Time `json:"at"`
}

// Supersedes reports whether d should replace prev on a component row.
func (d ClusterData) Supersedes(prev ClusterData) bool {
	if d.UID != prev.UID || d.Reason != prev.Reason {
		return false
	}
	if d.Resolved != prev.Resolved {
		return d.Resolved
	}
	if d.Severity != prev.Severity {
		return d.Severity > prev.Severity
	}
	return d.At.After(prev.At)
}
