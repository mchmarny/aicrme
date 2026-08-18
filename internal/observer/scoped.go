package observer

import (
	"log/slog"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
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
// multi-namespace recipe means one SharedInformerFactory per namespace PER
// KIND -- two per namespace (Important 2, Task 5 fix round 1: Pod and Event
// cannot share one factory, see factoryEntry's doc comment), roughly twenty
// for the current ten-namespace recipe, not one namespace-filtered factory.
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

// factoryEntry is one namespace's Pod/Event watch: TWO SharedInformerFactory
// instances (not one) plus the informer materialized on each, and the stop
// channel that owns their lifetime independently of the process-wide stopCh
// the three cluster-scoped informers in observer.go share. Handlers are
// registered on pod and event in startNamespace, using the scopedHandlers
// this type's owning scopedInformers was constructed with -- this type
// itself only makes the informers exist and keeps their lifetime correct.
//
// Two factories, not one, is Important 2's fix (Task 5 fix round 1): a
// SharedInformerFactory's WithTweakListOptions applies to every informer
// built from it -- ONE factory-level field
// (client-go@v0.36.3/informers/factory.go:62,91,398 --
// `core.New(f, f.namespace, f.tweakListOptions)`), not a per-informer
// setting. Task 6 needs `FieldSelector: "type=Warning"` on the Event
// ListWatch only; `type` is not a supported Pod field selector, so sharing
// one factory between the two would make the Event tweak apply to the Pod
// ListWatch too and the API server would reject it -- this namespace's Pod
// informer, this task's entire deliverable, would never sync. Splitting the
// factory now means Task 6 carries its tweak on its own factory without
// reopening this file a second time.
type factoryEntry struct {
	podFactory   informers.SharedInformerFactory
	eventFactory informers.SharedInformerFactory
	pod          cache.SharedIndexInformer
	event        cache.SharedIndexInformer
	stop         chan struct{}
}

// scopedHandlers holds the ResourceEventHandlerDetailedFuncs each
// namespace's Pod and Event informers register with, on startNamespace's
// pod and event informers respectively. Both use
// ResourceEventHandlerDetailedFuncs -- not the plain
// ResourceEventHandlerFuncs Observer.register uses for the three
// cluster-scoped kinds -- because both need AddFunc's isInInitialList
// parameter to tell an informer's initial-list Add from a later one: Pods
// (Task 5) so a pod already broken before this process started is not
// narrated as newly broken, and Events (Task 6) because an Event is created
// once and never updated, so a Warning that arrives only as an Add would
// otherwise be silently indistinguishable from initial-list noise.
//
// Passed in at construction (newScopedInformers) rather than reached for
// through a package-level Observer reference: scopedInformers has no
// reference to the Observer that owns it, and giving it one just to reach
// two method values would make the dependency run the wrong direction. A
// zero-value field (every func nil) is registered safely -- each
// ResourceEventHandlerDetailedFuncs method checks its own func for nil
// before calling through -- which is what lets event stay unset until Task 6
// without any change here.
type scopedHandlers struct {
	pod   cache.ResourceEventHandlerDetailedFuncs
	event cache.ResourceEventHandlerDetailedFuncs
}

// scopedInformers owns the Pod/Event factories for whichever run is
// currently in scope. mu guards every field below it. In production,
// reconcile has exactly one caller, run, and run's own select over its bus
// and ticker arms (below) strictly serializes the two -- reconcile never
// actually runs concurrently with itself there. The lock exists for tests:
// several drive run on its own goroutine while polling entryCount, or
// calling reconcile directly, from the test's own goroutine, and that access
// pattern is genuinely concurrent even though production's is not.
type scopedInformers struct {
	client   kubernetes.Interface
	handlers scopedHandlers
	// onNamespaceStop, if non-nil, is called with a namespace's name every
	// time that namespace's factories are torn down -- both when its run
	// ends/changes (stopAllLocked) and when it individually drops out of a
	// continuing run's scope (reconcile's per-namespace removal loop).
	// Important 4 (Task 5 fix round 1): Task 4's teardown is immediate on
	// RunScope.Terminal, so a pod deleted after that point is never
	// delivered to onDelete, and nothing else would clear the namespace-
	// scoped state Task 5/6's handlers accumulate (Observer.pods here).
	// scopedInformers has no reference to the Observer that owns that
	// state -- same reasoning as scopedHandlers' own doc comment for why
	// this is a callback passed in at construction, not a field reached for
	// the other way.
	onNamespaceStop func(ns string)

	mu sync.Mutex
	// runID is the run these entries belong to, or "" when nothing is
	// scoped. Compared only to decide whether an incoming scope names a
	// DIFFERENT run (tear down and restart fresh) -- termination itself is
	// decided by sc.Terminal, never by anything stored here. Reset to "" on
	// every terminal transition (Ruling 8's contract, pinned by
	// TestScopedInformersStopWhenTheRunEnds) -- which is exactly why
	// everTornDownRunID below cannot reuse this field for Ruling 37's
	// purpose: by the time the NEXT reconcile call arrives, runID has
	// already forgotten which run it was.
	runID   string
	entries map[string]*factoryEntry
	// everTornDown records every namespace THE CURRENT RUN (see
	// everTornDownRunID) has torn down at least once (Ruling 32, Task 6 fix
	// round 3) -- the signal onPodAdd/onEventAdd need to tell a namespace's
	// first-ever sighting under the current run (an informer's initial list
	// is a snapshot that predates it -- suppress,
	// TestPodInitialListDoesNotNarrate's case) from a RESUMPTION after this
	// SAME run already discarded whatever it knew about that namespace
	// (clearNamespacePods/clearNamespaceEvents, wired as onNamespaceStop
	// below) -- narrate instead, because the prior narration is genuinely
	// gone on both sides, not because anything is newly happening.
	//
	// Ruling 37 (Task 6 fix round 4, replacing part of Ruling 32): scoped to
	// engine.Retry's RunID, not to the process. Ruling 32 originally kept
	// this for the process's whole life ("a brand-new run reusing a
	// namespace name qualifies identically") -- the whole-branch review
	// (finding I2) demonstrated that reasoning does not survive Kubernetes'
	// own 1h Event TTL: a Warning from run 1, still present when run 2
	// starts because it has not yet TTL-expired, has NO resolution path for
	// run 2 (the pod it was about is gone, so neither Ruling 23's edge nor
	// Ruling 26's pull can ever retract it) -- run 2 re-narrates run 1's
	// leftover onto its own rows, permanently. reconcile clears this map
	// (see everTornDownRunID) exactly when it detects the incoming scope
	// names a genuinely DIFFERENT run, which preserves Ruling 32's
	// motivating Retry case (the SAME RunID after a failure) exactly while
	// dropping the cross-run carry-over.
	everTornDown map[string]bool
	// everTornDownRunID is the RunID everTornDown's current contents belong
	// to. reconcile compares an incoming sc.RunID against THIS, not against
	// runID: runID is blank during the entire gap between a run reaching
	// Terminal and whatever reconcile call comes next (including a Retry's
	// own restart), so comparing against it cannot tell "the next
	// non-terminal call names the SAME run" (Retry -- keep everTornDown)
	// from "a DIFFERENT run" (clear it) -- both would look like a change
	// from blank. everTornDownRunID is never reset to "" for that reason;
	// it only ever moves to whatever RunID reconcile most recently started
	// tracking a live scope for, surviving the Terminal gap runID does not.
	everTornDownRunID string
}

func newScopedInformers(client kubernetes.Interface, handlers scopedHandlers, onNamespaceStop func(ns string)) *scopedInformers {
	return &scopedInformers{
		client:          client,
		handlers:        handlers,
		onNamespaceStop: onNamespaceStop,
		entries:         make(map[string]*factoryEntry),
		everTornDown:    make(map[string]bool),
	}
}

// reconcile starts and stops per-namespace factories so the live set matches
// sc. It never blocks: the only call that reaches the network (a namespace's
// initial List) happens inside startNamespace's own goroutine, never on this
// call's goroutine -- the same posture 2b-ii's async observer.Start uses,
// applied here because reconcile runs repeatedly over a run's lifetime
// rather than once at process start, so no caller can be trusted to
// remember to wrap it in a goroutine itself.
//
// Idempotent, including under concurrent calls from separate goroutines
// (which only tests make -- run's own bus and ticker arms are two cases of
// one select and never call this concurrently with themselves, see
// scopedInformers' doc comment): namespaces already present are left alone,
// so a repeated identical scope never restarts a factory that is already
// watching.
//
// Tears down every existing factory when sc says there is nothing (or
// nothing further) to watch -- sc.RunID == "" (no run, or the two composed
// sources disagree, Ruling 6), sc.Namespaces is empty, or sc.Terminal is
// true -- and when sc names a DIFFERENT run than the one currently tracked,
// since a prior run's factories are never valid for a new run's namespaces.
// sc.Terminal is re-read on every call, never cached: see the package doc
// comment above for why that -- not a remembered fact about a run ID -- is
// what makes a retried run (same ID, Terminal now false) resume watching.
//
// Ruling 37 (Task 6 fix round 4): also clears everTornDown, but ONLY when
// sc.RunID differs from everTornDownRunID -- the run whose teardown history
// that map currently reflects -- which is deliberately NOT the same
// condition as the s.runID check three lines below. runID is already ""
// here on every call that follows a terminal transition (this function's
// own first branch just reset it, on THIS call or an earlier one), so
// comparing sc.RunID against runID cannot tell a Retry (same RunID resuming
// after Terminal) from a genuinely new run (different RunID) -- both look
// like "runID changed from blank". everTornDownRunID survives that gap
// (see its own doc comment) specifically so this comparison can.
func (s *scopedInformers) reconcile(sc RunScope) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sc.RunID == "" || len(sc.Namespaces) == 0 || sc.Terminal {
		s.stopAllLocked()
		s.runID = ""
		return
	}

	if sc.RunID != s.everTornDownRunID {
		s.everTornDown = make(map[string]bool)
		s.everTornDownRunID = sc.RunID
	}

	if s.runID != sc.RunID {
		s.stopAllLocked()
		s.runID = sc.RunID
	}

	for ns, e := range s.entries {
		if _, ok := sc.Namespaces[ns]; !ok {
			s.stopNamespaceLocked(ns, e)
		}
	}
	for ns := range sc.Namespaces {
		if _, ok := s.entries[ns]; ok {
			continue // already watching this namespace -- idempotent, no double-start
		}
		s.entries[ns] = s.startNamespace(ns)
	}
}

