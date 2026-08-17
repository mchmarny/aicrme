package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mchmarny/aicrme/internal/bus"
)

// slowStep blocks in Run until released, then writes an Artifact. It exists
// to hold a runStep call inside step.Run long enough for the test to
// manufacture supersession while that call is still in flight.
type slowStep struct {
	release chan struct{}
	entered chan struct{}
}

func (s *slowStep) Phase() Phase       { return PhaseApply }
func (s *slowStep) Requires() []string { return nil }
func (s *slowStep) Run(_ context.Context, r *Run, _ Emit) error {
	close(s.entered)
	<-s.release
	r.Artifacts["late"] = []byte("written-after-supersede")
	return nil
}

// TestSupersededGoroutineCannotWriteState pins runStep's merge-back
// aliveLocked check. It deliberately constructs a condition the public API
// cannot currently produce: today, Retry only launches a second execute
// goroutine once State == StateFailed, and StateFailed is set exclusively
// by the first goroutine's own terminal finish() call -- by the time any
// caller can legally observe Failed and call Retry, the first goroutine has
// already done its last piece of shared-state work, so no black-box test
// can catch a superseded goroutine still trying to write. This test bumps
// e.epoch directly (as a future Reset, per docs/phase-2-handoff.md, would
// need to) while a step is still inside Run, to prove the merge-back check
// actually stops a superseded write rather than merely looking like it does.
func TestSupersededGoroutineCannotWriteState(t *testing.T) {
	step := &slowStep{release: make(chan struct{}), entered: make(chan struct{})}
	e := New(bus.New(8), NewMemoryStore(), step)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	<-step.entered // runStep is inside step.Run, holding the epoch captured at launch

	e.mu.Lock()
	e.epoch++ // manufacture supersession; no public API can do this today
	e.mu.Unlock()

	close(step.release) // let step.Run return so runStep reaches the merge-back block

	// A fixed wait, not a poll-until-found: this asserts an absence, and the
	// merge-back block (a handful of map writes and a mutex round trip) has
	// nothing left to do it after this window even under -race's overhead.
	time.Sleep(200 * time.Millisecond)

	got, err := e.Get(run.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if _, ok := got.Artifacts["late"]; ok {
		t.Error("merge-back wrote Artifacts after supersession -- the epoch check did not stop it")
	}
}

// supersedingFailStore fails every Save, but first bumps e.epoch directly --
// manufacturing, from inside the Save call itself, the same "no longer the
// live goroutine" condition slowStep manufactures from outside. This is the
// only way this package can reach Retry's rollback with aliveLocked(epoch)
// false: the public Retry API has no legal way to launch a second live run
// for the same ID while a first Retry's Save call is still in flight, for
// the identical reason TestSupersededGoroutineCannotWriteState's doc comment
// gives for runStep's merge-back.
type supersedingFailStore struct {
	Store
	e *Engine
}

func (s *supersedingFailStore) Save(context.Context, *Run) error {
	s.e.mu.Lock()
	s.e.epoch++
	s.e.mu.Unlock()
	return errors.New("save failed")
}

// TestRetryRollbackDoesNotRestoreRecoveryPendingAgainstASupersededRun pins
// Ruling 11's guard requirement: restoring recoveredPending on a failed
// Retry Save must happen inside the same aliveLocked-guarded block that
// restores State and Err, not unconditionally. An unguarded restore would
// set recoveredPending against whatever run has since taken over the ID --
// possibly one that legitimately superseded it -- and 409 that run's Start
// calls forever, trading the bug this task fixes for a permanent one.
func TestRetryRollbackDoesNotRestoreRecoveryPendingAgainstASupersededRun(t *testing.T) {
	store := &supersedingFailStore{Store: NewMemoryStore()}
	e := New(bus.New(8), store)
	store.e = e

	run := &Run{
		ID:        "0123456789abcdef",
		State:     StateFailed,
		Decisions: map[string]string{},
		Artifacts: map[string][]byte{},
		StartedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	e.mu.Lock()
	e.current = run
	e.recoveredPending = true
	e.mu.Unlock()

	if _, err := e.Retry(run.ID); err == nil {
		t.Fatal("Retry() error = nil, want the manufactured Save failure to surface")
	}

	e.mu.Lock()
	got := e.recoveredPending
	e.mu.Unlock()
	if got {
		t.Error("recoveredPending restored true after the run was superseded mid-Save -- the rollback's aliveLocked guard did not cover it")
	}
}

// TestDecideRollbackDoesNotRestoreStateAgainstASupersededRun pins the same
// guard class Task 4's Retry rollback needed pinned one commit earlier
// (Ruling 13, see engine.go's own "a guard no test can break is not a
// guard"): Decide's failed-Save rollback must skip restoring
// Decisions/Pending/State/UpdatedAt once this goroutine's epoch is no
// longer the live one, or it stomps whatever legitimately superseded this
// run in the interim. Reuses supersedingFailStore (defined above for
// Retry's identical test) rather than inventing a second fake for the same
// job -- it bumps e.epoch from inside Save itself, manufacturing the one
// condition no public API can produce today, for the identical reason
// TestSupersededGoroutineCannotWriteState's doc comment gives.
func TestDecideRollbackDoesNotRestoreStateAgainstASupersededRun(t *testing.T) {
	store := &supersedingFailStore{Store: NewMemoryStore()}
	e := New(bus.New(8), store)
	store.e = e

	run := &Run{
		ID:        "0123456789abcdef",
		State:     StateAwaitingDecision,
		Pending:   []string{"apply"},
		Decisions: map[string]string{},
		Artifacts: map[string][]byte{},
		StartedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	e.mu.Lock()
	e.current = run
	e.mu.Unlock()

	if err := e.Decide(run.ID, map[string]string{"apply": "yes"}); err == nil {
		t.Fatal("Decide() error = nil, want the manufactured Save failure to surface")
	}

	e.mu.Lock()
	state := e.current.State
	e.mu.Unlock()
	if state == StateAwaitingDecision {
		t.Error("State rolled back to StateAwaitingDecision after the run was superseded mid-Save -- the rollback's aliveLocked guard did not cover it")
	}
}
