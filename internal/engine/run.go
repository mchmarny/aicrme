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
	StepIndex int    `json:"stepIndex"`
	Err       string `json:"error,omitempty"`
	// CleanupUnconfirmed is set (from the failing Step's own returned error,
	// via errors.Is against engine.ErrUnconfirmedCleanup) when a StateFailed
	// run's own pre-Active cleanup could not itself be confirmed -- Ruling
	// 12 (spec §8 row 3). Deliberately a structural field, not something
	// re-derived from Err: Err is human text that Retry legitimately
	// overwrites on every attempt, so a guard keyed off it would clear the
	// moment a retry failed for any unrelated, cleanly-cleaned-up reason
	// (fix round 1's C2).
	//
	// Sticky, not recomputed unconditionally (fix round 2's N2): runStep
	// moves this field only on positive evidence -- errors.Is against
	// engine.ErrUnconfirmedCleanup (sets true) or engine.ErrCleanupConfirmed
	// (clears to false) -- and leaves it exactly as it was on every other
	// failure, including one whose cleanup logic was never reached at all.
	// Retry does NOT reset this eagerly: that was fix round 1's shape, and
	// it reintroduced the same defect one call site over (a retry parked or
	// canceled before runStep's failure branch ever ran would otherwise
	// clear a guard nothing had confirmed resolved).
	//
	// Persisted by envelope.go, which is a hand-maintained projection of
	// this type, not a reuse of these json tags -- fix round 2's N1 found
	// this field had gone a full fix round without a producer there, so a
	// restart silently dropped the guard. envelope_test.go's parity test
	// (fix round 3's Ruling 20) now checks every exported Run field is
	// either carried by envelope or named in its exclusion list, so that
	// class of gap fails a test instead of shipping again.
	CleanupUnconfirmed bool      `json:"cleanupUnconfirmed,omitempty"`
	StartedAt          time.Time `json:"startedAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
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
	// Ownership is what the cluster looked like immediately BEFORE this
	// run's Apply, and it is the only thing that separates a release this
	// console created from one it adopted. AICR's generated install.sh runs
	// `helm upgrade --install`, so a release a human already had at the
	// same (name, namespace) is upgraded, prints a deploy header like any
	// other action, and lands in Components indistinguishable from one this
	// run created. Reset uninstalls only what is ABSENT here.
	//
	// Recorded before Apply because that is the only moment the answer
	// exists: --install and --create-namespace both erase the distinction
	// the instant they run.
	//
	// omitzero, not omitempty: see Workload's comment -- omitempty is a
	// no-op on a struct field, so every run that never reached Apply would
	// otherwise serialize an empty ownership object.
	Ownership Ownership `json:"ownership,omitzero"`
}

// ReleaseRef identifies one helm release the way helm itself does: a name is
// only unique within a namespace, so neither half alone identifies anything.
type ReleaseRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// NamespaceRef is one namespace's pre-Apply state.
type NamespaceRef struct {
	Name string `json:"name"`
	// UID is the namespace's object UID at snapshot time, empty when it did
	// not exist. It is what makes "the namespace this run created" a claim
	// about an OBJECT rather than about a name: a namespace deleted and
	// recreated between Apply and Reset wears the same name and belongs to
	// whoever recreated it.
	UID string `json:"uid,omitempty"`
	// Existed records whether the namespace was present pre-Apply. A
	// namespace that existed is never deleted by Reset, whatever is in it.
	Existed bool `json:"existed,omitempty"`
	// SnapshotErr is non-empty when this namespace could not be snapshotted
	// at all. It does not fail Apply (see steps.snapshotOwnership), but it
	// makes every release in the namespace unprovable, so Reset skips them
	// and says why -- an unanswered question is not evidence of ownership.
	SnapshotErr string `json:"snapshotErr,omitempty"`
}

// Ownership is the pre-Apply cluster state Reset reasons from. Its zero
// value is meaningful and safe: no evidence, so nothing is provably this
// run's, so Reset removes nothing. That is also what a record written before
// this field existed decodes to.
type Ownership struct {
	// Releases are the helm releases present BEFORE Apply ran. Anything
	// here was adopted, not created.
	Releases []ReleaseRef `json:"releases,omitempty"`
	// Namespaces is per-namespace state BEFORE Apply ran.
	Namespaces []NamespaceRef `json:"namespaces,omitempty"`
}

// ComponentState is the latest known state of one component the bundle
// installs. It is a projection, not a log: exactly one row per component,
// overwritten in place. Persisting this is what lets a recovered run redraw
// the pipeline, without persisting the event stream that produced it.
type ComponentState struct {
	Name  string `json:"name"`
	Index int    `json:"index"`
	Total int    `json:"total"`
	// Namespace is the helm release's target namespace, carried from
	// deploy.sh's own per-action header ("[1/14] cert-manager  →
	// cert-manager"). Recorded because Reset addresses a release as
	// (name, namespace) and has no other durable source for the second
	// half: the bundle directory that holds it lives in the pod's emptyDir
	// and is gone after any restart.
	Namespace string `json:"namespace,omitempty"`
	Status    string `json:"status"`
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
	// Ownership's two slices hold value types, so copying the slices is a
	// full deep copy. They are copied at all because *r above shares their
	// backing arrays, and Reset's whole safety argument rests on a caller
	// outside the lock never being able to edit this evidence.
	out.Ownership.Releases = append([]ReleaseRef(nil), r.Ownership.Releases...)
	out.Ownership.Namespaces = append([]NamespaceRef(nil), r.Ownership.Namespaces...)
	return &out
}