// startNamespace builds one namespace's two factories (Important 2, Task 5
// fix round 1 -- Pod and Event each get their own) and materializes the Pod
// and Event informer on each, then starts them on their own goroutine so a
// wedged API server's initial List cannot stall reconcile's caller -- the
// bite-proof this file exists to satisfy (TestScopedInformerStartDoesNotBlock).
// Callers must hold s.mu; the goroutine launched here touches only the
// factory and stop values it closes over, never s itself, so it needs no
// lock of its own.
//
// AddEventHandler errors below are logged, not returned, unlike
// Observer.register's identical call (observer.go) -- a deliberate
// divergence, not an oversight (Minor finding, Task 5 fix round 1).
// register's caller is Start, which itself returns an error to main, so
// propagating fits naturally. reconcile -- startNamespace's only caller --
// runs off run()'s own goroutine with no caller waiting on a return value
// (RunScope-driven and level-triggered; see reconcile's own doc comment for
// why it never blocks), so there is no plumbing-compatible path to surface
// an error meaningfully without restructuring reconcile's fire-and-forget
// shape Task 4 established. AddEventHandler only errors when the informer
// has already stopped, which cannot happen here (stop is fresh, closed by
// nothing yet) -- unreachable in practice, logged defensively in case that
// invariant ever stops holding.
func (s *scopedInformers) startNamespace(ns string) *factoryEntry {
	stop := make(chan struct{})
	// Two factories, not one -- see factoryEntry's doc comment (Important 2,
	// Task 5 fix round 1) for why sharing one between Pod and Event would
	// make Task 6's Event-only field selector apply to the Pod ListWatch too.
	// eventWarningListOptions (events.go) is applied ONLY to eventFactory --
	// podFactory carries no WithTweakListOptions call at all, which is what
	// keeps `type=Warning` off the Pod ListWatch (TestScopedInformersWatchOnlyTheirOwnNamespace's
	// sibling, TestEventFieldSelectorAppliesOnlyToTheEventFactory, asserts
	// this against client.Actions() directly, not just against these two
	// calls looking different here).
	podFactory := informers.NewSharedInformerFactoryWithOptions(s.client, resyncPeriod, informers.WithNamespace(ns))
	eventFactory := informers.NewSharedInformerFactoryWithOptions(s.client, resyncPeriod,
		informers.WithNamespace(ns), informers.WithTweakListOptions(eventWarningListOptions))
	pod := podFactory.Core().V1().Pods().Informer()
	event := eventFactory.Core().V1().Events().Informer()

	// AddEventHandler only registers a listener; it does no network I/O and
	// cannot block, so it is safe here on reconcile's own goroutine -- the
	// same posture as the factory.Start/WaitForCacheSync pairs below, which
	// DO reach the network and are therefore pushed onto their own goroutine
	// instead.
	if _, err := pod.AddEventHandler(s.handlers.pod); err != nil {
		slog.Warn("scoped observer pod handler registration failed", "namespace", ns, "error", err)
	}
	if _, err := event.AddEventHandler(s.handlers.event); err != nil {
		slog.Warn("scoped observer event handler registration failed", "namespace", ns, "error", err)
	}

	go func() {
		podFactory.Start(stop)
		eventFactory.Start(stop)
		for typ, ok := range podFactory.WaitForCacheSync(stop) {
			if !ok {
				slog.Warn("scoped observer cache did not sync", "namespace", ns, "type", typ.String())
			}
		}
		for typ, ok := range eventFactory.WaitForCacheSync(stop) {
			if !ok {
				slog.Warn("scoped observer cache did not sync", "namespace", ns, "type", typ.String())
			}
		}
	}()

	return &factoryEntry{podFactory: podFactory, eventFactory: eventFactory, pod: pod, event: event, stop: stop}
}

