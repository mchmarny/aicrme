package teardown_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/teardown"
)

type fakeExec struct {
	calls       [][]string
	failFor     map[string]error
	onCall      func(n int)
	interrupted bool
}

func (f *fakeExec) Run(ctx context.Context, argv []string, _ io.Writer) error {
	f.calls = append(f.calls, argv)
	if f.onCall != nil {
		f.onCall(len(f.calls))
	}
	if ctx.Err() != nil {
		f.interrupted = true
	}
	for name, err := range f.failFor {
		if slices.Contains(argv, name) {
			return err
		}
	}
	return nil
}

func noEmit(teardown.ReleaseOutcome) {}

// Reverse install order, with the exact flags: --ignore-not-found is what
// makes a second Reset clean rather than a wall of "release: not found",
// and --wait is what makes the namespace emptiness check that follows
// meaningful rather than a race against deletion.
func TestReleasesUninstallsInReverseOrderWithTheRightFlags(t *testing.T) {
	e := &fakeExec{}
	comps := []engine.ComponentState{
		{Name: "cert-manager", Namespace: "cert-manager", Index: 1},
		{Name: "nfd", Namespace: "node-feature-discovery", Index: 2},
		{Name: "gpu-operator", Namespace: "gpu-operator", Index: 3},
	}

	teardown.Releases(context.Background(), context.Background(), e, comps,
		engine.Ownership{}, teardown.Options{Timeout: 5 * time.Minute}, noEmit)

	want := [][]string{
		{"helm", "uninstall", "gpu-operator", "-n", "gpu-operator", "--ignore-not-found", "--wait", "--timeout", "5m0s"},
		{"helm", "uninstall", "nfd", "-n", "node-feature-discovery", "--ignore-not-found", "--wait", "--timeout", "5m0s"},
		{"helm", "uninstall", "cert-manager", "-n", "cert-manager", "--ignore-not-found", "--wait", "--timeout", "5m0s"},
	}
	if !reflect.DeepEqual(e.calls, want) {
		t.Errorf("commands =\n%v\nwant\n%v", e.calls, want)
	}
}

// The ownership bite-proof. A release present before Apply was adopted by
// `helm upgrade --install`, not created -- uninstalling it would remove
// something a human installed.
func TestReleasesSkipsWhatItDidNotCreate(t *testing.T) {
	e := &fakeExec{}
	comps := []engine.ComponentState{
		{Name: "gpu-operator", Namespace: "gpu-operator", Index: 1},
		{Name: "somebody-elses-thing", Namespace: "gpu-operator", Index: 2},
	}
	own := engine.Ownership{Releases: []engine.ReleaseRef{
		{Name: "somebody-elses-thing", Namespace: "gpu-operator"},
	}}

	out := teardown.Releases(context.Background(), context.Background(), e, comps, own,
		teardown.Options{Timeout: time.Minute}, noEmit)

	for _, call := range e.calls {
		for _, arg := range call {
			if arg == "somebody-elses-thing" {
				t.Fatal("uninstalled a release that existed before Apply")
			}
		}
	}
	var skipped bool
	for _, o := range out {
		if o.Name == "somebody-elses-thing" && o.Skip != "" {
			skipped = true
		}
	}
	if !skipped {
		t.Error("the skipped release is not reported -- it must be named, not silently ignored")
	}
	// The positive control. Asserting only that the bystander was spared
	// would pass against a Releases that uninstalled nothing at all.
	if len(e.calls) != 1 || !slices.Contains(e.calls[0], "gpu-operator") {
		t.Errorf("commands = %v, want exactly one, for gpu-operator", e.calls)
	}
}

// A release name is only unique within a namespace. Ownership recorded for
// (name, ns-a) says nothing about the same name in ns-b, and treating it as
// a match would strand a release this run really did create.
func TestReleasesMatchesOwnershipOnNamespaceToo(t *testing.T) {
	e := &fakeExec{}
	comps := []engine.ComponentState{{Name: "prometheus", Namespace: "monitoring", Index: 1}}
	own := engine.Ownership{Releases: []engine.ReleaseRef{
		{Name: "prometheus", Namespace: "somewhere-else"},
	}}

	out := teardown.Releases(context.Background(), context.Background(), e, comps, own,
		teardown.Options{Timeout: time.Minute}, noEmit)

	if len(e.calls) != 1 {
		t.Errorf("ran %v, want the uninstall -- the pre-existing release is in a different namespace", e.calls)
	}
	if out[0].Skip != "" {
		t.Errorf("Skip = %q, want empty", out[0].Skip)
	}
}

// Every release in a namespace whose snapshot failed is unprovable.
func TestReleasesSkipsNamespacesWhoseSnapshotFailed(t *testing.T) {
	e := &fakeExec{}
	comps := []engine.ComponentState{{Name: "prometheus-adapter", Namespace: "monitoring", Index: 1}}
	own := engine.Ownership{Namespaces: []engine.NamespaceRef{
		{Name: "monitoring", SnapshotErr: "connection refused"},
	}}

	out := teardown.Releases(context.Background(), context.Background(), e, comps, own,
		teardown.Options{Timeout: time.Minute}, noEmit)

	if len(e.calls) != 0 {
		t.Errorf("ran %v, want nothing -- ownership could not be established", e.calls)
	}
	if out[0].Skip == "" {
		t.Error("skip reason is empty")
	}
}

