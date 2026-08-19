// Package bus fans typed console events out to SSE subscribers and retains a
// bounded replay buffer so a reconnecting browser sees the whole run.
package bus

import (
	"encoding/json"
	"time"
)

// Kind classifies an event for UI routing.
type Kind string

const (
	// KindPhase marks a run-phase transition.
	KindPhase Kind = "phase"
	// KindLog is free-form narration.
	KindLog Kind = "log"
	// KindComponent is per-component install progress.
	KindComponent Kind = "component"
	// KindCluster is observer-sourced cluster telemetry.
	KindCluster Kind = "cluster"
	// KindDecision signals the run is parked awaiting user input.
	KindDecision Kind = "decision"
	// KindError is a terminal or retryable failure.
	KindError Kind = "error"
	// KindRecovered marks a run that restart recovery installed and that is
	// now blocking new runs until an operator retries or discards it
	// (internal/engine's Recover and its recoveredPending gate).
	//
	// It is deliberately its own Kind rather than a KindPhase with a special
	// message. Nothing else in the stream carries this fact: a recovered
	// StateDone run publishes exactly what a run that just finished normally
	// publishes, and a recovered StateFailed run publishes exactly what an
	// ordinary failure publishes -- so the console cannot infer it. And
	// web/src/components/Wizard.tsx's deriveRunState turns any KindPhase
	// message it does not recognize into state 'running', which would make a
	// marker published through that kind inert only by virtue of an if-branch
	// sitting ahead of a fallthrough.
	KindRecovered Kind = "recovered"
)

// Level is the severity used for UI emphasis.
type Level string

const (
	// LevelInfo is normal progress.
	LevelInfo Level = "info"
	// LevelWarn is surfaced, not buried; may be annotated as benign.
	LevelWarn Level = "warn"
	// LevelError is a failure.
	LevelError Level = "error"
)

// Event is the single wire shape every producer publishes and the SPA consumes.
// ID is assigned by the Bus and is the SSE Last-Event-ID cursor.
type Event struct {
	ID        uint64          `json:"id"`
	RunID     string          `json:"runId,omitempty"`
	At        time.Time       `json:"at"`
	Kind      Kind            `json:"kind"`
	Phase     string          `json:"phase,omitempty"`
	Level     Level           `json:"level"`
	Component string          `json:"component,omitempty"`
	Message   string          `json:"message"`
	Data      json.RawMessage `json:"data,omitempty"`
}
