package engine_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
)

// recoverStore is a Store double for Recover's tests. Per Ruling 1
// (docs/superpowers/sdd/.../progress.md), it embeds a real store rather than
// leaving the embedded engine.Store nil: LoadCurrent and Save are the only
// methods overridden below, so Load and Delete fall through to the embedded
// memoryStore instead of panicking on a nil interface.
type recoverStore struct {
	engine.Store

	mu          sync.Mutex
	loadCurrent *engine.Run
	loadErr     error
	saveCalls   int
}

func newRecoverStore() *recoverStore {
	return &recoverStore{Store: engine.NewMemoryStore()}
}

func (s *recoverStore) LoadCurrent(context.Context) (*engine.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	if s.loadCurrent == nil {
		return nil, aicrerrors.New(aicrerrors.ErrCodeNotFound, "no current run")
	}
	return s.loadCurrent.Clone(), nil
}

func (s *recoverStore) Save(ctx context.Context, r *engine.Run) error {
	s.mu.Lock()
	s.saveCalls++
	s.mu.Unlock()
	return s.Store.Save(ctx, r)
}

func (s *recoverStore) SaveCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveCalls
}

// fourStepEngine returns an engine over Discover, Recommend, Bundle, Apply --
// the shape main.go assembles today -- so bundleStepIndex() resolves to 2 and
// StepIndex 3 represents "Apply not yet complete."
func fourStepEngine(store engine.Store) *engine.Engine {
	b := bus.New(64)
	return engine.New(b, store,
		newFakeStep(engine.PhaseDiscover),
		newFakeStep(engine.PhaseRecommend),
		newFakeStep(engine.PhaseBundle),
		newFakeStep(engine.PhaseApply),
	)
}

