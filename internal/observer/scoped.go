package observer

import (
	"log/slog"
	"sync"
	"time"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/mchmarny/aicrme/internal/bus"
)

// The observer starts at pod start, before any run exists -- the recipe's
// namespaces come from recipe.json, which Recommend writes partway through a
// run (docs/superpowers/specs/2026-08-17-aicrme-phase-2b-iii-design.md
// Section 3). So Pods and Events, unlike the three cluster-scoped informers
// in observer.go, cannot be watched from a factory built once at Start: this
// file gives them a lifecycle that starts once a scope resolves and tears
// down when the run that resolved it ends.
//
// informers.WithNamespace takes exactly one namespace (verified against the
// pinned client-go v0.36.3, informers/factory.go:99 -- "factory.namespace =
// namespace" is a single field, last write wins, not a set), so following a
// multi-namespace recipe means one SharedInformerFactory per namespace,
// roughly ten for the current recipe, not one namespace-filtered factory.
//
// reconcile is driven purely by RunScope.Terminal (Ruling 8), re-derived by
// main from engine.Attribution() on every call
// (cmd/aicrme/main.go:newObserverScopeFn) -- never by anything this package
// remembers about a run across calls. An earlier version of this file
// inferred "the run just ended" from a terminal KindPhase bus message and
// latched that fact in a `terminated` field keyed by run ID. That was wrong
// two independent ways, both structural rather than fixable by patching the
// inference:
//   - engine.Retry (internal/engine/engine.go) reuses the SAME run ID after a
//     failure. The latch, once set for that ID, had no path back to false
//     for it -- a retried run's informers stayed permanently wedged off.
//   - internal/bus drops live events for a subscriber more than
//     subscriberBuffer behind (bus.go), and this listener neither replays
//     nor reconnects. If the DROPPED event was the run's terminal one -- its
//     LAST event -- nothing else ever signaled termination, so teardown
//     never happened.
//
// Making the scope itself say "is this run over" (RunScope.Terminal) removes
// the need to infer or remember anything: reconcile reads it fresh every
// time, so a retried run (same ID, Terminal now false) simply is not
// terminal, and a dropped bus event costs one missed wakeup, not permanent
// divergence -- see run's doc comment for what bounds that cost.

// factoryEntry is one namespace's Pod/Event watch: its own
// SharedInformerFactory plus the two informers materialized on it, and the
// stop channel that owns their lifetime independently of the process-wide
// stopCh the three cluster-scoped informers in observer.go share. Handlers
// are not registered here -- that is Tasks 5 and 6's job; this type only
// makes the informers exist and keeps their lifetime correct.
type factoryEntry struct {
	factory informers.SharedInformerFactory
	pod     cache.SharedIndexInformer
	event   cache.SharedIndexInformer
	stop    chan struct{}
}

// scopedInformers owns the Pod/Event factories for whichever run is
// currently in scope. mu guards every field below it: reconcile can run
// concurrently with itself in production, since run() calls it from both the
// bus subscription and the ticker with nothing else serializing the two.
type scopedInformers struct {
	client kubernetes.Interface

	mu sync.Mutex
	// runID is the run these entries belong to, or "" when nothing is
	// scoped. Compared only to decide whether an incoming scope names a
	// DIFFERENT run (tear down and restart fresh) -- termination itself is
	// decided by sc.Terminal, never by anything stored here.
	runID   string
	entries map[string]*factoryEntry
}

func newScopedInformers(client kubernetes.Interface) *scopedInformers {
	return &scopedInformers{client: client, entries: make(map[string]*factoryEntry)}
}

// reconcile starts and stops per-namespace factories so the live set matches
// sc. It never blocks: the only call that reaches the network (a namespace's
// initial List) happens inside startNamespace's own goroutine, never on this
// call's goroutine -- the same posture 2b-ii's async observer.Start uses,
// applied here because reconcile runs repeatedly over a run's lifetime
// rather than once at process start, so no caller can be trusted to
// remember to wrap it in a goroutine itself.
//
// Idempotent and safe to call from multiple goroutines (run below does, from
// both the bus fast path and the ticker floor) or with an unchanged sc:
// namespaces already present are left alone, so a repeated identical scope
// never restarts a factory that is already watching.
//
// Tears down every existing factory when sc says there is nothing (or
// nothing further) to watch -- sc.RunID == "" (no run, or the two composed
// sources disagree, Ruling 6), sc.Namespaces is empty, or sc.Terminal is
// true -- and when sc names a DIFFERENT run than the one currently tracked,
// since a prior run's factories are never valid for a new run's namespaces.
// sc.Terminal is re-read on every call, never cached: see the package doc
// comment above for why that -- not a remembered fact about a run ID -- is
// what makes a retried run (same ID, Terminal now false) resume watching.
func (s *scopedInformers) reconcile(sc RunScope) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sc.RunID == "" || len(sc.Namespaces) == 0 || sc.Terminal {
		s.stopAllLocked()
		s.runID = ""
		return
	}

	if s.runID != sc.RunID {
		s.stopAllLocked()
		s.runID = sc.RunID
	}

	for ns, e := range s.entries {
		if _, ok := sc.Namespaces[ns]; !ok {
			close(e.stop)
			delete(s.entries, ns)
		}
	}
	for ns := range sc.Namespaces {
		if _, ok := s.entries[ns]; ok {
			continue // already watching this namespace -- idempotent, no double-start
		}
		s.entries[ns] = s.startNamespace(ns)
	}
}

