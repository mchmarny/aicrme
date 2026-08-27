package teardown_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/teardown"
)

// kaiComp is the one component with a purge table today, at the namespace
// AICR's recipe installs it into.
func kaiComp() engine.ComponentState {
	return engine.ComponentState{Name: "kai-scheduler", Namespace: "kai-scheduler", Index: 1}
}

func kaiPurgeArgv() [][]string {
	return [][]string{
		{"kubectl", "delete", "schedulingshard.kai.scheduler", "default",
			"--ignore-not-found", "--timeout=1m0s"},
		{"kubectl", "delete", "queue.scheduling.run.ai", "default-queue",
			"--ignore-not-found", "--timeout=1m0s"},
		{"kubectl", "delete", "queue.scheduling.run.ai", "default-parent-queue",
			"--ignore-not-found", "--timeout=1m0s"},
		{"kubectl", "delete", "config.kai.scheduler", "kai-config",
			"-n", "kai-scheduler", "--ignore-not-found", "--timeout=1m0s"},
	}
}

// THE POINT OF THE WHOLE FEATURE. kai-scheduler's chart creates four objects
// and then tells helm to keep them: three carry helm.sh/resource-policy:
// keep, and Config/kai-config is a pre-install hook, which is not part of the
// release manifest at all. The SchedulingShard is the one that bites --
// it owns the kai-scheduler-default Deployment, so a reinstall that finds the
// shard already there never recreates the scheduler and the cluster keeps
// running the PREVIOUS generation's scheduler pod, which does not schedule
// new gangs. Measured: a second install placed a gang in 8s on a fresh
// cluster and never at all on a reset one, and 2s once these four were gone.
func TestReleasesPurgesWhatHelmIsToldToKeep(t *testing.T) {
	e := &fakeExec{}

	teardown.Releases(context.Background(), context.Background(), e,
		[]engine.ComponentState{kaiComp()}, engine.Ownership{},
		teardown.Options{Timeout: 5 * time.Minute}, noEmit)

	want := append([][]string{
		{"helm", "uninstall", "kai-scheduler", "-n", "kai-scheduler",
			"--ignore-not-found", "--wait", "--timeout", "5m0s"},
	}, kaiPurgeArgv()...)

	if !reflect.DeepEqual(e.calls, want) {
		t.Errorf("commands =\n%v\nwant\n%v", e.calls, want)
	}
}

// A component with nothing in the table must cost nothing. The table is
// per-component knowledge in a package that is otherwise entirely generic,
// and it has to stay opt-in rather than becoming a tax every uninstall pays.
func TestReleasesPurgesNothingForAComponentWithNoTableEntry(t *testing.T) {
	e := &fakeExec{}

	teardown.Releases(context.Background(), context.Background(), e,
		[]engine.ComponentState{{Name: "cert-manager", Namespace: "cert-manager", Index: 1}},
		engine.Ownership{}, teardown.Options{Timeout: 5 * time.Minute}, noEmit)

	if len(e.calls) != 1 {
		t.Errorf("ran %d commands, want only the uninstall: %v", len(e.calls), e.calls)
	}
}

// THE OWNERSHIP BITE-PROOF, and the reason the purge lives inside the
// uninstall branch rather than in a loop of its own. A release that already
// existed before Apply was adopted, not created -- so kai was installed by
// somebody else, and the SchedulingShard is THEIRS. Deleting it would take
// down a scheduler this console never installed.
func TestReleasesDoesNotPurgeAnAdoptedRelease(t *testing.T) {
	e := &fakeExec{}
	own := engine.Ownership{
		Releases: []engine.ReleaseRef{{Name: "kai-scheduler", Namespace: "kai-scheduler"}},
	}

	teardown.Releases(context.Background(), context.Background(), e,
		[]engine.ComponentState{kaiComp()}, own,
		teardown.Options{Timeout: 5 * time.Minute}, noEmit)

	if len(e.calls) != 0 {
		t.Errorf("ran %d commands against an adopted release, want none: %v", len(e.calls), e.calls)
	}
}