func baseRun(id string, state engine.State, phase engine.Phase, stepIndex int) *engine.Run {
	return &engine.Run{
		ID:        id,
		State:     state,
		Phase:     phase,
		Decisions: map[string]string{},
		Artifacts: map[string][]byte{},
		StepIndex: stepIndex,
		StartedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
}

// TestRecoverLandsNonTerminalRunsFailed covers every State a crash could
// plausibly interrupt: none of them has a goroutine behind it after a
// restart, so all three must land StateFailed with a distinguishable error.
func TestRecoverLandsNonTerminalRunsFailed(t *testing.T) {
	for _, state := range []engine.State{engine.StateIdle, engine.StateRunning, engine.StateAwaitingDecision} {
		t.Run(string(state), func(t *testing.T) {
			store := newRecoverStore()
			store.loadCurrent = baseRun("run-a", state, engine.PhaseDiscover, 0)
			e := fourStepEngine(store)

			if err := e.Recover(context.Background()); err != nil {
				t.Fatalf("Recover() error = %v", err)
			}
			got := e.Current()
			if got == nil {
				t.Fatal("Current() = nil, want the recovered run installed")
			}
			if got.State != engine.StateFailed {
				t.Errorf("State = %q, want %q", got.State, engine.StateFailed)
			}
			if !strings.Contains(got.Err, "interrupted") {
				t.Errorf("Err = %q, want it to contain %q", got.Err, "interrupted")
			}
		})
	}
}

// TestRecoverLeavesTerminalRunsAlone pins that StateDone and StateActive
// restore as-is -- neither implies a dead goroutine the way the non-terminal
// states do.
func TestRecoverLeavesTerminalRunsAlone(t *testing.T) {
	for _, state := range []engine.State{engine.StateDone, engine.StateActive} {
		t.Run(string(state), func(t *testing.T) {
			store := newRecoverStore()
			store.loadCurrent = baseRun("run-a", state, engine.PhaseRecommend, 1)
			e := fourStepEngine(store)

			if err := e.Recover(context.Background()); err != nil {
				t.Fatalf("Recover() error = %v", err)
			}
			got := e.Current()
			if got == nil {
				t.Fatal("Current() = nil, want the recovered run installed")
			}
			if got.State != state {
				t.Errorf("State = %q, want %q unchanged", got.State, state)
			}
			if got.StepIndex != 1 {
				t.Errorf("StepIndex = %d, want 1 unchanged", got.StepIndex)
			}
		})
	}
}

// TestRecoverRewindsAlreadyFailedRunAtApply is the blocker case this task
// exists to fix: a run that had ALREADY failed during Apply before the crash
// (not one the crash itself interrupted) must still rewind to Bundle, because
// the bundle directory died with the emptyDir either way. A rewind keyed on
// "was this run non-terminal when interrupted" would miss it and leave Retry
// dead on arrival against a bundle.path that no longer exists.
func TestRecoverRewindsAlreadyFailedRunAtApply(t *testing.T) {
	store := newRecoverStore()
	run := baseRun("run-a", engine.StateFailed, engine.PhaseApply, 3)
	run.Err = "helm upgrade --install failed: some component"
	store.loadCurrent = run
	e := fourStepEngine(store)

	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	got := e.Current()
	if got == nil {
		t.Fatal("Current() = nil, want the recovered run installed")
	}
	if got.State != engine.StateFailed {
		t.Errorf("State = %q, want %q", got.State, engine.StateFailed)
	}
	if got.StepIndex != 2 {
		t.Errorf("StepIndex = %d, want 2 (Bundle) -- a pre-crash Apply failure must rewind exactly like an interrupted one", got.StepIndex)
	}
}

// TestRecoverRewindsInterruptedRunAtApply is the companion case: a run that
// was StateRunning (mid-Apply) when the pod died must rewind the same way.
func TestRecoverRewindsInterruptedRunAtApply(t *testing.T) {
	store := newRecoverStore()
	store.loadCurrent = baseRun("run-a", engine.StateRunning, engine.PhaseApply, 3)
	e := fourStepEngine(store)

	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	got := e.Current()
	if got == nil {
		t.Fatal("Current() = nil, want the recovered run installed")
	}
	if got.State != engine.StateFailed {
		t.Errorf("State = %q, want %q", got.State, engine.StateFailed)
	}
	if got.StepIndex != 2 {
		t.Errorf("StepIndex = %d, want 2 (Bundle)", got.StepIndex)
	}
}

// TestRecoverDoesNotRewindBeforeBundle pins the clamp's other edge: a run
// that never reached Bundle has nothing to rewind.
func TestRecoverDoesNotRewindBeforeBundle(t *testing.T) {
	store := newRecoverStore()
	store.loadCurrent = baseRun("run-a", engine.StateFailed, engine.PhaseDiscover, 0)
	e := fourStepEngine(store)

	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	got := e.Current()
	if got == nil {
		t.Fatal("Current() = nil, want the recovered run installed")
	}
	if got.StepIndex != 0 {
		t.Errorf("StepIndex = %d, want 0 unchanged", got.StepIndex)
	}
}

// TestRecoverDoesNotRewindTerminalRuns pins that a StateDone run past Bundle
// keeps its StepIndex -- rewind is exclusively a StateFailed concern.
func TestRecoverDoesNotRewindTerminalRuns(t *testing.T) {
	store := newRecoverStore()
	store.loadCurrent = baseRun("run-a", engine.StateDone, engine.PhaseApply, 4)
	e := fourStepEngine(store)

	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	got := e.Current()
	if got == nil {
		t.Fatal("Current() = nil, want the recovered run installed")
	}
	if got.State != engine.StateDone {
		t.Errorf("State = %q, want %q unchanged", got.State, engine.StateDone)
	}
	if got.StepIndex != 4 {
		t.Errorf("StepIndex = %d, want 4 unchanged", got.StepIndex)
	}
}

// TestRecoverNotFoundIsACleanStart pins the common case: a store reporting
// ErrCodeNotFound is a cold start, not an error.
func TestRecoverNotFoundIsACleanStart(t *testing.T) {
	store := newRecoverStore() // loadCurrent left nil -> ErrCodeNotFound
	e := fourStepEngine(store)

	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v, want nil", err)
	}
	if got := e.Current(); got != nil {
		t.Errorf("Current() = %+v, want nil", got)
	}
	if e.StoreUnreadable() {
		t.Error("StoreUnreadable() = true, want false for a clean NotFound start")
	}
}