// stopNamespaceLocked closes ns's factories, evicts its entry, marks ns as
// having been torn down (Ruling 32, Task 6 fix round 3 -- see everTornDown's
// own doc comment), and -- if the caller supplied one -- notifies
// onNamespaceStop so namespace-scoped state Task 5/6's handlers accumulate
// outside this package (Observer.pods) can be cleared in step with the
// informer that used to feed it (Important 4, Task 5 fix round 1). Callers
// must hold s.mu; onNamespaceStop runs synchronously on this call's
// goroutine, same posture as everything else reconcile does under lock --
// it is expected to be cheap in-memory bookkeeping, not I/O.
//
// This sweep alone is NOT sufficient to prevent Observer.pods from
// re-accumulating stale entries after teardown (Important B, Task 5 fix
// round 2): close(e.stop) does not stop the informer's sharedProcessor
// synchronously, so a Pod notification already queued for delivery keeps
// arriving at onPodAdd/onPodUpdate after this function returns, and a write
// from one of those would land after this sweep already ran -- a sweep
// cannot win a race against deliveries already in flight, however it is
// ordered or however many times it runs. withNamespaceLive is the other
// half: every write those handlers make is gated on ns still being in
// s.entries, checked under the SAME s.mu this function also uses, so a
// stale delivery declines to write instead of racing the sweep that
// already happened.
func (s *scopedInformers) stopNamespaceLocked(ns string, e *factoryEntry) {
	close(e.stop)
	delete(s.entries, ns)
	s.everTornDown[ns] = true
	if s.onNamespaceStop != nil {
		s.onNamespaceStop(ns)
	}
}

