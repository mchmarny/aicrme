package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/mchmarny/aicrme/internal/bus"
)

// waitForAttributionState polls Attribution() until pred reports true or the
// deadline expires. Used instead of a fixed sleep because the transitions
// under test happen on the engine's own goroutine, at a pace this test does
// not control.
func waitForAttributionState(t *testing.T, e *Engine, pred func(Attribution) bool) Attribution {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		a := e.Attribution()
		if pred(a) {
			return a
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for attribution condition, last = %+v", a)
		default:
		}
		time.Sleep(time.Millisecond)
	}
}

// currentEpoch reads e.epoch under lock. Tests below that call
// setActiveAction/clearActiveAction directly (rather than through a real
// Start/Retry-driven run) rely on New's zero-value epoch, but reading it
// explicitly here -- instead of passing a bare 0 at every call site -- keeps
// that an asserted fact rather than an assumption baked in six separate
// places.
func currentEpoch(e *Engine) uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.epoch
}

func waitForRunState(t *testing.T, e *Engine, id string, want State) *Run {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		r, err := e.Get(context.Background(), id)
		if err == nil && r.State == want {
			return r
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for state %q, last err=%v run=%+v", want, err, r)
		default:
		}
		time.Sleep(time.Millisecond)
	}
}

// busHasStartedMarker reports whether the bus has already retained a header
// marker for (name, index). Retained events are never un-retained within the
// ring's capacity, so this check is stable once true.
func busHasStartedMarker(b *bus.Bus, name string, index int) bool {
	for _, ev := range b.Replay(0) {
		if ev.Kind != bus.KindComponent {
			continue
		}
		var m componentMarker
		if err := json.Unmarshal(ev.Data, &m); err != nil {
			continue
		}
		if m.Name == name && m.Index == index && m.Status == componentStatusStarted {
			return true
		}
	}
	return false
}

// TestAttributionIsEmptyBeforeAnyRun pins the zero value: nothing has
// mutated e.attribution and e.current is nil, so every field must read as
// its zero value. Breaks if Attribution() ever synthesizes a non-empty
// RunID/Phase from a nil e.current, or if New returns a non-zero
// e.attribution.
func TestAttributionIsEmptyBeforeAnyRun(t *testing.T) {
	e := New(bus.New(8), NewMemoryStore())

	got := e.Attribution()
	want := Attribution{}
	if got != want {
		t.Errorf("Attribution() = %+v, want zero value %+v", got, want)
	}
}

// TestAttributionCarriesTheActiveAction pins setActiveAction's contract:
// after one transition, Attribution() reports that action's name, index, and
// total, attached to the current run's ID and phase. Generation is
// deliberately excluded from this comparison -- pinning its arithmetic is
// TestAttributionGenerationAdvancesOnEveryTransition's job alone, so the two
// tests fail independently rather than both tripping over the same missing
// `Generation++`. Breaks if setActiveAction writes the wrong field, or if
// Attribution() fails to compose RunID/Phase from e.current.
func TestAttributionCarriesTheActiveAction(t *testing.T) {
	e := New(bus.New(8), NewMemoryStore())
	e.mu.Lock()
	e.current = &Run{ID: "run-1", Phase: PhaseApply}
	e.mu.Unlock()

	e.setActiveAction(currentEpoch(e), "nvidia-driver-daemonset", 3, 14)

	got := e.Attribution()
	got.Generation = 0 // excluded -- see doc comment
	want := Attribution{
		RunID:        "run-1",
		Phase:        PhaseApply,
		ActiveAction: "nvidia-driver-daemonset",
		ActiveIndex:  3,
		ActiveTotal:  14,
	}
	if got != want {
		t.Errorf("Attribution() = %+v, want %+v (Generation excluded from this comparison)", got, want)
	}
}

