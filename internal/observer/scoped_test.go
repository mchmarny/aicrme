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
//
// This only reaches map bookkeeping and non-nil handles, both of which stay
// true for factories that were never started or that watch the whole
// cluster instead of their own namespace -- see
// TestScopedInformersWatchOnlyTheirOwnNamespace for the assertion that
// actually reaches those two properties.
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
	s := newScopedInformers(client)

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
	s := newScopedInformers(client)
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
	s := newScopedInformers(client)
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
// to StateRunning (internal/engine/engine.go:769) -- is enough on its own to
// restart it.
func TestScopedInformersRestartAfterRetryReusesTheRunID(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := newScopedInformers(client)
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
	// (engine.go:769, 810) -- Attribution() recomputes Terminal live off
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
	s := newScopedInformers(client)
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
	s := newScopedInformers(client)
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
