package observer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/mchmarny/aicrme/internal/bus"
)

// scopeWith builds a RunScope the way main's newObserverScopeFn would for a
// resolved, still-live run: a non-empty RunID paired with a namespace set,
// Terminal false.
func scopeWith(runID string, ns ...string) RunScope {
	return RunScope{RunID: runID, Namespaces: namespaceSet(ns), Terminal: false}
}

// terminalScope builds the RunScope main would compose once
// engine.Attribution reports the run as terminal (Ruling 8): the same RunID
// and Namespaces a finished run keeps reporting (see scoped.go's package doc
// for why those two don't change on their own), with Terminal now true.
// Every test below terminates the same "run-1" fixture scopeWith uses, so
// runID is fixed rather than threaded through -- a real parameter here would
// have exactly one argument value at every call site.
func terminalScope(ns ...string) RunScope {
	return RunScope{RunID: "run-1", Namespaces: namespaceSet(ns), Terminal: true}
}

func namespaceSet(ns []string) map[string]struct{} {
	set := make(map[string]struct{}, len(ns))
	for _, n := range ns {
		set[n] = struct{}{}
	}
	return set
}

// TestScopedInformersDoNotStartBeforeAScopeExists is the constraint the whole
// task exists to satisfy: the observer starts before any run exists, so
// reconcile must be a no-op against the zero RunScope -- no factory, no
// client call. Checking client.Actions() rather than just entries is what
// keeps this from being vacuous: an implementation that recorded an entry
// without ever touching the client would still pass an entries-only
// assertion.
func TestScopedInformersDoNotStartBeforeAScopeExists(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := newScopedInformers(client, scopedHandlers{}, nil)

	s.reconcile(RunScope{})

	if got := len(s.entries); got != 0 {
		t.Fatalf("entries = %d, want 0 before any scope exists", got)
	}
	if actions := client.Actions(); len(actions) != 0 {
		t.Fatalf("client saw %d actions before any scope exists, want 0: %+v", len(actions), actions)
	}
}

// TestScopedInformersStartOncePerNamespace pins the constraint verified
// against the pinned client-go directly: informers.WithNamespace takes
// exactly one namespace (informers/factory.go:99, "factory.namespace =
// namespace" -- a single field, not a set), so a three-namespace recipe
// scope must yield three distinct factories PER KIND, not one
// namespace-filtered one. Pod and Event each get their own factory
// (Important 2, Task 5 fix round 1): a shared factory's
// WithTweakListOptions applies to every informer built from it, so Task 6's
// Event-only field selector would otherwise also hit the Pod ListWatch --
// see factoryEntry's doc comment (scoped.go).
//
// This only reaches map bookkeeping and non-nil handles, both of which stay
// true for factories that were never started or that watch the whole
// cluster instead of their own namespace -- see
// TestScopedInformersWatchOnlyTheirOwnNamespace for the assertion that
// actually reaches those two properties.
func TestScopedInformersStartOncePerNamespace(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := newScopedInformers(client, scopedHandlers{}, nil)

	s.reconcile(scopeWith("run-1", "gpu-operator", "kai-scheduler", "ai-runtime"))

	if got := len(s.entries); got != 3 {
		t.Fatalf("entries = %d, want 3 (one per namespace)", got)
	}
	seen := make(map[string]bool, 3)
	podFactories := make(map[interface{}]bool, 3)
	eventFactories := make(map[interface{}]bool, 3)
	for ns, e := range s.entries {
		seen[ns] = true
		podFactories[e.podFactory] = true
		eventFactories[e.eventFactory] = true
		if e.podFactory == e.eventFactory {
			t.Errorf("namespace %q: Pod and Event share one factory -- Task 6's tweak would leak onto the Pod ListWatch", ns)
		}
		if e.pod == nil || e.event == nil {
			t.Errorf("namespace %q: pod/event informer not materialized", ns)
		}
	}
	for _, ns := range []string{"gpu-operator", "kai-scheduler", "ai-runtime"} {
		if !seen[ns] {
			t.Errorf("namespace %q has no factory entry", ns)
		}
	}
	if len(podFactories) != 3 {
		t.Fatalf("distinct Pod factories = %d, want 3 (one per namespace, never shared)", len(podFactories))
	}
	if len(eventFactories) != 3 {
		t.Fatalf("distinct Event factories = %d, want 3 (one per namespace, never shared)", len(eventFactories))
	}
}