// TestAttributionGenerationAdvancesOnEveryTransition pins that Generation is
// strictly increasing across independent transitions, so a consumer can
// detect a stale read without comparing every field. Breaks if the
// Generation++ is removed or made conditional (e.g. skipped when the name
// doesn't change).
func TestAttributionGenerationAdvancesOnEveryTransition(t *testing.T) {
	e := New(bus.New(8), NewMemoryStore())
	e.mu.Lock()
	e.current = &Run{ID: "run-1"}
	e.mu.Unlock()

	e.setActiveAction(currentEpoch(e), "a", 1, 3)
	g1 := e.Attribution().Generation

	e.setActiveAction(currentEpoch(e), "b", 2, 3)
	g2 := e.Attribution().Generation

	e.setActiveAction(currentEpoch(e), "c", 3, 3)
	g3 := e.Attribution().Generation

	if g1 == 0 {
		t.Errorf("generation after first transition = 0, want nonzero")
	}
	if g2 <= g1 || g3 <= g2 {
		t.Errorf("generations = (%d, %d, %d), want strictly increasing", g1, g2, g3)
	}
}

// headerThenReturnStep emits exactly one component header marker, blocks
// until released, and then returns -- succeeding or failing per cfg. It
// exists to drive the REAL runStep/finish wiring (not setActiveAction/
// clearActiveAction called directly), so
// TestAttributionClearsActiveActionOnLeavingApply exercises the actual call
// sites engine.go wires, not a stand-in for them. The release gate matters:
// without it, setting and clearing the action both happen inside one
// synchronous Run() call, and the test could observe only the
// already-cleared end state -- passing even if setActiveAction were never
// wired at all.
type headerThenReturnStep struct {
	phase   Phase
	fail    bool
	release chan struct{}
}

func (h *headerThenReturnStep) Phase() Phase       { return h.phase }
func (h *headerThenReturnStep) Requires() []string { return nil }
func (h *headerThenReturnStep) Run(_ context.Context, _ *Run, emit Emit) error {
	emit(bus.Event{
		Kind:      bus.KindComponent,
		Component: "driver",
		Data:      json.RawMessage(`{"name":"driver","index":1,"total":1,"status":"started"}`),
	})
	<-h.release
	if h.fail {
		return fmt.Errorf("manufactured failure")
	}
	return nil
}

// TestAttributionClearsActiveActionOnLeavingApply pins that a terminal state
// leaves RunID (still derivable from e.current) but clears ActiveAction --
// exercised on both the success and the failure exit from Apply, since
// engine.go wires clearActiveAction at both call sites independently. It
// first confirms the action was actually observed active (see
// headerThenReturnStep's doc comment for why that matters) before releasing
// the step and checking it was cleared. Breaks if either runStep branch's
// clearActiveAction call, or finish's defensive one, is removed -- or if
// setActiveAction is never wired in the first place.
func TestAttributionClearsActiveActionOnLeavingApply(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fail      bool
		wantState State
	}{
		{name: "success", fail: false, wantState: StateDone},
		{name: "failure", fail: true, wantState: StateFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			step := &headerThenReturnStep{phase: PhaseApply, fail: tc.fail, release: make(chan struct{})}
			e := New(bus.New(8), NewMemoryStore(), step)

			run, err := e.Start(context.Background())
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}

			mid := waitForAttributionState(t, e, func(a Attribution) bool { return a.ActiveAction != "" })
			if mid.ActiveAction != "driver" || mid.ActiveIndex != 1 || mid.ActiveTotal != 1 {
				t.Errorf("mid-Apply Attribution() = %+v, want ActiveAction=driver index=1 total=1", mid)
			}

			close(step.release)
			waitForRunState(t, e, run.ID, tc.wantState)

			got := e.Attribution()
			if got.ActiveAction != "" {
				t.Errorf("ActiveAction = %q after run reached %s, want empty", got.ActiveAction, tc.wantState)
			}
			if got.RunID != run.ID {
				t.Errorf("RunID = %q, want %q -- a terminal state must not lose the run's identity", got.RunID, run.ID)
			}
		})
	}
}