// stopAllLocked closes and releases every tracked namespace's factory.
// Callers must hold s.mu and are responsible for what the teardown means for
// s.runID afterward -- this only empties s.entries.
func (s *scopedInformers) stopAllLocked() {
	for ns, e := range s.entries {
		s.stopNamespaceLocked(ns, e)
	}
}

// withNamespaceLive runs fn only if ns is still tracked as live (present in
// s.entries), holding s.mu for the CHECK and for fn's ENTIRE execution --
// not just the check -- so a teardown, which also needs s.mu (via
// stopNamespaceLocked), cannot interleave between "ns is live" and whatever
// fn does about it (Important B, Task 5 fix round 2).
//
// Mirrors internal/engine's epoch/aliveLocked(epoch) idiom: a check made and
// then acted on after releasing the lock, or under a different one, is not
// a guard -- it is exactly as racy as no check at all, just less obviously
// so. See stopNamespaceLocked's doc comment for the specific race this
// closes: close(stop) does not stop an informer's sharedProcessor
// synchronously, so a notification already queued keeps being delivered to
// Pod/Event handlers after teardown returns, and those handlers must
// decline to write once torn down rather than race the sweep that already
// ran.
//
// fn is expected to be fast, in-memory work -- a single map read/write --
// never I/O or a bus publish. s.mu also gates reconcile() and every other
// namespace's own handler delivery, so anything slower here would serialize
// far more than the one write it exists to protect; pods.go's callers keep
// their o.publish calls outside fn for exactly this reason.
//
// Deliberately deferred, not closed: this checks existence in s.entries,
// not per-entry identity/generation. A namespace ns torn down and then
// RESTARTED under a brand-new *factoryEntry -- a different run reusing the
// same namespace name -- would make this return true again for a stale
// delivery from the OLD, already-torn-down informer's lingering backlog,
// since existence alone cannot tell "live under a new generation" from
// "live under the generation this delivery belongs to". Reaching that
// window needs a notification to survive not just close(stop) not
// stopping delivery synchronously (the race this function closes) but ALSO
// two full reconcile cycles -- teardown, then a second namespace's worth of
// startNamespace work -- typically minutes apart in production (Ruling 9's
// 2s floor is the FLOOR, not the common case; the fast path is a KindPhase
// bus event, and consecutive runs are operator-paced). Confirmed real, not
// yet worth a full generation counter for how rarely it is reachable;
// revisit if a future consumer needs the stronger guarantee.
func (s *scopedInformers) withNamespaceLive(ns string, fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[ns]; ok {
		fn()
	}
}

