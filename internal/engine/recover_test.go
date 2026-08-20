package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

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
	// loadErrLimit caps how many LoadCurrent calls return loadErr before
	// falling through to the loadCurrent/NotFound path: 0 means "every
	// call" (a deterministic failure fixture), 1 means "fail once, then
	// succeed" (a transient-blip fixture), and so on.
	loadErrLimit int
	loadCalls    int
	saveCalls    int
}

func newRecoverStore() *recoverStore {
	return &recoverStore{Store: engine.NewMemoryStore()}
}

func (s *recoverStore) LoadCurrent(context.Context) (*engine.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadCalls++
	if s.loadErr != nil && (s.loadErrLimit == 0 || s.loadCalls <= s.loadErrLimit) {
		return nil, s.loadErr
	}
	if s.loadCurrent == nil {
		return nil, aicrerrors.New(aicrerrors.ErrCodeNotFound, "no current run")
	}
	return s.loadCurrent.Clone(), nil
}

func (s *recoverStore) LoadCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadCalls
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

// fourStepEngineOn is fourStepEngine over a caller-supplied bus, for tests
// that need to read the events recovery publishes.
func fourStepEngineOn(b *bus.Bus, store engine.Store) *engine.Engine {
	return engine.New(b, store,
		newFakeStep(engine.PhaseDiscover),
		newFakeStep(engine.PhaseRecommend),
		newFakeStep(engine.PhaseBundle),
		newFakeStep(engine.PhaseApply),
	)
}

// fourStepEngine returns an engine over Discover, Recommend, Bundle, Apply --
// the shape main.go assembles today -- so bundleStepIndex() resolves to 2 and
// StepIndex 3 represents "Apply not yet complete."
func fourStepEngine(store engine.Store) *engine.Engine {
	return fourStepEngineOn(bus.New(64), store)
}

// testRunID matches validRunID's format (16 hex characters, mirroring
// newID's real output) so fixtures exercising other validateLoaded checks
// don't also, incidentally, fail the ID check.
const testRunID = "0123456789abcdef"

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
			store.loadCurrent = baseRun(testRunID, state, engine.PhaseDiscover, 0)
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

// TestRecoverClearsPendingOnInterruptedRun pins the Minor fix alongside the
// State flip: awaitDecisions writes Pending and StateAwaitingDecision
// together, so a recovered run landing StateFailed with Pending still
// populated is self-inconsistent -- Pending implies a decision gate is
// still open, and after a restart no awaitDecisions goroutine exists to
// read an answer. Left uncleared, Task 4/6 would have to reason about a
// combination the state machine never otherwise produces.
func TestRecoverClearsPendingOnInterruptedRun(t *testing.T) {
	store := newRecoverStore()
	run := baseRun(testRunID, engine.StateAwaitingDecision, engine.PhaseRecommend, 1)
	run.Pending = []string{"intent", "platform"}
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
	if len(got.Pending) != 0 {
		t.Errorf("Pending = %v, want empty -- no awaitDecisions goroutine exists to read an answer after a restart", got.Pending)
	}
}

