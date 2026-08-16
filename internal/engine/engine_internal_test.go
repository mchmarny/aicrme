package engine

import (
	"context"
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
