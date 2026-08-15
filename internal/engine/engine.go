package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/bus"
)

// Emit publishes a console event. The engine stamps RunID and Phase, so steps
// only supply Kind, Level, Message, Component, and Data.
type Emit func(bus.Event)

// Step is one phase of the run. Requires lists decision keys that must be
// present in Run.Decisions before Run is called; the engine parks in
// StateAwaitingDecision until they are supplied.
type Step interface {
	Phase() Phase
	Requires() []string
	Run(ctx context.Context, r *Run, emit Emit) error
}

// Engine executes steps in order for a single run. One run at a time: this is
// a single-replica demo console, not a scheduler.
type Engine struct {
	bus   *bus.Bus
	store Store
	steps []Step

	mu      sync.Mutex
	current *Run
	resume  chan struct{}
	newID   func() string
}

// New returns an Engine that will execute steps in the order given.
func New(b *bus.Bus, st Store, steps ...Step) *Engine {
	return &Engine{
		bus:   b,
		store: st,
		steps: steps,
		newID: randomID,
	}
}

func randomID() string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

// Start creates a run and executes it in the background. It returns as soon as
// the run is registered; callers observe progress over the bus.
func (e *Engine) Start(ctx context.Context) (*Run, error) {
	e.mu.Lock()
	if e.current != nil && isLive(e.current.State) {
		e.mu.Unlock()
		return nil, aicrerrors.New(aicrerrors.ErrCodeConflict, "a run is already in progress")
	}
	now := time.Now().UTC()
	r := &Run{
		ID:        e.newID(),
		State:     StateRunning,
		Decisions: map[string]string{},
		Artifacts: map[string][]byte{},
		StartedAt: now,
		UpdatedAt: now,
	}
	e.current = r
	e.resume = make(chan struct{}, 1)
	snapshot := r.Clone()
	e.mu.Unlock()

	if err := e.store.Save(ctx, snapshot); err != nil {
		return nil, err
	}
	go e.execute(context.WithoutCancel(ctx))
	return snapshot, nil
}

func isLive(s State) bool {
	return s == StateRunning || s == StateAwaitingDecision
}

// Decide supplies user decisions and unparks a run waiting on them.
func (e *Engine) Decide(runID string, decisions map[string]string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.current == nil || e.current.ID != runID {
		return aicrerrors.New(aicrerrors.ErrCodeNotFound, "run not found: "+runID)
	}
	if e.current.State != StateAwaitingDecision {
		return aicrerrors.New(aicrerrors.ErrCodeConflict, "run is not awaiting a decision")
	}
	for _, key := range e.current.Pending {
		if _, ok := decisions[key]; !ok {
			return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "missing required decision: "+key)
		}
	}
	for k, v := range decisions {
		e.current.Decisions[k] = v
	}
	e.current.Pending = nil
	e.current.State = StateRunning
	e.current.UpdatedAt = time.Now().UTC()

	select {
	case e.resume <- struct{}{}:
	default:
	}
	return nil
}

// Current returns a copy of the most recently started run, or nil if none
// has been started yet. Unlike Get, it needs no run ID -- callers that care
// about "whatever this single-replica console is doing right now" (e.g. the
// options endpoint deriving cluster coordinates from the latest snapshot)
// would otherwise have no way to find it.
func (e *Engine) Current() *Run {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil {
		return nil
	}
	return e.current.Clone()
}

// Get returns a copy of the run's current state.
func (e *Engine) Get(runID string) (*Run, error) {
	e.mu.Lock()
	if e.current != nil && e.current.ID == runID {
		out := e.current.Clone()
		e.mu.Unlock()
		return out, nil
	}
	e.mu.Unlock()
	return e.store.Load(context.Background(), runID)
}

func (e *Engine) execute(ctx context.Context) {
	for _, step := range e.steps {
		if !e.awaitDecisions(ctx, step) {
			return
		}
		if err := e.runStep(ctx, step); err != nil {
			return
		}
	}
	e.finish(ctx, StateDone, "")
}

// awaitDecisions parks the run until every key in step.Requires() is present.
// Returns false if the context ended while parked.
func (e *Engine) awaitDecisions(ctx context.Context, step Step) bool {
	for {
		e.mu.Lock()
		var missing []string
		for _, key := range step.Requires() {
			if _, ok := e.current.Decisions[key]; !ok {
				missing = append(missing, key)
			}
		}
		if len(missing) == 0 {
			e.mu.Unlock()
			return true
		}
		e.current.Pending = missing
		e.current.State = StateAwaitingDecision
		e.current.Phase = step.Phase()
		e.current.UpdatedAt = time.Now().UTC()
		runID := e.current.ID
		resume := e.resume
		snapshot := e.current.Clone()
		e.mu.Unlock()

		_ = e.store.Save(ctx, snapshot)
		e.bus.Publish(bus.Event{
			RunID: runID, Kind: bus.KindDecision, Phase: string(step.Phase()),
			Message: "awaiting decision",
		})

		select {
		case <-resume:
		case <-ctx.Done():
			return false
		}
	}
}

func (e *Engine) runStep(ctx context.Context, step Step) error {
	e.mu.Lock()
	e.current.Phase = step.Phase()
	e.current.State = StateRunning
	e.current.UpdatedAt = time.Now().UTC()
	runID := e.current.ID
	// The step gets a private copy, not e.current. A step writing
	// r.Artifacts while a concurrent Get() clones e.current is a data race
	// that -race reports; the copy's writes are merged back under the lock
	// after Run returns.
	scratch := e.current.Clone()
	snapshot := e.current.Clone()
	e.mu.Unlock()

	_ = e.store.Save(ctx, snapshot)
	e.bus.Publish(bus.Event{
		RunID: runID, Kind: bus.KindPhase, Phase: string(step.Phase()),
		Message: "phase started",
	})

	emit := func(ev bus.Event) {
		ev.RunID = runID
		if ev.Phase == "" {
			ev.Phase = string(step.Phase())
		}
		e.bus.Publish(ev)
	}

	if err := step.Run(ctx, scratch, emit); err != nil {
		e.bus.Publish(bus.Event{
			RunID: runID, Kind: bus.KindError, Phase: string(step.Phase()),
			Level: bus.LevelError, Message: err.Error(),
		})
		e.finish(ctx, StateFailed, err.Error())
		return err
	}

	// Merge the step's writes back under the lock. Artifacts and Decisions are
	// the only fields a step may add to; the engine owns everything else.
	e.mu.Lock()
	for k, v := range scratch.Artifacts {
		e.current.Artifacts[k] = v
	}
	for k, v := range scratch.Decisions {
		e.current.Decisions[k] = v
	}
	e.current.UpdatedAt = time.Now().UTC()
	merged := e.current.Clone()
	e.mu.Unlock()
	_ = e.store.Save(ctx, merged)

	e.bus.Publish(bus.Event{
		RunID: runID, Kind: bus.KindPhase, Phase: string(step.Phase()),
		Message: "phase complete",
	})
	return nil
}

func (e *Engine) finish(ctx context.Context, state State, errMsg string) {
	e.mu.Lock()
	e.current.State = state
	e.current.Err = errMsg
	e.current.UpdatedAt = time.Now().UTC()
	snapshot := e.current.Clone()
	e.mu.Unlock()

	_ = e.store.Save(ctx, snapshot)
	e.bus.Publish(bus.Event{
		RunID: snapshot.ID, Kind: bus.KindPhase,
		Level: levelFor(state), Message: "run " + string(state),
	})
}

func levelFor(s State) bus.Level {
	if s == StateFailed {
		return bus.LevelError
	}
	return bus.LevelInfo
}