// TestScopedInformersWatchOnlyTheirOwnNamespace is the mutation-proof
// TestScopedInformersStartOncePerNamespace cannot be: it asserts against
// fake.Clientset.Actions(), which records the namespace each List/Watch call
// actually used against the client, not just against s.entries' own
// bookkeeping. Map/pointer assertions stay green whether or not
// informers.WithNamespace(ns) is actually applied (a factory watching the
// whole cluster still produces one entry with a non-nil pod/event informer)
// and whether or not the factory was ever started at all (zero entries
// mutate nothing about a map lookup) -- this test fails under both of those
// mutations: an unscoped factory records actions with namespace "" instead
// of "gpu-operator", and a never-started factory records no actions at all,
// so the initial waitFor times out.
func TestScopedInformersWatchOnlyTheirOwnNamespace(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := newScopedInformers(client, scopedHandlers{}, nil)

	s.reconcile(scopeWith("run-1", "gpu-operator"))

	waitFor(t, func() bool { return len(client.Actions()) > 0 })

	for _, a := range client.Actions() {
		if a.GetNamespace() != "gpu-operator" {
			t.Fatalf("action %s/%s recorded namespace %q, want gpu-operator only -- the factory is not namespace-scoped",
				a.GetVerb(), a.GetResource().Resource, a.GetNamespace())
		}
	}
}

// TestScopedInformersStopWhenTheRunEnds is Ruling 3: a run's terminal
// transition tears down its factories immediately, with no grace window.
// Terminal is what reconcile treats as authoritative (Ruling 8) -- there is
// no separate stop-by-runID entry point to call, so this drives the same
// path production does: a scope whose Terminal has flipped true.
func TestScopedInformersStopWhenTheRunEnds(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := newScopedInformers(client, scopedHandlers{}, nil)
	s.reconcile(scopeWith("run-1", "gpu-operator", "kai-scheduler"))
	if got := len(s.entries); got != 2 {
		t.Fatalf("entries before termination = %d, want 2", got)
	}

	s.reconcile(terminalScope("gpu-operator", "kai-scheduler"))

	if got := len(s.entries); got != 0 {
		t.Fatalf("entries after the run's terminal transition = %d, want 0", got)
	}
	if s.runID != "" {
		t.Errorf("runID = %q after termination, want empty", s.runID)
	}
}

// TestScopedInformersStopClosesTheFactoryStopChannel is the mutation-proof
// TestScopedInformersStopWhenTheRunEnds cannot be: len(s.entries) == 0 stays
// true even if stopAllLocked only deleted the map entry without ever closing
// its stop channel, which would leave that namespace's informer goroutine
// (started in startNamespace) running -- and its ListWatch open against the
// API server -- forever, with nothing left in this process referencing it to
// stop it later. A closed channel receives immediately without blocking; an
// open one with no sender blocks, so the select below distinguishes the two
// deterministically, independent of timing.
func TestScopedInformersStopClosesTheFactoryStopChannel(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := newScopedInformers(client, scopedHandlers{}, nil)
	s.reconcile(scopeWith("run-1", "gpu-operator"))

	entry, ok := s.entries["gpu-operator"]
	if !ok {
		t.Fatal("no entry for gpu-operator after reconcile -- test setup failure")
	}

	s.reconcile(terminalScope("gpu-operator"))

	select {
	case <-entry.stop:
	default:
		t.Fatal("factory's stop channel was not closed on teardown -- its informer goroutine runs forever")
	}
}