// TestAttributionIsReadAtomically pins that a concurrent reader never
// observes a torn snapshot -- an action's name paired with a different
// action's index. Breaks if setActiveAction's four field writes are split
// across more than one lock hold, or if Attribution() reads fields
// individually instead of copying the whole struct under one lock.
func TestAttributionIsReadAtomically(t *testing.T) {
	e := New(bus.New(8), NewMemoryStore())
	e.mu.Lock()
	e.current = &Run{ID: "run-1"}
	e.mu.Unlock()

	const transitions = 500
	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < transitions; i++ {
			e.setActiveAction(currentEpoch(e), fmt.Sprintf("component-%d", i), i, transitions)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			a := e.Attribution()
			if a.ActiveAction != "" {
				want := fmt.Sprintf("component-%d", a.ActiveIndex)
				if a.ActiveAction != want {
					t.Errorf("Attribution() = %+v: name %q does not match its own index %d (want %q) -- torn snapshot", a, a.ActiveAction, a.ActiveIndex, want)
				}
			}
			select {
			case <-stop:
				return
			default:
			}
		}
	}()

	wg.Wait()
}

// TestAttributionDoesNotCloneArtifacts proves the read path stays off
// Engine.Current(), which deep-copies every artifact. A large artifact on
// the current run makes the difference unmissable: cloning it would show up
// as an allocation proportional to its size on every single call, while a
// correct accessor's cost is a small, constant struct copy. Breaks if
// Attribution() (or anything it calls) ever touches e.current.Artifacts.
func TestAttributionDoesNotCloneArtifacts(t *testing.T) {
	const artifactSize = 5 * 1024 * 1024 // 5 MiB
	big := make([]byte, artifactSize)

	e := New(bus.New(8), NewMemoryStore())
	e.mu.Lock()
	e.current = &Run{
		ID:        "run-1",
		Artifacts: map[string][]byte{"snapshot.yaml": big},
		Decisions: map[string]string{},
	}
	e.mu.Unlock()
	e.setActiveAction(currentEpoch(e), "driver", 1, 1)

	const calls = 500
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < calls; i++ {
		got := e.Attribution()
		if got.ActiveAction != "driver" {
			t.Fatalf("Attribution() = %+v, want ActiveAction %q", got, "driver")
		}
	}
	runtime.ReadMemStats(&after)

	perCall := (after.TotalAlloc - before.TotalAlloc) / calls
	// A single clone of the 5 MiB artifact (Run.Clone's
	// append([]byte(nil), v...)) would allocate roughly artifactSize bytes
	// on EVERY call. 64 KiB is generous headroom for the struct copy and
	// lock bookkeeping while still failing hard the instant a clone creeps
	// in -- it is three orders of magnitude below artifactSize.
	const ceiling = 64 * 1024
	if perCall > ceiling {
		t.Errorf("Attribution() allocated ~%d bytes/call (artifact is %d bytes), want < %d -- looks like it is routed through Engine.Current()'s artifact clone rather than reading the cheap snapshot",
			perCall, artifactSize, ceiling)
	}
}

// markerStep emits `count` distinct component header markers back to back,
// with no pauses, so a concurrent reader gets many chances to observe the
// engine goroutine between publishing a marker and updating the attribution
// snapshot -- the exact window the ordering contract (design doc §2,
// "Marker ordering is part of the contract") exists to close.
type markerStep struct {
	count int
}

func (m *markerStep) Phase() Phase       { return PhaseApply }
func (m *markerStep) Requires() []string { return nil }
func (m *markerStep) Run(_ context.Context, _ *Run, emit Emit) error {
	for i := 1; i <= m.count; i++ {
		name := fmt.Sprintf("component-%d", i)
		emit(bus.Event{
			Kind:      bus.KindComponent,
			Component: name,
			Data:      json.RawMessage(fmt.Sprintf(`{"name":%q,"index":%d,"total":%d,"status":"started"}`, name, i, m.count)),
		})
	}
	return nil
}