// A namespace whose pre-Apply state could not be recorded makes every
// release in it unprovable, and an unprovable release's objects are equally
// unprovable. Same rule, one layer down.
func TestReleasesDoesNotPurgeAnUnprovableRelease(t *testing.T) {
	e := &fakeExec{}
	own := engine.Ownership{
		Namespaces: []engine.NamespaceRef{
			{Name: "kai-scheduler", SnapshotErr: "the API server said no"},
		},
	}

	teardown.Releases(context.Background(), context.Background(), e,
		[]engine.ComponentState{kaiComp()}, own,
		teardown.Options{Timeout: 5 * time.Minute}, noEmit)

	if len(e.calls) != 0 {
		t.Errorf("ran %d commands against an unprovable release, want none: %v", len(e.calls), e.calls)
	}
}

// A helm uninstall that failed may have left the release half-removed, with
// its controllers still running. Deleting the objects those controllers own
// while they are alive invites them to be recreated -- or worse, to be
// recreated with a finalizer nothing will clear. The purge is what follows a
// CONFIRMED removal, so a failed uninstall gets none.
func TestReleasesDoesNotPurgeAfterAFailedUninstall(t *testing.T) {
	e := &fakeExec{failFor: map[string]error{"kai-scheduler": errors.New("timed out")}}

	out := teardown.Releases(context.Background(), context.Background(), e,
		[]engine.ComponentState{kaiComp()}, engine.Ownership{},
		teardown.Options{Timeout: 5 * time.Minute}, noEmit)

	if len(e.calls) != 1 {
		t.Errorf("ran %d commands after a failed uninstall, want only the uninstall: %v", len(e.calls), e.calls)
	}
	if out[0].Err == "" {
		t.Error("the failed uninstall lost its error")
	}
	if len(out[0].Objects) != 0 {
		t.Errorf("reported %d purged objects after a failed uninstall, want 0", len(out[0].Objects))
	}
}

// A purge that fails is residue -- an object still standing that the next
// install will inherit -- so it is recorded rather than swallowed. And it
// does not stop the remaining purges: the same reasoning Releases already
// applies to a failed uninstall, one layer down. Three of the four objects
// removed is strictly better than one.
func TestReleasesRecordsAPurgeFailureAndKeepsGoing(t *testing.T) {
	e := &fakeExec{failFor: map[string]error{"default-queue": errors.New("webhook refused")}}

	out := teardown.Releases(context.Background(), context.Background(), e,
		[]engine.ComponentState{kaiComp()}, engine.Ownership{},
		teardown.Options{Timeout: 5 * time.Minute}, noEmit)

	if len(e.calls) != 5 {
		t.Errorf("ran %d commands, want the uninstall plus all four purges: %v", len(e.calls), e.calls)
	}
	if out[0].Err != "" {
		t.Errorf("the release itself was reported failed (%q); only one object failed", out[0].Err)
	}

	var failed []string
	for _, o := range out[0].Objects {
		if o.Err != "" {
			failed = append(failed, o.Name)
		}
	}
	// default-queue only. fakeExec matches an argv ELEMENT exactly, so the
	// two queue names are distinct inputs here -- and asserting the exact
	// slice rather than a count is what would catch a purge that reported
	// the wrong one of them as the failure.
	if !reflect.DeepEqual(failed, []string{"default-queue"}) {
		t.Errorf("failed objects = %v, want exactly [default-queue]", failed)
	}
	if len(out[0].Objects) != 4 {
		t.Errorf("reported %d objects, want all 4 attempted", len(out[0].Objects))
	}
}

// Cancellation is checked between commands, never during one -- the property
// Releases already guarantees for uninstalls. A purge is a delete against a
// live cluster and gets the same treatment: the one in flight finishes.
func TestReleasesStopsPurgingWhenCanceledBetweenCommands(t *testing.T) {
	cancelCtx, cancel := context.WithCancel(context.Background())
	e := &fakeExec{}
	// Cancel after the uninstall and the first purge have run.
	e.onCall = func(n int) {
		if n == 2 {
			cancel()
		}
	}

	teardown.Releases(context.Background(), cancelCtx, e,
		[]engine.ComponentState{kaiComp()}, engine.Ownership{},
		teardown.Options{Timeout: 5 * time.Minute}, noEmit)

	if e.interrupted {
		t.Error("a command was interrupted mid-flight; cancellation must be cooperative")
	}
	if len(e.calls) != 2 {
		t.Errorf("ran %d commands, want the uninstall plus the one purge already in flight: %v",
			len(e.calls), e.calls)
	}
}