// startNamespace builds one namespace's factory and materializes its Pod and
// Event informers, then starts them on their own goroutine so a wedged API
// server's initial List cannot stall reconcile's caller -- the bite-proof
// this file exists to satisfy (TestScopedInformerStartDoesNotBlock). Callers
// must hold s.mu; the goroutine launched here touches only the factory and
// stop values it closes over, never s itself, so it needs no lock of its
// own.
func (s *scopedInformers) startNamespace(ns string) *factoryEntry {
	stop := make(chan struct{})
	factory := informers.NewSharedInformerFactoryWithOptions(s.client, resyncPeriod, informers.WithNamespace(ns))
	pod := factory.Core().V1().Pods().Informer()
	event := factory.Core().V1().Events().Informer()

	go func() {
		factory.Start(stop)
		for typ, ok := range factory.WaitForCacheSync(stop) {
			if !ok {
				slog.Warn("scoped observer cache did not sync", "namespace", ns, "type", typ.String())
			}
		}
	}()

	return &factoryEntry{factory: factory, pod: pod, event: event, stop: stop}
}

// stopAllLocked closes and releases every tracked namespace's factory.
// Callers must hold s.mu and are responsible for what the teardown means for
// s.runID afterward -- this only empties s.entries.
func (s *scopedInformers) stopAllLocked() {
	for ns, e := range s.entries {
		close(e.stop)
		delete(s.entries, ns)
	}
}

// stop tears down every namespace's factory unconditionally. Used on process
// shutdown (run()'s stopCh closing), where there is no specific run's scope
// to reconcile against -- the process is exiting regardless of what any run
// is doing.
func (s *scopedInformers) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopAllLocked()
	s.runID = ""
}

// reconcileInterval is run's correctness floor, not its normal cadence: the
// bus subscription below is the fast path and fires on every KindPhase
// event, so in the common case teardown happens within microseconds of
// Engine.finish's Publish call, same as before this fix. The floor exists
// because internal/bus drops live events for a subscriber more than
// subscriberBuffer behind (bus.go) with no replay and no reconnect for this
// internal listener -- unlike an SSE client, which resumes with
// Last-Event-ID. If the DROPPED event is a run's terminal KindPhase -- its
// LAST event, since nothing publishes after finish() -- no later bus event
// for that run will ever arrive to trigger a reconcile, and the scope stays
// stuck reporting that run's now-stale Terminal:false understanding until a
// DIFFERENT run's first KindPhase happens to fire (unbounded if the operator
// never starts another run). The ticker guarantees this goroutine re-reads
// scope() -- which recomputes Terminal fresh from engine state, not from
// anything this goroutine remembered -- at least once per interval
// regardless of what the bus delivered.
//
// reconcile is a handful of map operations against already-in-memory state;
// the only I/O it can trigger (a per-namespace factory (re)start) runs off
// its own goroutine (startNamespace), so ticking is cheap. 2s bounds the
// worst-case teardown lag from "instant" to "still well under
// human-perceptible" for a console whose shortest phase (Discover) runs
// minutes, without generating meaningful lock contention or CPU churn over a
// run's lifetime.
const reconcileInterval = 2 * time.Second

// run drives the scoped lifecycle for the observer's whole process lifetime:
// a level-triggered reconcile against the CURRENT scope, woken by two
// independent sources that both converge on the same reconcile(scope()) call
// -- the observer's own bus, as the fast path, and a ticker at the given
// interval, as the correctness floor (see reconcileInterval's doc comment
// for why the floor is necessary; Observer.Start passes that constant as
// interval, and tests pass their own to exercise one path at a time without
// waiting out the production period). Neither source is trusted alone: every
// KindPhase event triggers a reconcile, not just a terminal one, because
// namespaces resolve partway through a run (Recommend writing recipe.json)
// with no dedicated event marking that moment -- the next phase-complete
// marker is what wakes this goroutine to notice a newly resolved scope.
func (s *scopedInformers) run(scope func() RunScope, b *bus.Bus, stopCh <-chan struct{}, interval time.Duration) {
	sub, unsub := b.Subscribe(0)
	defer unsub()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			s.stop()
			return
		case e, ok := <-sub:
			if !ok {
				s.stop()
				return
			}
			if e.Kind != bus.KindPhase {
				continue
			}
			s.reconcile(scope())
		case <-ticker.C:
			s.reconcile(scope())
		}
	}
}