// isRestart reports whether ns has been torn down by this process at least
// once before (Ruling 32, Task 6 fix round 3 -- see everTornDown's own doc
// comment for the full reasoning). onPodAdd/onEventAdd call this to decide
// whether an informer's initial-list Add is a console's first-ever sighting
// of ns (false: suppress, seed silently) or a resumption after this SAME
// process already discarded whatever it knew about ns (true: narrate --
// the prior state is genuinely gone on both sides, not just newly quiet).
//
// Locked like every other read of scopedInformers' own state, and safe to
// call from a handler that does not otherwise hold s.mu: it takes and
// releases the lock itself for exactly this one read, the same shape
// currentPod uses for its own s.entries lookup above.
func (s *scopedInformers) isRestart(ns string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.everTornDown[ns]
}

// currentPod looks up ns/name directly in that namespace's own live Pod
// informer's cache -- a PULL, not the PUSH delivery every other Pod-related
// read in this package relies on. Ruling 26 (Task 6 fix round 2): the
// re-review probed a Warning narrating for a pod whose ONLY Pod-informer
// delivery was an initial-list Add that seedPodBaseline skips entirely for
// an already-healthy pod (podTrouble returns !ok, so seedPodBaseline's own
// `if !ok { return }` records nothing) -- o.pods then has no entry for that
// pod at all, indistinguishable from "never observed". A PUSH-only design
// has no way to tell those apart; a direct cache read does, because
// client-go's informer maintains the current object regardless of whether
// this package's own handlers chose to record anything about it.
//
// ok is false if the namespace is not currently live or the pod is not (or
// not yet) in the cache -- callers must treat that as "unknown", never as
// "healthy": resolving on an unknown is the false-positive direction this
// package's own comments elsewhere (podCondition.narrated,
// clearNamespacePods) already treat as the worse mistake to make.
func (s *scopedInformers) currentPod(ns, name string) (*corev1.Pod, bool) {
	s.mu.Lock()
	entry, live := s.entries[ns]
	s.mu.Unlock()
	// entry.pod is nil for a bare *factoryEntry a test seeds directly to
	// exercise handlers without a real informer (handlers_internal_test.go's
	// newTestObserver, and this package's own newTestObserver in
	// pods_test.go) -- calling GetIndexer() on a nil
	// cache.SharedIndexInformer panics, so this is treated the same as
	// "not live": unknown, never "healthy".
	if !live || entry.pod == nil {
		return nil, false
	}
	obj, exists, err := entry.pod.GetIndexer().GetByKey(ns + "/" + name)
	if err != nil || !exists {
		return nil, false
	}
	pod, ok := obj.(*corev1.Pod)
	return pod, ok
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