// TestAttributionUpdateFollowsThePublish is the ordering test: it drives
// thousands of marker transitions through the REAL runStep/emit wiring
// (engine.go) while a pool of concurrent readers busy-poll Attribution(),
// and for every transition any of them observes, confirms the corresponding
// header marker is already retained on the bus. In the correct
// implementation this holds deterministically -- Publish() (including
// retain, under the bus's own lock) fully completes before setActiveAction
// begins, on the same goroutine, so any reader that observes the new value
// is guaranteed the publish already happened; retained events are never
// un-retained within the ring's capacity, so this can never flip true-to-
// false and produce a false alarm. Swap the two statements (bite-proof) and
// the guarantee is gone: the danger window is real but only nanoseconds
// wide, which is why this drives many back-to-back transitions against a
// pool of readers contending on the same lock the writer uses, rather than
// one reader hoping to get lucky once.
func TestAttributionUpdateFollowsThePublish(t *testing.T) {
	const transitions = 8000
	b := bus.New(transitions + 8)
	e := New(b, NewMemoryStore(), &markerStep{count: transitions})

	var violation string
	var mu sync.Mutex
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// GOMAXPROCS(0), not NumCPU(): NumCPU reports HOST cores regardless of
	// any CPU quota the process is actually scheduled under. Under a
	// cgroup-throttled container (e.g. GOMAXPROCS=2 on an 11-core host),
	// NumCPU spawns one busy-polling reader per host core, and that many
	// readers contending for two threads starves the writer driving 8000
	// synchronous transitions -- a deterministic false failure via
	// waitForRunState's timeout, not a flake. GOMAXPROCS(0) reports what the
	// runtime will actually schedule, which is what reader parallelism
	// should track.
	readers := runtime.GOMAXPROCS(0)
	if readers < 4 {
		readers = 4
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var lastGen uint64
			for {
				select {
				case <-stop:
					return
				default:
				}
				a := e.Attribution()
				if a.Generation == lastGen || a.ActiveAction == "" {
					continue
				}
				lastGen = a.Generation
				if !busHasStartedMarker(b, a.ActiveAction, a.ActiveIndex) {
					mu.Lock()
					if violation == "" {
						violation = fmt.Sprintf("Attribution() reported active action %q (index %d, generation %d) before its header marker reached the bus",
							a.ActiveAction, a.ActiveIndex, a.Generation)
					}
					mu.Unlock()
				}
			}
		}()
	}

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitForRunState(t, e, run.ID, StateDone)
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if violation != "" {
		t.Error(violation)
	}
}

// TestSupersededGoroutineCannotWriteAttribution pins setActiveAction's and
// clearActiveAction's epoch guard (Fix round 1): a write carrying an epoch
// that is no longer the live one must be a silent no-op, not a mutation of
// whatever run has since taken over e.attribution. Unreachable today through
// any public API -- Start and Retry both refuse to launch while a run is
// live, so two goroutines never legitimately hold different epochs for the
// same Engine at once -- so, like
// TestSupersededGoroutineCannotWriteState (engine_internal_test.go), this
// manufactures the condition directly by bumping e.epoch out from under a
// captured, now-stale epoch value, rather than relying on a race that no
// black-box test could produce.
func TestSupersededGoroutineCannotWriteAttribution(t *testing.T) {
	e := New(bus.New(8), NewMemoryStore())
	e.mu.Lock()
	e.current = &Run{ID: "run-1"}
	staleEpoch := e.epoch
	e.epoch++ // manufacture supersession; no public API can do this today
	liveEpoch := e.epoch
	e.mu.Unlock()

	e.setActiveAction(staleEpoch, "component-1", 1, 1)
	if got := e.Attribution(); got.ActiveAction != "" {
		t.Errorf("setActiveAction with a superseded epoch wrote ActiveAction = %q, want no-op", got.ActiveAction)
	}

	e.setActiveAction(liveEpoch, "component-2", 2, 2)
	if got := e.Attribution(); got.ActiveAction != "component-2" {
		t.Fatalf("setActiveAction with the live epoch = %+v, want ActiveAction = component-2 (test setup failure, not the guard under test)", got)
	}

	e.clearActiveAction(staleEpoch)
	if got := e.Attribution(); got.ActiveAction != "component-2" {
		t.Errorf("clearActiveAction with a superseded epoch = %+v, want no-op (ActiveAction still component-2)", got)
	}

	e.clearActiveAction(liveEpoch)
	if got := e.Attribution(); got.ActiveAction != "" {
		t.Errorf("clearActiveAction with the live epoch left ActiveAction = %q, want cleared", got.ActiveAction)
	}
}