// TestRecoverClearsPendingOnAlreadyTerminalRun is the case
// TestRecoverClearsPendingOnInterruptedRun could not reach, because the clear
// used to live inside Recover's isLive||StateIdle branch: a record that is
// ALREADY terminal on disk with Pending still populated. That is not a
// hypothetical shape -- a SIGTERM at a decision gate makes awaitDecisions
// take its ctx.Done() branch and finish() write StateFailed over a run whose
// Pending awaitDecisions had just set, so StateFailed+Pending is exactly what
// the console persists on every shutdown-at-a-gate. StateDone and StateActive
// cover the terminal states the branch never touched at all.
func TestRecoverClearsPendingOnAlreadyTerminalRun(t *testing.T) {
	for _, state := range []engine.State{engine.StateFailed, engine.StateDone, engine.StateActive} {
		t.Run(string(state), func(t *testing.T) {
			store := newRecoverStore()
			run := baseRun(testRunID, state, engine.PhaseRecommend, 1)
			run.Pending = []string{"intent", "platform"}
			store.loadCurrent = run
			e := fourStepEngine(store)

			if err := e.Recover(context.Background()); err != nil {
				t.Fatalf("Recover() error = %v", err)
			}
			got := e.Current()
			if got == nil {
				t.Fatal("Current() = nil, want the recovered run installed")
			}
			if got.State != state {
				t.Errorf("State = %q, want %q unchanged -- this run was already terminal", got.State, state)
			}
			if len(got.Pending) != 0 {
				t.Errorf("Pending = %v, want empty -- no awaitDecisions goroutine survives a restart in any state", got.Pending)
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
			store.loadCurrent = baseRun(testRunID, state, engine.PhaseRecommend, 1)
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
	run := baseRun(testRunID, engine.StateFailed, engine.PhaseApply, 3)
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
	store.loadCurrent = baseRun(testRunID, engine.StateRunning, engine.PhaseApply, 3)
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
	store.loadCurrent = baseRun(testRunID, engine.StateFailed, engine.PhaseDiscover, 0)
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
	store.loadCurrent = baseRun(testRunID, engine.StateDone, engine.PhaseApply, 4)
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
// a cold start. This fixture fails EVERY attempt with ErrCodeInternal --
// the plausibly-transient class loadCurrentWithRetry retries -- so getting
// here means retries were exhausted, not that the first error degraded
// immediately (see TestRecoverDoesNotRetryDeterministicLoadFailure for that
// case). Recover must not install the (nonexistent, since it could not be
// read) run, AND it must swap the engine's store so that nothing this
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
	if calls := store.LoadCalls(); calls <= 1 {
		t.Errorf("LoadCurrent was called %d time(s), want more than 1 -- an always-ErrCodeInternal failure should exhaust the retry budget, not degrade on the first attempt", calls)
	}

	if _, err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if calls := store.SaveCalls(); calls != 0 {
		t.Errorf("original store received %d Save call(s) after Start, want 0 -- an unreadable record must never be overwritten", calls)
	}
}

// TestRecoverRetriesTransientLoadFailure pins Ruling 10: a LoadCurrent
// failure that fails once with ErrCodeInternal -- the shape cmstore
// produces for a failed Get, plausibly a control-plane blip sharing an
// origin with the pod restart itself -- then succeeds, must recover the run
// rather than degrading on the first error.
func TestRecoverRetriesTransientLoadFailure(t *testing.T) {
	store := newRecoverStore()
	store.loadErr = aicrerrors.New(aicrerrors.ErrCodeInternal, "api server unreachable")
	store.loadErrLimit = 1 // fails once, then the loadCurrent below is returned
	store.loadCurrent = baseRun(testRunID, engine.StateRunning, engine.PhaseDiscover, 0)
	e := fourStepEngine(store)

	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v, want nil", err)
	}
	if e.StoreUnreadable() {
		t.Error("StoreUnreadable() = true, want false -- a transient blip that resolves within the retry budget must not degrade")
	}
	got := e.Current()
	if got == nil {
		t.Fatal("Current() = nil, want the run installed after the retry succeeded")
	}
	if got.State != engine.StateFailed {
		t.Errorf("State = %q, want %q", got.State, engine.StateFailed)
	}
	if calls := store.LoadCalls(); calls != 2 {
		t.Errorf("LoadCurrent was called %d time(s), want 2 (one failure, one success)", calls)
	}
}

// TestRecoverDoesNotRetryDeterministicLoadFailure pins the other half of
// Ruling 10: a load failure that is not the plausibly-transient
// ErrCodeInternal shape -- ErrCodeInvalidRequest here, representing a
// decode failure, an unsupported schema version, or a missing payload key
// -- fails identically on every attempt, so Recover must not retry it. The
// call count is the assertion that matters: without it, a broken "retry
// everything" implementation would pass the degraded-outcome check too.
func TestRecoverDoesNotRetryDeterministicLoadFailure(t *testing.T) {
	store := newRecoverStore()
	store.loadErr = aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "run checkpoint is missing its payload key")
	e := fourStepEngine(store)

	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v, want nil", err)
	}
	if !e.StoreUnreadable() {
		t.Error("StoreUnreadable() = false, want true")
	}
	if calls := store.LoadCalls(); calls != 1 {
		t.Errorf("LoadCurrent was called %d time(s), want exactly 1 -- a deterministic failure must not be retried", calls)
	}
}

// TestRecoverLoadRetryRespectsContextCancellation pins that the retry
// backoff is genuinely bounded by the caller's context, not a fixed sleep:
// an already-canceled context must stop the retry loop immediately rather
// than waiting out the backoff, and Recover must still degrade cleanly
// (never hang, never panic) rather than treating the cancellation as some
// third outcome.
func TestRecoverLoadRetryRespectsContextCancellation(t *testing.T) {
	store := newRecoverStore()
	store.loadErr = aicrerrors.New(aicrerrors.ErrCodeInternal, "api server unreachable")
	e := fourStepEngine(store)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled before Recover ever calls LoadCurrent

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := e.Recover(ctx); err != nil {
			t.Errorf("Recover() error = %v, want nil", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Recover did not return promptly against an already-canceled context")
	}

	if !e.StoreUnreadable() {
		t.Error("StoreUnreadable() = false, want true -- a canceled retry must still degrade rather than install nothing silently")
	}
	if calls := store.LoadCalls(); calls != 1 {
		t.Errorf("LoadCurrent was called %d time(s), want exactly 1 -- cancellation during backoff must stop before a second attempt", calls)
	}
}

// TestRecoverRejectsInvalidRecord pins that a record which decodes cleanly is
// not automatically trustworthy: each case below takes the same unreadable
// path as a load failure, not a partial install. Beyond the brief's required
// three (empty ID, unknown state, out-of-range StepIndex), this also covers
// the rest of validateLoaded's contract per the spec and Ruling 9/10's
// follow-up: an unrecognized Phase, a zero StartedAt, a malformed (wrong
// length or non-hex) ID, and a zero UpdatedAt.
func TestRecoverRejectsInvalidRecord(t *testing.T) {
	zeroStarted := baseRun(testRunID, engine.StateFailed, engine.PhaseApply, 0)
	zeroStarted.StartedAt = time.Time{}

	zeroUpdated := baseRun(testRunID, engine.StateFailed, engine.PhaseApply, 0)
	zeroUpdated.UpdatedAt = time.Time{}

	cases := []struct {
		name string
		run  *engine.Run
	}{
		{
			name: "empty ID",
			run:  baseRun("", engine.StateFailed, engine.PhaseApply, 0),
		},
		{
			// Not empty, not 16 hex characters -- validRunID must reject the
			// shape as well as the empty case, since newID never produces this.
			name: "malformed ID",
			run:  baseRun("not-a-valid-run-id", engine.StateFailed, engine.PhaseApply, 0),
		},
		{
			name: "unknown state",
			run:  baseRun(testRunID, engine.State("bogus"), engine.PhaseApply, 0),
		},
		{
			name: "step index beyond the step slice",
			run:  baseRun(testRunID, engine.StateFailed, engine.PhaseApply, 99),
		},
		{
			name: "unknown phase",
			run:  baseRun(testRunID, engine.StateFailed, engine.Phase("bogus"), 0),
		},
		{
			name: "zero StartedAt",
			run:  zeroStarted,
		},
		{
			name: "zero UpdatedAt",
			run:  zeroUpdated,
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

// waitForPersistedState polls store directly -- not the engine's in-memory
// Get -- until id's STORED record reports want or 2 seconds pass. Fix round
// 3's NEW-2: finish sets Run.State under e.mu but Saves the checkpoint only
// AFTER releasing it (engine.go), so a caller that treats an in-memory Get
// reflecting the terminal state as proof the CHECKPOINT is written races
// that Save. Confirmed non-hypothetical: this exact race made
// TestUnconfirmedCleanupSurvivesRestart fail once under `-race ./...`
// (recovering Err "interrupted by a console restart", StepIndex 0,
// CleanupUnconfirmed false -- the PRIOR, pre-failure checkpoint), and
// widening the window by 50ms made it fail every time. A restart test's
// whole point is what a SECOND process reading the STORE would see, so it
// must wait on the store, not on the first process's own memory.
func waitForPersistedState(t *testing.T, store engine.Store, id string, want engine.State) *engine.Run {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var r *engine.Run
	var err error
	for {
		r, err = store.Load(context.Background(), id)
		if err == nil && r.State == want {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("persisted record never reached state %q, last err=%v run=%+v", want, err, r)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestUnconfirmedCleanupSurvivesRestart is fix round 2's N1 regression:
// envelope.go is a hand-maintained projection -- its own doc comment says
// it deliberately does not reuse Run's json tags -- and Run.CleanupUnconfirmed
// went a full fix round without a producer there, so a pod restart silently
// dropped Ruling 12's guard while the OLD Run.Err text it replaced would
// have survived just fine. This is why NewMemoryStore cannot be used here:
// its Save/Load clone the Run struct directly in-process and never touch
// encodeRun/decodeRun at all, so it would report this field surviving
// correctly whether or not envelope.go actually carries it. The real
// ConfigMap-backed store, and a genuinely SECOND *engine.Engine instance
// over the same persisted record, is what simulates the pod restart spec
// §9's own recovered-StateActive flow describes.
func TestUnconfirmedCleanupSurvivesRestart(t *testing.T) {
	client := fake.NewSimpleClientset()
	store := engine.NewConfigMapStore(client, testNamespace, testName, testOwner())

	proveStep := newFakeStep(engine.PhaseProve)
	proveStep.err = fmt.Errorf("run failed: %w", engine.ErrUnconfirmedCleanup)
	before := engine.New(bus.New(64), store, newFakeStep(engine.PhaseBundle), proveStep)

	run, err := before.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	// waitForPersistedState, not waitState -- see its own doc comment
	// (fix round 3's NEW-2). before.Discard below reads e.current directly
	// (not the store), so it is unaffected by this race either way, but the
	// Recover() call after it is exactly the read this race can corrupt.
	failed := waitForPersistedState(t, store, run.ID, engine.StateFailed)
	if !failed.CleanupUnconfirmed {
		t.Fatalf("fixture run.CleanupUnconfirmed = false before restart, want true")
	}
	if discardErr := before.Discard(context.Background(), run.ID); discardErr == nil {
		t.Fatal("fixture check: Discard() succeeded before restart, want the unconfirmed-cleanup guard blocking")
	}

	// A genuinely second engine instance over the same underlying
	// ConfigMap, not a second call on `before` -- the shape an actual pod
	// restart produces.
	after := engine.New(bus.New(64), store, newFakeStep(engine.PhaseBundle), newFakeStep(engine.PhaseProve))
	if recoverErr := after.Recover(context.Background()); recoverErr != nil {
		t.Fatalf("Recover() error = %v", recoverErr)
	}
	recovered, err := after.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if recovered.State != engine.StateFailed || !recovered.CleanupUnconfirmed {
		t.Fatalf("recovered run = %+v, want StateFailed with CleanupUnconfirmed = true", recovered)
	}

	// Discard is not gated by recoveredPending (Retry and Discard are the
	// only two things that clear it), so this specifically exercises
	// Ruling 12's guard surviving the restart, not a different gate.
	if discardErr := after.Discard(context.Background(), run.ID); discardErr == nil {
		t.Error("Discard() succeeded on the recovered run -- Ruling 12's guard did not survive the restart")
	}
	// End to end: whatever the exact gate, the operator must still not be
	// able to start a new run over the unresolved orphan post-restart.
	if _, startErr := after.Start(context.Background()); startErr == nil {
		t.Error("Start() succeeded on the recovered run -- a new run started over an unconfirmed orphan after a restart")
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

// TestRecoverRestoresComponentState pins that a persisted run's per-component
// projection (Task 1's engine.ComponentState) survives the load-and-install
// path unmangled: names and their latest status are what let a recovered run
// redraw the pipeline instead of a bare failure.
func TestRecoverRestoresComponentState(t *testing.T) {
	store := newRecoverStore()
	run := baseRun(testRunID, engine.StateDone, engine.PhaseApply, 4)
	run.Components = []engine.ComponentState{
		{Name: "gpu-operator", Index: 1, Total: 2, Status: "installed"},
		{Name: "kai-scheduler", Index: 2, Total: 2, Status: "installed"},
	}
	store.loadCurrent = run
	e := fourStepEngine(store)

	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	got := e.Current()
	if got == nil {
		t.Fatal("Current() = nil, want the recovered run installed")
	}
	if len(got.Components) != 2 {
		t.Fatalf("Components = %+v, want exactly 2 rows, unchanged from the persisted record", got.Components)
	}
	want := map[string]engine.ComponentState{
		"gpu-operator":  {Name: "gpu-operator", Index: 1, Total: 2, Status: "installed"},
		"kai-scheduler": {Name: "kai-scheduler", Index: 2, Total: 2, Status: "installed"},
	}
	for _, c := range got.Components {
		if c != want[c.Name] {
			t.Errorf("Components row %+v, want %+v", c, want[c.Name])
		}
	}
}

// bootstrapComponentPayload mirrors the JSON shape Recover's bootstrap
// KindComponent events carry -- the same field names applier.ComponentData
// uses (internal/applier/parse.go), which is what the SPA's
// web/src/pipeline.ts isComponentData actually checks. Declared locally
// rather than imported: internal/engine must not depend on internal/applier.
type bootstrapComponentPayload struct {
	Name   string `json:"name"`
	Index  int    `json:"index"`
	Total  int    `json:"total"`
	Status string `json:"status"`
}

// TestRecoverPublishesBootstrapEvents pins the mechanism that lets a
// recovered run render as a pipeline with no frontend change: Recover
// publishes the run's identity and phase, one KindComponent event per
// persisted component row, and (since this run lands StateFailed) the
// interruption notice as a distinct KindError event.
//
// Every assertion below is exact-count-and-exact-value, not
// strings.Contains: the brief calls out that the last two tasks each shipped
// a test that passed while the property it named was broken, because a
// substring assertion also matched a different, louder event. Counting
// events by Kind and requiring an exact Message match closes that hole --
// a neighboring event cannot satisfy "there is exactly one KindPhase event
// and its Message is exactly 'run failed'".
func TestRecoverPublishesBootstrapEvents(t *testing.T) {
	store := newRecoverStore()
	run := baseRun(testRunID, engine.StateRunning, engine.PhaseApply, 3)
	run.Components = []engine.ComponentState{
		{Name: "gpu-operator", Index: 1, Total: 2, Status: "installed"},
		{Name: "kai-scheduler", Index: 2, Total: 2, Status: "failed"},
	}
	store.loadCurrent = run

	b := bus.New(64)
	e := engine.New(b, store,
		newFakeStep(engine.PhaseDiscover),
		newFakeStep(engine.PhaseRecommend),
		newFakeStep(engine.PhaseBundle),
		newFakeStep(engine.PhaseApply),
	)

	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	events := b.Replay(0)
	var componentEvents, phaseEvents, errorEvents, recoveredEvents []bus.Event
	for _, ev := range events {
		switch ev.Kind {
		case bus.KindComponent:
			componentEvents = append(componentEvents, ev)
		case bus.KindPhase:
			phaseEvents = append(phaseEvents, ev)
		case bus.KindError:
			errorEvents = append(errorEvents, ev)
		case bus.KindRecovered:
			recoveredEvents = append(recoveredEvents, ev)
		case bus.KindLog, bus.KindCluster, bus.KindDecision:
			// Bootstrap never publishes these kinds; nothing to collect.
		}
	}

	// The marker the console keys its retry/discard affordance off. Exactly
	// one, and first: everything after it describes a run the operator has
	// already been told is waiting on them.
	if len(recoveredEvents) != 1 {
		t.Fatalf("recovered events = %d, want exactly 1: %+v", len(recoveredEvents), recoveredEvents)
	}
	if events[0].Kind != bus.KindRecovered {
		t.Errorf("first published event Kind = %q, want %q", events[0].Kind, bus.KindRecovered)
	}
	if recoveredEvents[0].RunID != testRunID {
		t.Errorf("recovered event RunID = %q, want %q", recoveredEvents[0].RunID, testRunID)
	}
	if recoveredEvents[0].Phase != string(engine.PhaseApply) {
		t.Errorf("recovered event Phase = %q, want %q", recoveredEvents[0].Phase, engine.PhaseApply)
	}

	if len(componentEvents) != 2 {
		t.Fatalf("component events = %d, want exactly 2 (one per persisted row): %+v", len(componentEvents), componentEvents)
	}
	byName := map[string]bootstrapComponentPayload{}
	levelByName := map[string]bus.Level{}
	for _, ev := range componentEvents {
		if ev.RunID != testRunID {
			t.Errorf("component event RunID = %q, want %q", ev.RunID, testRunID)
		}
		var p bootstrapComponentPayload
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			t.Fatalf("unmarshal component event Data: %v (data=%s)", err, ev.Data)
		}
		byName[p.Name] = p
		levelByName[p.Name] = ev.Level
	}
	if got := byName["gpu-operator"]; got.Status != "installed" || got.Index != 1 || got.Total != 2 {
		t.Errorf("gpu-operator payload = %+v, want status=installed index=1 total=2", got)
	}
	if got := byName["kai-scheduler"]; got.Status != "failed" || got.Index != 2 || got.Total != 2 {
		t.Errorf("kai-scheduler payload = %+v, want status=failed index=2 total=2", got)
	}
	// A recovered failed component must carry the same severity a live one
	// does (internal/applier/parse.go's reFailed -> bus.LevelError), not a
	// uniform LevelInfo that flattens a failure into ordinary progress.
	if got := levelByName["gpu-operator"]; got != bus.LevelInfo {
		t.Errorf("gpu-operator Level = %q, want %q", got, bus.LevelInfo)
	}
	if got := levelByName["kai-scheduler"]; got != bus.LevelError {
		t.Errorf("kai-scheduler Level = %q, want %q", got, bus.LevelError)
	}

	// web/src/components/Wizard.tsx's deriveRunState sets state to 'failed'
	// only on an exact Message == "run failed" match -- the same wording
	// engine.go's finish() already uses for a live run, so the recovered
	// case renders through the identical path.
	if len(phaseEvents) != 1 {
		t.Fatalf("phase events = %d, want exactly 1: %+v", len(phaseEvents), phaseEvents)
	}
	if phaseEvents[0].RunID != testRunID {
		t.Errorf("phase event RunID = %q, want %q", phaseEvents[0].RunID, testRunID)
	}
	if phaseEvents[0].Phase != string(engine.PhaseApply) {
		t.Errorf("phase event Phase = %q, want %q", phaseEvents[0].Phase, engine.PhaseApply)
	}
	if phaseEvents[0].Message != "run failed" {
		t.Errorf("phase event Message = %q, want exactly %q", phaseEvents[0].Message, "run failed")
	}

	if len(errorEvents) != 1 {
		t.Fatalf("error events = %d, want exactly 1 (the interruption notice): %+v", len(errorEvents), errorEvents)
	}
	if errorEvents[0].RunID != testRunID {
		t.Errorf("error event RunID = %q, want %q", errorEvents[0].RunID, testRunID)
	}
	if want := "interrupted by a console restart"; errorEvents[0].Message != want {
		t.Errorf("error event Message = %q, want exactly %q", errorEvents[0].Message, want)
	}
}

// TestRecoverPublishesTheRecoveryMarkerInEveryPhaseAndState is the Go half of
// the Critical finding. TestRecoverPublishesBootstrapEvents exercises exactly
// one combination -- PhaseApply, StateRunning-flipped-to-failed -- and that is
// why nothing caught a console with no reachable operator action anywhere
// else: a restart at the Recommend decision gate (the longest idle window in
// the product, since it waits on a human) and a restart after any completed
// run both land outside it.
//
// Every phase a restart can interrupt, crossed with every state a recovered
// record can carry, must publish the marker AND must actually be gated: the
// marker is only worth anything if it is published exactly when Start is
// refusing, and refusing exactly when the marker is published. Asserting both
// together is what stops the two drifting into a console that says "waiting
// for you" while quietly accepting new runs, or blocks new runs while saying
// nothing.
func TestRecoverPublishesTheRecoveryMarkerInEveryPhaseAndState(t *testing.T) {
	phases := []engine.Phase{
		engine.PhaseDiscover, engine.PhaseRecommend, engine.PhaseBundle, engine.PhaseApply,
	}
	states := []engine.State{
		engine.StateIdle, engine.StateRunning, engine.StateAwaitingDecision,
		engine.StateFailed, engine.StateDone, engine.StateActive,
	}

	for _, phase := range phases {
		for _, state := range states {
			t.Run(string(phase)+"/"+string(state), func(t *testing.T) {
				store := newRecoverStore()
				store.loadCurrent = baseRun(testRunID, state, phase, 0)
				b := bus.New(64)
				e := fourStepEngineOn(b, store)

				if err := e.Recover(context.Background()); err != nil {
					t.Fatalf("Recover() error = %v", err)
				}

				var markers []bus.Event
				for _, ev := range b.Replay(0) {
					if ev.Kind == bus.KindRecovered {
						markers = append(markers, ev)
					}
				}
				if len(markers) != 1 {
					t.Fatalf("recovered markers = %d, want exactly 1: %+v", len(markers), markers)
				}
				if markers[0].RunID != testRunID {
					t.Errorf("marker RunID = %q, want %q", markers[0].RunID, testRunID)
				}
				if markers[0].Phase != string(phase) {
					t.Errorf("marker Phase = %q, want %q -- the console renders the phase it names", markers[0].Phase, phase)
				}

				// The gate the marker exists to explain. Without the marker
				// this 409 is what the operator hits with no way out.
				if _, err := e.Start(context.Background()); err == nil {
					t.Error("Start() error = nil, want a conflict -- a marker published for a run Start would happily replace is a lie")
				} else {
					var se *aicrerrors.StructuredError
					if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeConflict {
						t.Errorf("Start() error = %v, want ErrCodeConflict", err)
					}
				}

				// Discard is the affordance that must exist for every state
				// Retry refuses outright -- Scenario B: a completed run's
				// record survives, the next helm upgrade recovers it, Retry
				// answers "run is not in a failed state", and without Discard
				// the console would be bricked by an ordinary upgrade.
				//
				// StateActive is the one exception, and a restart does not
				// remove it: a recovered StateActive record names a workload
				// that may still be running in the cluster exactly as it
				// would for a live process's own StateActive run, so
				// Discard's job here is to refuse and name the same remedy --
				// see TestDiscardRejectsActiveRun, which pins the identical
				// invariant for the live-process case this loop cannot reach.
				if state == engine.StateActive {
					err := e.Discard(context.Background(), testRunID)
					if err == nil {
						t.Fatal("Discard() succeeded on a recovered active run -- it would orphan the workload")
					}
					var se *aicrerrors.StructuredError
					if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeConflict {
						t.Errorf("Discard() error = %v, want ErrCodeConflict", err)
					}
					if !strings.Contains(err.Error(), "stop") {
						t.Errorf("Discard() error = %q, want it to name stopping the workload", err)
					}
					return
				}
				if err := e.Discard(context.Background(), testRunID); err != nil {
					t.Fatalf("Discard() error = %v, want a recovered run discardable in every state", err)
				}
				if _, err := e.Start(context.Background()); err != nil {
					t.Errorf("Start() after Discard error = %v, want nil -- discard must free the console", err)
				}
			})
		}
	}
}
