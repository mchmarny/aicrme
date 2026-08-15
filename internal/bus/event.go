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