// TestScopedInformersNotifyOnNamespaceStopOnFullTeardown is Important 4
// (Task 5 fix round 1): a run's terminal transition must notify
// onNamespaceStop for every namespace it tears down, not just close the
// factory and drop the map entry -- that notification is what lets
// Observer.clearNamespacePods (observer.go) evict namespace-scoped state
// (o.pods) in step with the informer that used to feed it, instead of
// stranding it (the review's demonstrated defect).
func TestScopedInformersNotifyOnNamespaceStopOnFullTeardown(t *testing.T) {
	client := fake.NewSimpleClientset()
	var stopped []string
	s := newScopedInformers(client, scopedHandlers{}, func(ns string) {
		stopped = append(stopped, ns)
	})
	s.reconcile(scopeWith("run-1", "gpu-operator", "kai-scheduler"))

	s.reconcile(terminalScope("gpu-operator", "kai-scheduler"))

	sort.Strings(stopped)
	if got := stopped; len(got) != 2 || got[0] != "gpu-operator" || got[1] != "kai-scheduler" {
		t.Errorf("onNamespaceStop notified for %v, want both gpu-operator and kai-scheduler exactly once each", got)
	}
}

// TestScopedInformersNotifyOnNamespaceStopWhenScopeShrinks is
// TestScopedInformersNotifyOnNamespaceStopOnFullTeardown's counterpart for
// the OTHER teardown path: a continuing run whose namespace set shrinks
// (reconcile's per-namespace removal loop, not stopAllLocked). Both paths
// must notify -- a fix that only wired the callback into stopAllLocked would
// leave this path silently stranding state exactly as before.
func TestScopedInformersNotifyOnNamespaceStopWhenScopeShrinks(t *testing.T) {
	client := fake.NewSimpleClientset()
	var stopped []string
	s := newScopedInformers(client, scopedHandlers{}, func(ns string) {
		stopped = append(stopped, ns)
	})
	s.reconcile(scopeWith("run-1", "gpu-operator", "kai-scheduler"))

	s.reconcile(scopeWith("run-1", "gpu-operator")) // kai-scheduler drops out of scope, run continues

	if got := stopped; len(got) != 1 || got[0] != "kai-scheduler" {
		t.Errorf("onNamespaceStop notified for %v, want exactly [kai-scheduler]", got)
	}
}

// TestScopedInformersAreIdempotent is the property named in the brief: a
// repeated scope must not double-start. Comparing factory pointers, not just
// the count, is what actually proves it -- a naive reconcile that tore down
// and rebuilt on every call could still land on len(entries) == 3 while
// silently churning every factory underneath.
func TestScopedInformersAreIdempotent(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := newScopedInformers(client, scopedHandlers{}, nil)
	sc := scopeWith("run-1", "gpu-operator", "kai-scheduler", "ai-runtime")

	s.reconcile(sc)
	first := make(map[string]*factoryEntry, len(s.entries))
	for ns, e := range s.entries {
		first[ns] = e
	}

	s.reconcile(sc)
	s.reconcile(sc)

	if got := len(s.entries); got != 3 {
		t.Fatalf("entries after three identical reconciles = %d, want 3, not a double-start", got)
	}
	for ns, e := range s.entries {
		if first[ns] != e {
			t.Errorf("namespace %q got a new factory entry on a repeated identical scope, want the same one reused", ns)
		}
	}
}

// blockingHandler returns an http.Handler that blocks every request until
// release is closed. Mirrors cmd/aicrme/main_test.go's helper of the same
// name -- fake.NewSimpleClientset ignores context and never blocks (verified
// there against k8s.io/client-go/gentype.FakeClient.Get), so proving a call
// against a client whose List never returns needs a real transport pointed
// at a server that genuinely never answers.
func blockingHandler() (handler http.Handler, release chan struct{}) {
	release = make(chan struct{})
	handler = http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	return handler, release
}

// TestScopedInformerStartDoesNotBlock is the required bite-proof target and
// the 2b-i crashloop "one door over": a wedged API server's List must not
// stall reconcile's caller. It would pass vacuously if reconcile never
// actually started anything against the client -- it does, against a real
// *kubernetes.Clientset pointed at a server that hangs every request, so the
// only way this returns promptly is if the List genuinely runs off the
// calling goroutine.
func TestScopedInformerStartDoesNotBlock(t *testing.T) {
	handler, release := blockingHandler()
	srv := httptest.NewServer(handler)
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	client, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("kubernetes.NewForConfig() error = %v", err)
	}

	s := newScopedInformers(client, scopedHandlers{}, nil)
	done := make(chan struct{})
	go func() {
		s.reconcile(scopeWith("run-1", "gpu-operator"))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile blocked on a client whose List never returns")
	}
}