// TestRecoverUnreadableRecordDoesNotInstallOrOverwrite is the other half of
// "unreadable is not absent": a non-NotFound load failure must not look like
// a cold start. Recover must not install the (nonexistent, since it could not
// be read) run, AND it must swap the engine's store so that nothing this
// process subsequently does -- proved here with a real Start -- writes
// through the original, unreadable store. A transient blip must not let a
// new run overwrite a record that was merely unreadable at that moment.
func TestRecoverUnreadableRecordDoesNotInstallOrOverwrite(t *testing.T) {
	store := newRecoverStore()
	store.loadErr = aicrerrors.New(aicrerrors.ErrCodeInternal, "api server unreachable")
	e := fourStepEngine(store)

	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v, want nil", err)
	}
	if got := e.Current(); got != nil {
		t.Errorf("Current() = %+v, want nil", got)
	}
	if !e.StoreUnreadable() {
		t.Error("StoreUnreadable() = false, want true")
	}

	if _, err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if calls := store.SaveCalls(); calls != 0 {
		t.Errorf("original store received %d Save call(s) after Start, want 0 -- an unreadable record must never be overwritten", calls)
	}
}

// TestRecoverRejectsInvalidRecord pins that a record which decodes cleanly is
// not automatically trustworthy: each case below takes the same unreadable
// path as a load failure, not a partial install. Beyond the brief's required
// three (empty ID, unknown state, out-of-range StepIndex), this also covers
// the other two validateLoaded checks the brief and spec both name (an
// unrecognized Phase, a zero StartedAt) so the whole validation contract has
// direct coverage rather than half of it riding on the required cases alone.
func TestRecoverRejectsInvalidRecord(t *testing.T) {
	zeroStarted := baseRun("run-a", engine.StateFailed, engine.PhaseApply, 0)
	zeroStarted.StartedAt = time.Time{}

	cases := []struct {
		name string
		run  *engine.Run
	}{
		{
			name: "empty ID",
			run:  baseRun("", engine.StateFailed, engine.PhaseApply, 0),
		},
		{
			name: "unknown state",
			run:  baseRun("run-a", engine.State("bogus"), engine.PhaseApply, 0),
		},
		{
			name: "step index beyond the step slice",
			run:  baseRun("run-a", engine.StateFailed, engine.PhaseApply, 99),
		},
		{
			name: "unknown phase",
			run:  baseRun("run-a", engine.StateFailed, engine.Phase("bogus"), 0),
		},
		{
			name: "zero StartedAt",
			run:  zeroStarted,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newRecoverStore()
			store.loadCurrent = tc.run
			e := fourStepEngine(store)

			if err := e.Recover(context.Background()); err != nil {
				t.Fatalf("Recover() error = %v, want nil", err)
			}
			if got := e.Current(); got != nil {
				t.Errorf("Current() = %+v, want nil", got)
			}
			if !e.StoreUnreadable() {
				t.Error("StoreUnreadable() = false, want true")
			}
		})
	}
}

// TestRecoverRequiresExactlyOneBundleStep pins the fail-fast startup
// assertion: a rewind target that is ambiguous or absent is a programming
// error, and discovering it during a real recovery is the worst possible
// time.
func TestRecoverRequiresExactlyOneBundleStep(t *testing.T) {
	t.Run("zero Bundle steps", func(t *testing.T) {
		b := bus.New(64)
		e := engine.New(b, engine.NewMemoryStore(), newFakeStep(engine.PhaseDiscover))

		err := e.Recover(context.Background())
		if !errors.Is(err, engine.ErrStepConfig) {
			t.Errorf("Recover() error = %v, want it to match ErrStepConfig", err)
		}
	})

	t.Run("two Bundle steps", func(t *testing.T) {
		b := bus.New(64)
		e := engine.New(b, engine.NewMemoryStore(),
			newFakeStep(engine.PhaseBundle),
			newFakeStep(engine.PhaseBundle),
		)

		err := e.Recover(context.Background())
		if !errors.Is(err, engine.ErrStepConfig) {
			t.Errorf("Recover() error = %v, want it to match ErrStepConfig", err)
		}
	})
}

