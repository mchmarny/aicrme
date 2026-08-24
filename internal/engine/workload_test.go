package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// mustEncodeRun is a test-only wrapper: every test here treats a failed
// encode as a setup failure, not the behavior under test.
func mustEncodeRun(t *testing.T, r Run) []byte {
	t.Helper()
	blob, err := encodeRun(&r, cmPayloadCeiling)
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	return blob
}

// The record carries the workload so the console can name it after a restart
// -- but correctness must not DEPEND on it, because terminal saves are
// best-effort and the store can degrade to memory. Task 8's reconciliation
// covers the case where this is missing; this test only pins the round trip.
func TestWorkloadSurvivesTheRecordRoundTrip(t *testing.T) {
	in := Run{ID: "run-abc", State: StateActive,
		Workload: Workload{Namespace: "aicrme-prove", Kind: "Job", Name: "prove-run-abc"}}
	out, err := decodeRun(mustEncodeRun(t, in), cmPayloadCeiling)
	if err != nil {
		t.Fatalf("decodeRun() error = %v", err)
	}
	if out.Workload != in.Workload {
		t.Errorf("Workload = %+v, want %+v", out.Workload, in.Workload)
	}
}

// A run that never reaches an ActiveStep has nothing to name -- Workload
// must decode as the zero value rather than picking up junk from an unset
// field.
func TestWorkloadEmptyOnARunThatNeverWentActive(t *testing.T) {
	in := Run{ID: "run-idle", State: StateDone}
	out, err := decodeRun(mustEncodeRun(t, in), cmPayloadCeiling)
	if err != nil {
		t.Fatalf("decodeRun() error = %v", err)
	}
	if out.Workload != (Workload{}) {
		t.Errorf("Workload = %+v, want the zero value", out.Workload)
	}
}

// envelopeBeforeWorkload mirrors envelope as it existed before this field was
// added. It exists to prove decodeRun still accepts a record a previous
// image wrote -- this field must be optional on both sides, the same
// contract envelope.Truncated already relies on, so adding it is not a
// reason to bump envelopeVersion.
type envelopeBeforeWorkload struct {
	Version    int               `json:"version"`
	ID         string            `json:"id"`
	State      State             `json:"state"`
	Phase      Phase             `json:"phase"`
	Decisions  map[string]string `json:"decisions,omitempty"`
	Pending    []string          `json:"pending,omitempty"`
	Components []ComponentState  `json:"components,omitempty"`
	StepIndex  int               `json:"stepIndex"`
	Err        string            `json:"error,omitempty"`
	StartedAt  time.Time         `json:"startedAt"`
	UpdatedAt  time.Time         `json:"updatedAt"`
	Artifacts  map[string][]byte `json:"artifacts,omitempty"`
	Truncated  []string          `json:"truncated,omitempty"`
}

// TestDecodeRunAcceptsARecordWithoutWorkload is the backward-compatibility
// check the brief asks for: a ConfigMap written by a build before this field
// existed has no "workload" key at all, and a rollout that adds the field
// must not turn that record unreadable.
func TestDecodeRunAcceptsARecordWithoutWorkload(t *testing.T) {
	old := envelopeBeforeWorkload{
		Version:   envelopeVersion,
		ID:        "run-old",
		State:     StateDone,
		Phase:     PhaseApply,
		StartedAt: time.Now().UTC().Truncate(time.Second),
		UpdatedAt: time.Now().UTC().Truncate(time.Second),
	}
	blob, err := gzipJSON(old)
	if err != nil {
		t.Fatalf("gzipJSON() error = %v", err)
	}
	out, err := decodeRun(blob, cmPayloadCeiling)
	if err != nil {
		t.Fatalf("decodeRun() error = %v, want a pre-Workload record to still decode", err)
	}
	if out.Workload != (Workload{}) {
		t.Errorf("Workload = %+v, want the zero value on a record with no workload field", out.Workload)
	}
}

// Run is marshaled straight to the HTTP API, and web/src/api.ts's Run type
// is documented to mirror it field-for-field. omitempty is a no-op on a
// struct field -- encoding/json only treats false/0/nil/empty-length values
// as empty -- so a zero-value Workload would still serialize as
// "workload":{"namespace":"","kind":"","name":""} on every run that never
// went active. That matters beyond noise: `if (run.workload)` in the
// console is truthy for `{}`, so Task 8's "does this run have a workload"
// check would read true on every run that never had one. The tag must be
// omitzero, which does treat a zero-value struct as empty.
func TestRunWorkloadOmitsZeroValueFromJSON(t *testing.T) {
	blob, err := json.Marshal(Run{ID: "run-zero"})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(blob), `"workload"`) {
		t.Errorf("json = %s, want no \"workload\" key for a zero-value Workload", blob)
	}

	blob, err = json.Marshal(Run{ID: "run-active",
		Workload: Workload{Namespace: "aicrme-prove", Kind: "Job", Name: "prove-run-active"}})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if !strings.Contains(string(blob), `"workload"`) {
		t.Errorf("json = %s, want a \"workload\" key for a populated Workload", blob)
	}
}

// The envelope carries the same fix for the same reason encodeRun's own
// size guard cares about: a zero-value Workload written into every run
// record that never went active is pure waste against maxPayload, and the
// contract is "optional on both sides" either way.
func TestEnvelopeOmitsZeroWorkloadFromJSON(t *testing.T) {
	blob, err := json.Marshal(envelope{Version: envelopeVersion, ID: "run-zero"})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(blob), `"workload"`) {
		t.Errorf("json = %s, want no \"workload\" key for a zero-value Workload", blob)
	}

	blob, err = json.Marshal(envelope{Version: envelopeVersion, ID: "run-active",
		Workload: Workload{Namespace: "aicrme-prove", Kind: "Job", Name: "prove-run-active"}})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if !strings.Contains(string(blob), `"workload"`) {
		t.Errorf("json = %s, want a \"workload\" key for a populated Workload", blob)
	}
}