// TestScopedInformersSurviveAScopeChange: a new run with different namespaces
// stops the old set and starts the new, never leaving the two mixed.
func TestScopedInformersSurviveAScopeChange(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := newScopedInformers(client, scopedHandlers{}, nil)

	s.reconcile(scopeWith("run-1", "gpu-operator", "kai-scheduler"))
	if got := len(s.entries); got != 2 {
		t.Fatalf("entries after run-1's scope = %d, want 2", got)
	}

	s.reconcile(scopeWith("run-2", "ai-runtime"))

	if got := len(s.entries); got != 1 {
		t.Fatalf("entries after run-2's scope = %d, want 1", got)
	}
	if _, ok := s.entries["ai-runtime"]; !ok {
		t.Error("run-2's ai-runtime namespace was not started")
	}
	if _, ok := s.entries["gpu-operator"]; ok {
		t.Error("run-1's gpu-operator factory survived the scope change")
	}
	if _, ok := s.entries["kai-scheduler"]; ok {
		t.Error("run-1's kai-scheduler factory survived the scope change")
	}
	if s.runID != "run-2" {
		t.Errorf("runID = %q, want run-2", s.runID)
	}
}

// TestScopedInformersRestartAfterRetryReusesTheRunID promotes the
// coordinator review's Critical 1 probe into a permanent test.
// engine.Retry reuses the SAME run ID after a failure
// (internal/engine/engine.go) -- under the mechanism this fix replaces
// (a `terminated` sentinel latched by run ID off a terminal bus message),
// that reuse permanently wedged the retried run's informers off, because the
// sentinel had no path back to "not terminal" for an ID it had already
// recorded. With RunScope.Terminal read fresh from engine.Attribution() on
// every reconcile (Ruling 8) instead of remembered here, a retried run's
// very next scope() read -- Terminal back to false for the identical RunID,
// exactly what Engine.Attribution reports once Retry flips e.current.State
// to StateRunning (internal/engine/engine.go:785) -- is enough on its own to
// restart it.
func TestScopedInformersRestartAfterRetryReusesTheRunID(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := newScopedInformers(client, scopedHandlers{}, nil)
	b := bus.New(64)

	var scopeMu scopeHolder
	scopeMu.set(scopeWith("run-1", "gpu-operator"))

	stop := make(chan struct{})
	runDone := make(chan struct{})
	// A long ticker interval rules out the correctness floor
	// (TestScopedInformersRunTicksAsACorrectnessFloor covers that path
	// separately): if this test passes, it is the bus fast path that picked
	// up each scope change, not the ticker.
	go func() {
		s.run(scopeMu.get, b, stop, time.Hour)
		close(runDone)
	}()
	t.Cleanup(func() {
		close(stop)
		<-runDone
	})

	b.Publish(bus.Event{RunID: "run-1", Kind: bus.KindPhase, Message: "phase started"})
	waitFor(t, func() bool { return s.entryCount() == 1 })

	// The run fails: main's composed scope now reports Terminal for run-1.
	scopeMu.set(terminalScope("gpu-operator"))
	b.Publish(bus.Event{RunID: "run-1", Kind: bus.KindPhase, Message: "run failed"})
	waitFor(t, func() bool { return s.entryCount() == 0 })

	// engine.Retry reuses the SAME run ID and flips State back to
	// StateRunning, under e.mu, before publishing "run retrying"
	// (engine.go:785, 826) -- Attribution() recomputes Terminal live off
	// e.current.State on every call, so scope() reports Terminal: false
	// again for the identical RunID with no memory of the earlier read.
	scopeMu.set(scopeWith("run-1", "gpu-operator"))
	b.Publish(bus.Event{RunID: "run-1", Kind: bus.KindPhase, Message: "run retrying"})

	waitFor(t, func() bool { return s.entryCount() == 1 })
}