// TestStartSetsPhaseFromFirstStep pins the producer-side fix (Ruling 9): a
// run returned by Start already names a declared Phase before any step has
// executed, because Start sets it at construction rather than leaving it to
// the first step's own runStep/awaitDecisions call.
func TestStartSetsPhaseFromFirstStep(t *testing.T) {
	e := fourStepEngine(engine.NewMemoryStore())

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if run.Phase != engine.PhaseDiscover {
		t.Errorf("Phase = %q, want %q (fourStepEngine's first step)", run.Phase, engine.PhaseDiscover)
	}
}

// TestRecoverInstallsARunPersistedBeforeAnyStepRan is the regression test
// for the whole empty-Phase chain Ruling 9 fixes: a run persisted by Start's
// very first Save -- before any step has completed -- must still recover
// successfully rather than being rejected as unreadable. Without the fix, a
// crash landing in that (previously) phase-less window would have disabled
// persistence for the rest of the process, permanently: the record is never
// overwritten once the unreadable path swaps to a memory store.
func TestRecoverInstallsARunPersistedBeforeAnyStepRan(t *testing.T) {
	store := engine.NewMemoryStore()
	step := &ctxBlockingStep{phase: engine.PhaseDiscover, entered: make(chan struct{}, 1)}
	e1 := engine.New(bus.New(64), store, step)

	run, err := e1.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-step.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("step never entered")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = e1.CancelAndWait(ctx)
	})

	if run.Phase != engine.PhaseDiscover {
		t.Fatalf("Start()'s returned run has Phase = %q, want %q -- see TestStartSetsPhaseFromFirstStep",
			run.Phase, engine.PhaseDiscover)
	}

	// A second engine recovering against the same store, simulating a
	// restart while the first step was still in flight, must install this
	// run rather than taking the unreadable path.
	e2 := fourStepEngine(store)
	if err := e2.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if e2.StoreUnreadable() {
		t.Fatal("StoreUnreadable() = true, want a normal pre-first-step record to recover cleanly")
	}
	got := e2.Current()
	if got == nil {
		t.Fatal("Current() = nil, want the persisted pre-first-step run installed")
	}
	if got.ID != run.ID {
		t.Errorf("ID = %q, want %q", got.ID, run.ID)
	}
	if got.State != engine.StateFailed {
		t.Errorf("State = %q, want %q", got.State, engine.StateFailed)
	}
	if !strings.Contains(got.Err, "interrupted") {
		t.Errorf("Err = %q, want it to contain %q", got.Err, "interrupted")
	}
	if got.StepIndex != 0 {
		t.Errorf("StepIndex = %d, want 0 -- no step had completed", got.StepIndex)
	}
}

// TestStartWithNoStepsDoesNotPanic pins the guard: an engine built with no
// steps (test-only -- main.go always assembles a real step slice, and
// internal/api's test suite constructs engines this way deliberately, to
// exercise HTTP behavior unrelated to step execution) has nothing to derive
// an initial Phase from. Start must not index into the empty slice, and must
// keep completing the run rather than failing it -- returning an error here
// would break every internal/api test built on this construction, which
// currently asserts Start succeeds and the run reaches StateDone immediately.
func TestStartWithNoStepsDoesNotPanic(t *testing.T) {
	e := engine.New(bus.New(8), engine.NewMemoryStore())

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v, want nil", err)
	}
	if run.Phase != "" {
		t.Errorf("Phase = %q, want the zero value -- there is no step to derive it from", run.Phase)
	}

	final := waitState(t, e, run.ID, engine.StateDone)
	if final.StepIndex != 0 {
		t.Errorf("StepIndex = %d, want 0", final.StepIndex)
	}
}
