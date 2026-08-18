package bus_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mchmarny/aicrme/internal/bus"
)

// Breaks if the Severity constants are reordered or renumbered -- the whole
// point of Severity being an ordered int rather than an opaque string is
// that a row can pick "the worse of two conditions" with a plain comparison.
func TestSeverityOrdering(t *testing.T) {
	if bus.SeverityInfo >= bus.SeverityWarn {
		t.Errorf("SeverityInfo (%d) is not less than SeverityWarn (%d)", bus.SeverityInfo, bus.SeverityWarn)
	}
	if bus.SeverityWarn >= bus.SeverityError {
		t.Errorf("SeverityWarn (%d) is not less than SeverityError (%d)", bus.SeverityWarn, bus.SeverityError)
	}
}

// Breaks if Supersedes' severity comparison is removed, inverted, or the
// operator flipped (> vs <): an error on a row would then lose to a warn
// that arrived first, which is the opposite of "show what matters most".
func TestSupersedesPrefersHigherSeverity(t *testing.T) {
	at := time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	warn := bus.ClusterData{UID: "ds-uid", Reason: "ImagePullBackOff", Severity: bus.SeverityWarn, At: at}
	fail := bus.ClusterData{UID: "ds-uid", Reason: "ImagePullBackOff", Severity: bus.SeverityError, At: at}

	if !fail.Supersedes(warn) {
		t.Errorf("SeverityError.Supersedes(SeverityWarn) = false, want true")
	}
	if warn.Supersedes(fail) {
		t.Errorf("SeverityWarn.Supersedes(SeverityError) = true, want false")
	}
}

// Breaks if the At tiebreaker is removed or its direction flipped: two
// updates on the same resource at the same severity would then either never
// advance the row or regress it to the older one.
func TestSupersedesPrefersNewerAtEqualSeverity(t *testing.T) {
	older := bus.ClusterData{UID: "ds-uid", Reason: "CrashLoopBackOff", Severity: bus.SeverityWarn, At: time.Unix(100, 0)}
	newer := bus.ClusterData{UID: "ds-uid", Reason: "CrashLoopBackOff", Severity: bus.SeverityWarn, At: time.Unix(200, 0)}

	if !newer.Supersedes(older) {
		t.Errorf("newer.Supersedes(older) = false, want true")
	}
	if older.Supersedes(newer) {
		t.Errorf("older.Supersedes(newer) = true, want false")
	}
}

// Breaks if the Resolved check is removed, or inverted (see the Step 6
// bite-proof), or reordered after the severity/At checks. resolved here
// deliberately has BOTH a lower severity AND an earlier At than unresolved,
// so any of those other fields deciding the outcome first would also flip
// this test -- it is not enough for the Resolved branch to merely exist, it
// has to run first and win.
func TestResolvedConditionSupersedesUnresolved(t *testing.T) {
	unresolved := bus.ClusterData{
		UID: "ds-uid", Reason: "ImagePullBackOff",
		Severity: bus.SeverityError, At: time.Unix(200, 0),
	}
	resolved := bus.ClusterData{
		UID: "ds-uid", Reason: "ImagePullBackOff",
		Severity: bus.SeverityInfo, Resolved: true, At: time.Unix(100, 0),
	}

	if !resolved.Supersedes(unresolved) {
		t.Errorf("resolved.Supersedes(unresolved) = false, want true (lower severity and earlier At must not matter)")
	}
	if unresolved.Supersedes(resolved) {
		t.Errorf("unresolved.Supersedes(resolved) = true, want false")
	}
}

// Breaks if the UID equality check is dropped from Supersedes. resourceB
// shares resourceA's Reason and beats it on both severity and At, so without
// the UID guard the fall-through comparisons alone would make resourceB
// supersede resourceA -- exactly the cross-resource bleed the UID check
// exists to prevent (a Deployment recreated under the same name must not
// inherit, and must not donate, conditions across the identity boundary).
func TestSupersedesIsNotTransitiveAcrossResources(t *testing.T) {
	resourceA := bus.ClusterData{UID: "uid-a", Reason: "ImagePullBackOff", Severity: bus.SeverityWarn, At: time.Unix(100, 0)}
	resourceB := bus.ClusterData{UID: "uid-b", Reason: "ImagePullBackOff", Severity: bus.SeverityError, At: time.Unix(200, 0)}

	if resourceB.Supersedes(resourceA) {
		t.Errorf("resourceB.Supersedes(resourceA) = true, want false: different UIDs must never supersede")
	}
	if resourceA.Supersedes(resourceB) {
		t.Errorf("resourceA.Supersedes(resourceB) = true, want false: different UIDs must never supersede")
	}
}

// Breaks if any ClusterData field loses its json tag, gets renamed on one
// side, or if Event.Data stops carrying an opaque json.RawMessage faithfully
// (e.g. double-encoding or truncating it) -- this is the path a real
// KindCluster event actually takes: ClusterData marshaled into Event.Data,
// the Event marshaled and unmarshaled across the SSE wire, then Data
// unmarshaled back out on the other side.
func TestClusterDataRoundTripsThroughEventData(t *testing.T) {
	want := bus.ClusterData{
		Kind:      "Deployment",
		Namespace: "gpu-operator",
		Name:      "nim-service",
		UID:       "deploy-uid",
		Container: "nim",
		Reason:    "ImagePullBackOff",
		Ready:     1,
		Desired:   8,
		Severity:  bus.SeverityError,
		Resolved:  true,
		At:        time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
	}

	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal(ClusterData) error = %v", err)
	}

	eventRaw, err := json.Marshal(bus.Event{Kind: bus.KindCluster, Data: raw})
	if err != nil {
		t.Fatalf("Marshal(Event) error = %v", err)
	}

	var roundTripped bus.Event
	if err := json.Unmarshal(eventRaw, &roundTripped); err != nil {
		t.Fatalf("Unmarshal(Event) error = %v", err)
	}

	var got bus.ClusterData
	if err := json.Unmarshal(roundTripped.Data, &got); err != nil {
		t.Fatalf("Unmarshal(ClusterData) error = %v", err)
	}

	switch {
	case got.Kind != want.Kind:
		t.Errorf("Kind = %q, want %q", got.Kind, want.Kind)
	case got.Namespace != want.Namespace:
		t.Errorf("Namespace = %q, want %q", got.Namespace, want.Namespace)
	case got.Name != want.Name:
		t.Errorf("Name = %q, want %q", got.Name, want.Name)
	case got.UID != want.UID:
		t.Errorf("UID = %q, want %q", got.UID, want.UID)
	case got.Container != want.Container:
		t.Errorf("Container = %q, want %q", got.Container, want.Container)
	case got.Reason != want.Reason:
		t.Errorf("Reason = %q, want %q", got.Reason, want.Reason)
	case got.Ready != want.Ready:
		t.Errorf("Ready = %d, want %d", got.Ready, want.Ready)
	case got.Desired != want.Desired:
		t.Errorf("Desired = %d, want %d", got.Desired, want.Desired)
	case got.Severity != want.Severity:
		t.Errorf("Severity = %d, want %d", got.Severity, want.Severity)
	case got.Resolved != want.Resolved:
		t.Errorf("Resolved = %v, want %v", got.Resolved, want.Resolved)
	case !got.At.Equal(want.At):
		t.Errorf("At = %v, want %v", got.At, want.At)
	}
}