// TestScopedInformersRunReconcilesOnEveryPhaseEvent proves run()'s fast
// path: any KindPhase event -- not a specific message -- triggers a
// reconcile against the current scope, so both the initial lazy start (once
// a namespace scope resolves) and a terminal teardown ride the same
// mechanism. The hour-long ticker interval rules out the correctness floor:
// waitFor's 2s bound cannot be satisfied by a ticker that has not fired yet,
// so a pass here is only possible if the bus event itself woke run().
func TestScopedInformersRunReconcilesOnEveryPhaseEvent(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := newScopedInformers(client, scopedHandlers{}, nil)
	b := bus.New(64)

	var scopeMu scopeHolder
	scopeMu.set(scopeWith("run-1", "gpu-operator"))

	stop := make(chan struct{})
	runDone := make(chan struct{})
	go func() {
		s.run(scopeMu.get, b, stop, time.Hour)
		close(runDone)
	}()
	t.Cleanup(func() {
		close(stop)
		<-runDone
	})

	b.Publish(bus.Event{RunID: "run-1", Kind: bus.KindPhase, Message: "phase complete"})
	waitFor(t, func() bool { return s.entryCount() == 1 })

	scopeMu.set(terminalScope("gpu-operator"))
	b.Publish(bus.Event{RunID: "run-1", Kind: bus.KindPhase, Message: "run done"})

	waitFor(t, func() bool { return s.entryCount() == 0 })
}

// TestScopedInformersRunTicksAsACorrectnessFloor proves Ruling 9's floor
// works with NO bus event at all -- internal/bus drops live events for a
// subscriber more than subscriberBuffer behind with no replay and no
// reconnect for this listener (bus.go), so if the terminal KindPhase is the
// one dropped, the bus fast path never fires again for that run. A short
// tick interval stands in for that: the initial scope is picked up, and a
// later Terminal flip is picked up, with zero bus traffic published either
// time -- only the ticker is present to have done it.
func TestScopedInformersRunTicksAsACorrectnessFloor(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := newScopedInformers(client, scopedHandlers{}, nil)
	b := bus.New(64)

	var scopeMu scopeHolder
	scopeMu.set(scopeWith("run-1", "gpu-operator"))

	stop := make(chan struct{})
	runDone := make(chan struct{})
	const tick = 20 * time.Millisecond
	go func() {
		s.run(scopeMu.get, b, stop, tick)
		close(runDone)
	}()
	t.Cleanup(func() {
		close(stop)
		<-runDone
	})

	waitFor(t, func() bool { return s.entryCount() == 1 })

	scopeMu.set(terminalScope("gpu-operator"))

	waitFor(t, func() bool { return s.entryCount() == 0 })
}

// reconcileIntervalLeakBound is TestReconcileIntervalStaysWithinTheLeakBound's
// ceiling on the constant that actually ships. Every other run()-test above
// injects its own interval specifically to run fast and to isolate one wake
// source from the other, which is the right call for those tests but leaves
// the value Observer.Start actually passes (reconcileInterval) never
// exercised by anything: every test in this file would stay green even if
// reconcileInterval itself silently grew to, say, 24h, quietly removing
// Ruling 9's correctness floor from production. This bound is deliberately
// generous relative to the constant's own 2s (an order of magnitude, not a
// tight tolerance) -- the property worth pinning is "this cannot regress to
// effectively unbounded", not "this must stay exactly 2s".
const reconcileIntervalLeakBound = 30 * time.Second

// TestReconcileIntervalStaysWithinTheLeakBound pins the production constant
// itself, not the mechanism every other test in this file exercises through
// its own injected interval. reconcileInterval IS the answer to "how long can
// ~10 namespaces' Pod/Event watches survive a dropped terminal KindPhase
// event" (see reconcileInterval's own doc comment) -- it is Ruling 9's
// correctness floor, not an incidental tuning knob, so a value outside a sane
// bound is not a performance regression, it is that floor quietly disappearing
// with every other test in the package still green.
func TestReconcileIntervalStaysWithinTheLeakBound(t *testing.T) {
	if reconcileInterval <= 0 || reconcileInterval > reconcileIntervalLeakBound {
		t.Fatalf("reconcileInterval = %v, want a positive value at or under %v -- this constant IS the bound on how long a dropped terminal event can leave scoped informers running, not just a tuning knob",
			reconcileInterval, reconcileIntervalLeakBound)
	}
}

