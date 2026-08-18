package observer

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/mchmarny/aicrme/internal/bus"
)

// scopeWith builds a RunScope the way main's newObserverScopeFn would for a
// resolved run: a non-empty RunID paired with a namespace set.
func scopeWith(runID string, ns ...string) RunScope {
	set := make(map[string]struct{}, len(ns))
	for _, n := range ns {
		set[n] = struct{}{}
	}
	return RunScope{RunID: runID, Namespaces: set}
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
	s := newScopedInformers(client)

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
// scope must yield three distinct factories, not one namespace-filtered one.
func TestScopedInformersStartOncePerNamespace(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := newScopedInformers(client)

	s.reconcile(scopeWith("run-1", "gpu-operator", "kai-scheduler", "ai-runtime"))

	if got := len(s.entries); got != 3 {
		t.Fatalf("entries = %d, want 3 (one per namespace)", got)
	}
	seen := make(map[string]bool, 3)
	factories := make(map[interface{}]bool, 3)
	for ns, e := range s.entries {
		seen[ns] = true
		factories[e.factory] = true
		if e.pod == nil || e.event == nil {
			t.Errorf("namespace %q: pod/event informer not materialized", ns)
		}
	}
	for _, ns := range []string{"gpu-operator", "kai-scheduler", "ai-runtime"} {
		if !seen[ns] {
			t.Errorf("namespace %q has no factory entry", ns)
		}
	}
	if len(factories) != 3 {
		t.Fatalf("distinct factories = %d, want 3 (one factory per namespace, never shared)", len(factories))
	}
}

// TestScopedInformersStopWhenTheRunEnds is Ruling 3: a run's terminal
// transition tears down its factories immediately, with no grace window.
// stopIfRunID is the production trigger (internal/observer's bus listener
// calls it on a terminal KindPhase event) -- exercised directly here so the
// property is proven independent of that wiring.
func TestScopedInformersStopWhenTheRunEnds(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := newScopedInformers(client)
	s.reconcile(scopeWith("run-1", "gpu-operator", "kai-scheduler"))
	if got := len(s.entries); got != 2 {
		t.Fatalf("entries before termination = %d, want 2", got)
	}

	s.stopIfRunID("run-1")

	if got := len(s.entries); got != 0 {
		t.Fatalf("entries after the run's terminal transition = %d, want 0", got)
	}
	if s.runID != "" {
		t.Errorf("runID = %q after termination, want empty", s.runID)
	}
}

// TestStopIfRunIDIgnoresAStaleRunID is stopIfRunID's own guard, the mirror of
// engine.go's epoch-guard pattern: a terminal event for a run this instance
// has already moved on from (reconcile already tore down and started a
// different run's factories) must not clobber the CURRENT run's set. Without
// the runID comparison, a late-arriving terminal event for run-1 would tear
// down run-2's live factories.
func TestStopIfRunIDIgnoresAStaleRunID(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := newScopedInformers(client)
	s.reconcile(scopeWith("run-1", "gpu-operator"))
	s.reconcile(scopeWith("run-2", "kai-scheduler")) // supersedes run-1

	s.stopIfRunID("run-1") // stale: this instance has already moved to run-2

	if got := len(s.entries); got != 1 {
		t.Fatalf("entries after a stale run-1 termination = %d, want 1 (run-2's factory left alone)", got)
	}
	if _, ok := s.entries["kai-scheduler"]; !ok {
		t.Error("run-2's kai-scheduler factory was torn down by a stale run-1 termination")
	}
	if s.runID != "run-2" {
		t.Errorf("runID = %q, want run-2 (unaffected by the stale run-1 signal)", s.runID)
	}
}

// TestScopedInformersAreIdempotent is the property named in the brief: a
// repeated scope must not double-start. Comparing factory pointers, not just
// the count, is what actually proves it -- a naive reconcile that tore down
// and rebuilt on every call could still land on len(entries) == 3 while
// silently churning every factory underneath.
func TestScopedInformersAreIdempotent(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := newScopedInformers(client)
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

	s := newScopedInformers(client)
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
	s := newScopedInformers(client)

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

// TestScopedInformersDoNotReviveATerminatedRunsScope closes the gap
// stopIfRunID's own teardown would otherwise reopen: main's RunScope
// composition has no way to say "this run is over" other than the terminal
// bus event this package's run() method already consumes once (Ruling 3
// dictates aggressive teardown, but main.go's engine.Attribution/CurrentID
// keep reporting the terminated run's own RunID and Namespaces unchanged
// until a NEW run starts or the run is discarded -- see newRunScopeFn,
// cmd/aicrme/main.go). Without this guard, any later reconcile call that
// still observes the just-terminated run's scope (a stray duplicate event, a
// second phase marker) would restart the very informers stopIfRunID just
// tore down.
func TestScopedInformersDoNotReviveATerminatedRunsScope(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := newScopedInformers(client)
	sc := scopeWith("run-1", "gpu-operator")
	s.reconcile(sc)
	s.stopIfRunID("run-1")

	s.reconcile(sc) // main would still report this scope; must not revive it

	if got := len(s.entries); got != 0 {
		t.Fatalf("entries after reconciling the terminated run's own scope = %d, want 0", got)
	}
}

// TestScopedInformersRunStopsOnTerminalPhaseEvent proves the actual
// production trigger for Ruling 3: run() subscribes to the observer's own
// bus, and a KindPhase "run done"/"run failed" event for the currently
// scoped RunID tears the factories down immediately -- not on the next poll,
// there is no poll. It would fail if run() ignored KindPhase events, matched
// the wrong message strings, or reconciled instead of tearing down on a
// terminal message (which would restart the just-finished run, see
// TestScopedInformersDoNotReviveATerminatedRunsScope).
func TestScopedInformersRunStopsOnTerminalPhaseEvent(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := newScopedInformers(client)
	b := bus.New(64)

	var scopeMu scopeHolder
	scopeMu.set(scopeWith("run-1", "gpu-operator"))

	stop := make(chan struct{})
	runDone := make(chan struct{})
	go func() {
		s.run(scopeMu.get, b, stop)
		close(runDone)
	}()
	t.Cleanup(func() {
		close(stop)
		<-runDone
	})

	// Wake run() so it reconciles the initial scope into existence.
	b.Publish(bus.Event{RunID: "run-1", Kind: bus.KindPhase, Message: "phase complete"})
	waitFor(t, func() bool { return s.entryCount() == 1 })

	b.Publish(bus.Event{RunID: "run-1", Kind: bus.KindPhase, Message: "run done"})

	waitFor(t, func() bool { return s.entryCount() == 0 })
}

// scopeHolder lets the test above swap the RunScope run() reads without a
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

// entryCount reads s.entries under lock, so the test above can poll it from
// outside without racing run()'s own goroutine.
func (s *scopedInformers) entryCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// waitFor polls cond until it is true or a bounded deadline passes, so the
// test above does not need a fixed sleep to synchronize with run()'s own
// goroutine.
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
