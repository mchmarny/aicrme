// Package engine drives the linear run state machine over a slice of Steps.
package engine

import (
	"maps"
	"time"
)

// Phase identifies one stage of the six-phase arc.
type Phase string

const (
	// PhaseDiscover captures the cluster snapshot.
	PhaseDiscover Phase = "discover"
	// PhaseRecommend resolves the AICR recipe.
	PhaseRecommend Phase = "recommend"
	// PhaseBundle generates the deployable bundle.
	PhaseBundle Phase = "bundle"
	// PhaseApply installs the bundle.
	PhaseApply Phase = "apply"
	// PhaseValidate runs the recipe's validation phases.
	PhaseValidate Phase = "validate"
	// PhaseProve runs the reference workload.
	PhaseProve Phase = "prove"
)

// State is the run's lifecycle position.
type State string

const (
	// StateIdle is a created but unstarted run.
	StateIdle State = "idle"
	// StateRunning means a step is executing.
	StateRunning State = "running"
	// StateAwaitingDecision means the next step needs user input.
	StateAwaitingDecision State = "awaiting_decision"
	// StateFailed is terminal with an error.
	StateFailed State = "failed"
	// StateActive is terminal-but-active: every step finished and the Prove
	// workload is deliberately still running. Reset must tear the workload
	// down before uninstalling components beneath it (spec §6).
	StateActive State = "active"
	// StateDone is terminal and quiescent.
	StateDone State = "done"
)

// Run is the full state of one console run.
type Run struct {
	ID        string            `json:"id"`
	State     State             `json:"state"`
	Phase     Phase             `json:"phase"`
	Decisions map[string]string `json:"decisions"`
	Artifacts map[string][]byte `json:"-"`
	Pending   []string          `json:"pending,omitempty"`
	Err       string            `json:"error,omitempty"`
	StartedAt time.Time         `json:"startedAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
}

// Clone returns a deep copy safe to hand to callers outside the engine lock.
func (r *Run) Clone() *Run {
	out := *r
	out.Decisions = maps.Clone(r.Decisions)
	if out.Decisions == nil {
		out.Decisions = map[string]string{}
	}
	out.Artifacts = make(map[string][]byte, len(r.Artifacts))
	for k, v := range r.Artifacts {
		out.Artifacts[k] = append([]byte(nil), v...)
	}
	out.Pending = append([]string(nil), r.Pending...)
	return &out
}