// TestObserverStartPassesTheProductionReconcileInterval closes the gap the
// bound above does not: that reconcileInterval is a sane value proves
// nothing about whether Observer.Start's o.scoped.run(...) call actually
// passes IT, rather than a hardcoded literal or a second constant that
// quietly drifts from it. Reading observer.go's own source for the call
// site is the same technique internal/bus/kinds_web_test.go already uses to
// pin a cross-file contract without running anything -- cheaper than a
// wall-clock test that waits out Observer.Start's real timer, which is not
// worth the seconds it would cost for a fact a source read proves directly.
func TestObserverStartPassesTheProductionReconcileInterval(t *testing.T) {
	const observerSource = "observer.go"
	raw, err := os.ReadFile(observerSource)
	if err != nil {
		t.Fatalf("reading %s: %v", observerSource, err)
	}
	re := regexp.MustCompile(`o\.scoped\.run\([^)]*\breconcileInterval\b[^)]*\)`)
	if !re.Match(raw) {
		t.Fatalf("%s's o.scoped.run(...) call does not reference reconcileInterval by name -- production wiring may have drifted to a hardcoded or different interval, decoupled from TestReconcileIntervalStaysWithinTheLeakBound's guarantee", observerSource)
	}
}

// scopeHolder lets the tests above swap the RunScope run() reads without a
// data race, standing in for the live-engine composition run() actually
// reads from in production.
type scopeHolder struct {
	mu sync.Mutex
	sc RunScope
}

func (h *scopeHolder) set(sc RunScope) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sc = sc
}

func (h *scopeHolder) get() RunScope {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sc
}

// entryCount reads s.entries under lock, so tests can poll it from outside
// without racing run()'s own goroutine.
func (s *scopedInformers) entryCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// waitFor polls cond until it is true or a bounded deadline passes, so tests
// do not need a fixed sleep to synchronize with run()'s own goroutine.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if cond() {
			return
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatal("condition not met within 2s")
		}
	}
}