// A component row with no namespace cannot be addressed as a helm release
// at all. It happens to a run recovered from a record written before
// ComponentState carried the field.
func TestReleasesSkipsAComponentWithNoRecordedNamespace(t *testing.T) {
	e := &fakeExec{}
	comps := []engine.ComponentState{{Name: "gpu-operator", Index: 1}}

	out := teardown.Releases(context.Background(), context.Background(), e, comps, engine.Ownership{},
		teardown.Options{Timeout: time.Minute}, noEmit)

	if len(e.calls) != 0 {
		t.Errorf("ran %v, want nothing -- half a release identity is not a release", e.calls)
	}
	if out[0].Skip == "" {
		t.Error("skip reason is empty")
	}
}

// One failure must not end the teardown: stopping early leaves strictly
// more residue than finishing, which inverts Apply's own policy for a
// reason (spec section 6).
func TestReleasesContinuesPastAFailure(t *testing.T) {
	e := &fakeExec{failFor: map[string]error{"nfd": errors.New("release is in a failed state")}}
	comps := []engine.ComponentState{
		{Name: "cert-manager", Namespace: "cert-manager", Index: 1},
		{Name: "nfd", Namespace: "node-feature-discovery", Index: 2},
		{Name: "gpu-operator", Namespace: "gpu-operator", Index: 3},
	}

	out := teardown.Releases(context.Background(), context.Background(), e, comps, engine.Ownership{},
		teardown.Options{Timeout: time.Minute}, noEmit)

	if len(e.calls) != 3 {
		t.Errorf("ran %d commands, want 3 -- a failed uninstall must not end the teardown", len(e.calls))
	}
	var failed int
	for _, o := range out {
		if o.Err != "" {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("reported %d failures, want exactly 1", failed)
	}
}

// Operator cancellation is cooperative: the in-flight uninstall completes,
// and the NEXT one does not start. internal/applier/exec.go's BashExec
// SIGTERMs the whole process group the moment its context is canceled, so
// handing it the cancellable context would interrupt helm mid-uninstall --
// which is how a release ends up half-removed, the exact residue Reset
// exists to eliminate.
func TestReleasesCancellationDoesNotInterruptTheInFlightCommand(t *testing.T) {
	cancelCtx, cancel := context.WithCancel(context.Background())
	e := &fakeExec{onCall: func(n int) {
		if n == 1 {
			cancel() // cancel DURING the first uninstall
		}
	}}
	comps := []engine.ComponentState{
		{Name: "a", Namespace: "ns-a", Index: 1},
		{Name: "b", Namespace: "ns-b", Index: 2},
	}

	teardown.Releases(context.Background(), cancelCtx, e, comps, engine.Ownership{},
		teardown.Options{Timeout: time.Minute}, noEmit)

	if len(e.calls) != 1 {
		t.Errorf("ran %d commands, want exactly 1 -- the in-flight one completes, the next does not start", len(e.calls))
	}
	if e.interrupted {
		t.Error("the in-flight command saw a canceled context")
	}
}

// A canceled teardown still has to say what it did not do. The outcome
// list is the residue inventory -- an operator handed a partial teardown
// with no record of the remainder has nothing to act on.
func TestReleasesReportsWhatCancellationLeftBehind(t *testing.T) {
	cancelCtx, cancel := context.WithCancel(context.Background())
	e := &fakeExec{onCall: func(n int) {
		if n == 1 {
			cancel()
		}
	}}
	comps := []engine.ComponentState{
		{Name: "a", Namespace: "ns-a", Index: 1},
		{Name: "b", Namespace: "ns-b", Index: 2},
	}

	out := teardown.Releases(context.Background(), cancelCtx, e, comps, engine.Ownership{},
		teardown.Options{Timeout: time.Minute}, noEmit)

	if len(out) != 2 {
		t.Fatalf("outcomes = %+v, want one row per component even when canceled", out)
	}
	// Reverse order: b ran, a was skipped.
	if out[0].Name != "b" || out[0].Skip != "" || out[0].Err != "" {
		t.Errorf("outcomes[0] = %+v, want b uninstalled cleanly", out[0])
	}
	if out[1].Name != "a" || out[1].Skip == "" {
		t.Errorf("outcomes[1] = %+v, want a skipped with a stated reason", out[1])
	}
}

// Every outcome reaches the caller as it happens, not only in the returned
// slice: the returned slice arrives after the last uninstall, and a
// teardown of thirteen components with --wait takes minutes.
func TestReleasesEmitsEachOutcomeAsItHappens(t *testing.T) {
	e := &fakeExec{}
	comps := []engine.ComponentState{
		{Name: "cert-manager", Namespace: "cert-manager", Index: 1},
		{Name: "gpu-operator", Namespace: "gpu-operator", Index: 2},
	}

	var emitted []teardown.ReleaseOutcome
	out := teardown.Releases(context.Background(), context.Background(), e, comps, engine.Ownership{},
		teardown.Options{Timeout: time.Minute}, func(o teardown.ReleaseOutcome) {
			emitted = append(emitted, o)
		})

	if !reflect.DeepEqual(emitted, out) {
		t.Errorf("emitted %+v, returned %+v -- they must be the same outcomes in the same order", emitted, out)
	}
}
