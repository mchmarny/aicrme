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
	// Components is the latest known state of each component the bundle
	// installs.
	Components []ComponentState `json:"components,omitempty"`
	// Workload names the reference workload an ActiveStep left running, so
	// the console can label it after a restart. It is a hint, not the
	// source of truth -- see Workload's doc comment.
	//
	// omitzero, not omitempty: omitempty is a no-op on a struct field (a
	// zero-value struct is never "empty" by encoding/json's rules), so a
	// run that never went active would still serialize
	// "workload":{"namespace":"","kind":"","name":""} -- and
	// `if (run.workload)` in the console is truthy for that empty object.
	Workload Workload `json:"workload,omitzero"`
	// StepIndex is the index of the next step to execute. It exists so a
	// failed run can be retried from the step that failed rather than from
	// the top: re-running Discover would redeploy the snapshot agent Job
	// and take minutes, and re-running Recommend would discard the
	// decisions the user already made. It advances only after a step
	// succeeds, so a failure leaves it pointing at the step to retry.
	StepIndex int       `json:"stepIndex"`
	Err       string    `json:"error,omitempty"`
	StartedAt time.Time `json:"startedAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// Truncated names artifacts the store dropped to fit its size limit (see
	// encodeRun). It is read-mostly state about the RECORD, not the run: the
	// engine never sets it, decodeRun populates it on load, and encodeRun
	// carries it forward so a record that has already lost an artifact keeps
	// saying so on every subsequent save.
	//
	// It exists because a truncated record cannot be retried -- Bundle reads
	// snapshot.yaml, which is the first artifact shed -- so a console that
	// only knew the record was recoverable would offer a Retry guaranteed to
	// fail at the step it resumes on. The record was honest about the loss;
	// this is what makes the console honest too.
	Truncated []string `json:"truncated,omitempty"`
}

// ComponentState is the latest known state of one component the bundle
// installs. It is a projection, not a log: exactly one row per component,
// overwritten in place. Persisting this is what lets a recovered run redraw
// the pipeline, without persisting the event stream that produced it.
type ComponentState struct {
	Name   string `json:"name"`
	Index  int    `json:"index"`
	Total  int    `json:"total"`
	Status string `json:"status"`
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
	out.Components = append([]ComponentState(nil), r.Components...)
	out.Truncated = append([]string(nil), r.Truncated...)
	return &out
}