// TestObserverDiscriminatesBetweenNamespacesOnPartialTeardown is I1
// (Task 6 fix round 4, whole-branch review): the observer's namespace
// dimension had NO Observer-level coverage at more than one namespace --
// every RunScope.Namespaces fixture in pods_test.go/events_test.go/
// handlers_internal_test.go has exactly one entry, and scoped_test.go's own
// two-namespace tests (scopeWith("run-1","gpu-operator","kai-scheduler"))
// construct newScopedInformers directly with an empty scopedHandlers{} and
// a nil onNamespaceStop -- they exercise factory lifecycle only, never
// o.pods/o.events/withNamespaceLive's actual discriminating behavior. With
// one namespace, s.entries[ns] (the correct check) and len(s.entries) > 0
// (a namespace-blind mutation of it) are the SAME predicate, and
// clearNamespacePods/clearNamespaceEvents deleting unconditionally vs.
// filtering on ns are indistinguishable when there is only ever one key to
// delete either way -- which is exactly why three mutations of that shape
// survived all 79 existing observer tests (see this file's own three
// bite-proofs in the fix-round report).
//
// Uses a REAL Observer (New, not newScopedInformers directly) with two live
// namespaces, each carrying its own narrated Pod condition AND Event
// condition, so this single test reaches all three mutation sites:
//  1. Tears down JUST namespace A via reconcile's per-namespace removal
//     loop (a continuing run's scope shrinking -- not a full terminal
//     teardown, so B staying live is the point) and asserts A's o.pods/
//     o.events entries are gone while B's survive untouched (kills M12 --
//     clearNamespacePods ignoring ns -- and M13, clearNamespaceEvents's
//     identical shape).
//  2. Asserts withNamespaceLive refuses a write for torn-down A even
//     though B is still live (len(s.entries) > 0 is true purely because of
//     B) -- and that it still ADMITS a write for B, so the test cannot pass
//     by a mutation that simply refuses everything (kills M14).
func TestObserverDiscriminatesBetweenNamespacesOnPartialTeardown(t *testing.T) {
	const nsA = "gpu-operator"
	const nsB = "kai-scheduler"

	client := fake.NewSimpleClientset()
	b := bus.New(64)
	o := New(client, b, func() RunScope {
		return RunScope{RunID: "run-1", Namespaces: map[string]struct{}{nsA: {}, nsB: {}}}
	})
	sub, unsub := b.Subscribe(0)
	t.Cleanup(unsub)

	o.scoped.reconcile(scopeWith("run-1", nsA, nsB))
	t.Cleanup(o.scoped.stop)

	entryA, ok := o.scoped.entries[nsA]
	if !ok {
		t.Fatalf("no scoped entry for namespace %q after reconcile", nsA)
	}
	entryB, ok := o.scoped.entries[nsB]
	if !ok {
		t.Fatalf("no scoped entry for namespace %q after reconcile", nsB)
	}
	waitFor(t, entryA.pod.HasSynced)
	waitFor(t, entryB.pod.HasSynced)
	waitFor(t, entryA.event.HasSynced)
	waitFor(t, entryB.event.HasSynced)

	podA := withContainerWaiting(testPod("pod-uid-a"), "app", reasonImagePullBackOff)
	podB := withContainerWaiting(testPod("pod-uid-b"), "app", reasonImagePullBackOff)
	podB.Namespace = nsB

	if _, err := client.CoreV1().Pods(nsA).Create(context.Background(), podA, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create() pod A error = %v", err)
	}
	waitForClusterData(t, sub) // A's pod narration

	if _, err := client.CoreV1().Pods(nsB).Create(context.Background(), podB, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create() pod B error = %v", err)
	}
	waitForClusterData(t, sub) // B's pod narration

	evA := testEvent("evt-uid-a", "worker-1.a", corev1.EventTypeWarning)
	evB := testEvent("evt-uid-b", "worker-1.b", corev1.EventTypeWarning)
	evB.Namespace = nsB
	evB.InvolvedObject.Namespace = nsB

	if _, err := client.CoreV1().Events(nsA).Create(context.Background(), evA, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create() event A error = %v", err)
	}
	waitForClusterData(t, sub) // A's event narration

	if _, err := client.CoreV1().Events(nsB).Create(context.Background(), evB, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create() event B error = %v", err)
	}
	waitForClusterData(t, sub) // B's event narration

	o.mu.Lock()
	podsBefore, eventsBefore := len(o.pods), len(o.events)
	o.mu.Unlock()
	if podsBefore != 2 || eventsBefore != 2 {
		t.Fatalf("setup: o.pods = %d, o.events = %d, want 2 and 2", podsBefore, eventsBefore)
	}

	// Tear down JUST namespace A -- reconcile's per-namespace removal loop
	// (a continuing run's scope shrinking), not stopAllLocked -- while B
	// stays live in the SAME scopedInformers instance.
	o.scoped.reconcile(scopeWith("run-1", nsB))

	o.mu.Lock()
	_, aPodTracked := o.pods[podKey(podA)]
	_, bPodTracked := o.pods[podKey(podB)]
	_, aEventTracked := o.events[eventInvolvedKey(evA)]
	_, bEventTracked := o.events[eventInvolvedKey(evB)]
	o.mu.Unlock()

	if aPodTracked {
		t.Error("namespace A's Pod condition survived its own teardown -- clearNamespacePods ignored ns (M12)")
	}
	if !bPodTracked {
		t.Error("namespace B's Pod condition was wiped by namespace A's teardown -- clearNamespacePods ignored ns (M12)")
	}
	if aEventTracked {
		t.Error("namespace A's Event condition survived its own teardown -- clearNamespaceEvents ignored ns (M13)")
	}
	if !bEventTracked {
		t.Error("namespace B's Event condition was wiped by namespace A's teardown -- clearNamespaceEvents ignored ns (M13)")
	}

	// withNamespaceLive discrimination (M14): a write for the torn-down
	// namespace A must be refused even though B is STILL live --
	// len(s.entries) > 0 is true purely because of B.
	var wroteA bool
	o.scoped.withNamespaceLive(nsA, func() { wroteA = true })
	if wroteA {
		t.Error("withNamespaceLive admitted a write for a torn-down namespace because an unrelated namespace is still live (M14)")
	}

	// And B's own liveness must still work -- proves this isn't "always
	// refuse", which would pass the assertion above for the wrong reason.
	var wroteB bool
	o.scoped.withNamespaceLive(nsB, func() { wroteB = true })
	if !wroteB {
		t.Error("withNamespaceLive refused a write for the still-live namespace B -- setup/regression failure")
	}
}
