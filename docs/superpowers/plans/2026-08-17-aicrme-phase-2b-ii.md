# Phase 2b-ii Implementation Plan — restart recovery and the SSE cursor

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A console pod that restarts mid-Apply comes back with the run's state, decisions, artifacts, and per-component progress intact, refuses to silently replace it, and reconnects the browser to a working event stream.

**Architecture:** A ConfigMap-backed `engine.Store` writes a versioned, gzipped whole-run envelope on every state transition. Startup recovers the record before serving, lands non-terminal runs in `StateFailed`, and rewinds `StepIndex` past Bundle because the bundle directory dies with the emptyDir. A `recoveredPending` flag makes `Start` return 409 until the operator retries or discards. The bus gains a per-process epoch announced as an SSE control event.

**Tech Stack:** Go 1.26, `k8s.io/client-go` (fake clientset for tests), React/TypeScript SPA, Helm chart, `golangci-lint`.

**Spec:** `docs/superpowers/specs/2026-08-17-aicrme-phase-2b-ii-design.md` — read it before Task 1. `docs/phase-2-handoff.md` carries the inherited constraints.

## Global Constraints

- **`github.com/NVIDIA/aicr` is pinned at `v0.19.0`.** No version may change; `make check-aicr-pin` enforces it.
- **Coverage floor 80%** aggregate (`.settings.yaml` `quality.coverage_threshold`). All Go tests run under `-race`.
- **`make qualify` must pass before every commit.** It is the full local gate and matches CI exactly.
- **Commits signed (`-S`).** No `Co-Authored-By`, no sign-off (`-s`), no "Generated with" trailer. Branch `phase-2b-ii`, never `main`.
- **Never delete, skip, or weaken an existing test.**
- **Prefer self-documenting code.** Comment *why*, never *what*. `misspell` locale US: "canceled"/"canceling", never "cancelled"/"cancelling". `revive`'s `exported` rule requires doc comments on exported symbols.
- **No store I/O while holding `e.mu`.** The observer's scope accessor calls `Engine.CurrentID` and `Engine.Artifact` on a per-watch-event path and both take that lock; holding it across a ConfigMap round trip stalls every observer publish. Mutate under the lock, snapshot, unlock, do I/O, re-acquire only to roll back.
- **The `cluster-admin` grant in the chart is deliberate and disclosed.** Do not narrow it.
- **`AICRME_SNAPSHOT_REQUESTS` and `AICRME_SNAPSHOT_NODE_SELECTOR` are test-path knobs.** Never add them to `values.yaml`.

---

## File Structure

**Create:**
- `internal/engine/envelope.go` — versioned persistence format, gzip, size guard, bounded decompression, validation.
- `internal/engine/envelope_test.go`
- `internal/engine/cmstore.go` — the ConfigMap-backed `Store`.
- `internal/engine/cmstore_test.go`
- `internal/engine/recover.go` — recovery entry point, `StepIndex` rewind, step-slice validation.
- `internal/engine/recover_test.go`

**Modify:**
- `internal/engine/store.go` — `LoadCurrent`, `Delete`, `memoryStore` implementations.
- `internal/engine/run.go` — `ComponentState`, `Run.Components`, `Clone` coverage.
- `internal/engine/engine.go` — `recoveredPending`, `Discard`, `Decide` restructure, save-failure policy, checkpoint ordering.
- `internal/bus/bus.go` — epoch, `since > nextID` guard.
- `internal/api/events.go` — emit the epoch control event.
- `internal/api/runs.go` — context threading, discard endpoint.
- `internal/api/server.go` — route the discard endpoint.
- `internal/steps/apply.go` — maintain `run.Components`.
- `cmd/aicrme/main.go` — build the store, recover before serving.
- `charts/aicrme/templates/deployment.yaml` — `strategy: Recreate`.
- `test/chart/contract.sh` — assert the strategy.
- `web/src/useEvents.ts` — epoch handling and reconnect.
- `docs/phase-2-handoff.md` — Task 10.

---

## Task 1: The persistence envelope

**Files:**
- Create: `internal/engine/envelope.go`, `internal/engine/envelope_test.go`
- Modify: `internal/engine/run.go`, `internal/engine/store.go`

**Interfaces:**
- Produces: `ComponentState`, `Run.Components`, `encodeRun([]byte, error)`, `decodeRun(*Run, error)`, `ErrTooLarge`, `ErrUnsupportedVersion`, `Store.LoadCurrent`, `Store.Delete`.
- Consumes: nothing.

- [ ] **Step 1: Add `ComponentState` and `Run.Components`**

In `internal/engine/run.go`, after the `Run` struct:

```go
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
```

Add to `Run`, after `Pending`:

```go
	Components []ComponentState `json:"components,omitempty"`
```

Extend `Clone` so the slice is deep-copied — a shared backing array would let a
caller outside the engine lock mutate live state:

```go
	out.Components = append([]ComponentState(nil), r.Components...)
```

- [ ] **Step 2: Write the failing envelope tests**

Create `internal/engine/envelope_test.go` (package `engine`, internal — these test
unexported functions):

```go
package engine

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func testRun() *Run {
	return &Run{
		ID:         "abc123",
		State:      StateRunning,
		Phase:      PhaseApply,
		Decisions:  map[string]string{"intent": "inference"},
		Pending:    []string{"apply"},
		StepIndex:  3,
		StartedAt:  time.Now().UTC().Truncate(time.Second),
		UpdatedAt:  time.Now().UTC().Truncate(time.Second),
		Components: []ComponentState{{Name: "nfd", Index: 2, Total: 14, Status: "installed"}},
		Artifacts:  map[string][]byte{"snapshot.yaml": bytes.Repeat([]byte("a: b\n"), 100)},
	}
}

func TestEncodeDecodeRoundTripsArtifacts(t *testing.T) {
	in := testRun()
	blob, err := encodeRun(in)
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	out, err := decodeRun(blob)
	if err != nil {
		t.Fatalf("decodeRun() error = %v", err)
	}
	// Artifacts are the point: Run.Artifacts is json:"-", so a naive
	// json.Marshal(run) would round-trip everything here EXCEPT the one field
	// recovery cannot work without.
	if !bytes.Equal(out.Artifacts["snapshot.yaml"], in.Artifacts["snapshot.yaml"]) {
		t.Errorf("snapshot.yaml did not survive: got %d bytes, want %d",
			len(out.Artifacts["snapshot.yaml"]), len(in.Artifacts["snapshot.yaml"]))
	}
	if out.ID != in.ID || out.State != in.State || out.Phase != in.Phase || out.StepIndex != in.StepIndex {
		t.Errorf("scalar fields drifted: %+v", out)
	}
	if len(out.Components) != 1 || out.Components[0].Name != "nfd" {
		t.Errorf("Components = %v, want one nfd row", out.Components)
	}
	if out.Decisions["intent"] != "inference" {
		t.Errorf("Decisions = %v", out.Decisions)
	}
}

func TestEncodeDropsBundlePath(t *testing.T) {
	in := testRun()
	in.Artifacts["bundle.path"] = []byte("/var/lib/aicrme/runs/abc123/bundle")
	blob, err := encodeRun(in)
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	out, err := decodeRun(blob)
	if err != nil {
		t.Fatalf("decodeRun() error = %v", err)
	}
	if _, ok := out.Artifacts["bundle.path"]; ok {
		t.Error("bundle.path survived encoding -- it points into an emptyDir that " +
			"does not survive a restart, so restoring it aims Apply at a vanished directory")
	}
	if in.Artifacts["bundle.path"] == nil {
		t.Error("encodeRun mutated the caller's run")
	}
}

func TestEncodeCompresses(t *testing.T) {
	in := testRun()
	// Highly compressible, like a real snapshot.yaml.
	in.Artifacts["snapshot.yaml"] = bytes.Repeat([]byte("nodes:\n  - name: gpu-0\n"), 4000)
	raw := len(in.Artifacts["snapshot.yaml"])
	blob, err := encodeRun(in)
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	if len(blob) >= raw/4 {
		t.Errorf("encoded size %d is not meaningfully smaller than raw %d -- "+
			"compression is the headroom against the 1MiB ConfigMap cap", len(blob), raw)
	}
}

func TestEncodeRejectsOversizedPayload(t *testing.T) {
	in := testRun()
	// Incompressible, so it cannot be squeezed under the cap.
	big := make([]byte, 4<<20)
	for i := range big {
		big[i] = byte(i * 7)
	}
	in.Artifacts["snapshot.yaml"] = big
	if _, err := encodeRun(in); err == nil {
		t.Fatal("encodeRun() error = nil, want ErrTooLarge")
	} else if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error = %v, want a too-large error", err)
	}
}

func TestDecodeRejectsUnknownVersion(t *testing.T) {
	blob, err := encodeRun(testRun())
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	bumped := bumpVersionForTest(t, blob)
	if _, err := decodeRun(bumped); err == nil {
		t.Fatal("decodeRun() error = nil, want an unsupported-version error")
	}
}

func TestDecodeBoundsDecompression(t *testing.T) {
	// A gzip bomb: small stored, enormous expanded. The pod is capped at
	// 512Mi, so an unbounded reader here is an OOM kill rather than an error.
	bomb := gzipBombForTest(t, 64<<20)
	if _, err := decodeRun(bomb); err == nil {
		t.Fatal("decodeRun() error = nil, want a decode error from the size bound")
	}
}
```

Add these two helpers at the bottom of the same file:

```go
// bumpVersionForTest rewrites the envelope's version to one the decoder does
// not know, without hand-authoring a fixture that would drift from the type.
func bumpVersionForTest(t *testing.T, blob []byte) []byte {
	t.Helper()
	var env envelope
	if err := gunzipJSON(blob, &env); err != nil {
		t.Fatalf("gunzipJSON() error = %v", err)
	}
	env.Version = envelopeVersion + 99
	out, err := gzipJSON(env)
	if err != nil {
		t.Fatalf("gzipJSON() error = %v", err)
	}
	return out
}

func gzipBombForTest(t *testing.T, size int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(make([]byte, size)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return buf.Bytes()
}
```

(add `"compress/gzip"` to the test file's imports)

- [ ] **Step 3: Run to verify they fail**

Run: `go test ./internal/engine/ -race -run 'TestEncode|TestDecode' -v`
Expected: FAIL — `undefined: encodeRun`, `undefined: decodeRun`, `undefined: envelope`.

- [ ] **Step 4: Implement the envelope**

Create `internal/engine/envelope.go`:

```go
package engine

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// envelopeVersion is the persisted schema version. It exists so a future
// format change is safe to roll out against a ConfigMap written by a previous
// image: an unrecognized version is refused rather than partially decoded.
const envelopeVersion = 1

// maxPayload bounds the encoded record. Kubernetes caps a ConfigMap at
// roughly 1MiB; failing at 800KiB with a named error beats letting the API
// server reject an oversized object with something opaque, and leaves room
// for the object's own metadata.
const maxPayload = 800 << 10

// maxDecompressed bounds what decodeRun will expand. A small stored payload
// can inflate without limit, and the pod runs under a 512Mi cap, so an
// unbounded reader turns a malformed record into an OOM kill instead of an
// error.
const maxDecompressed = 8 << 20

// ephemeralArtifacts are dropped on encode. bundle.path points into the
// chart's emptyDir, which does not survive a restart -- persisting it would
// hand a recovered Apply a path to a directory that no longer exists, which
// is strictly worse than the key being absent.
var ephemeralArtifacts = map[string]bool{"bundle.path": true}

// envelope is the persisted projection of a Run. It exists rather than
// reusing the API's json tags because Run.Artifacts is json:"-" -- that tag
// is load-bearing (it keeps snapshot.yaml out of HTTP responses) and must
// stay, so the store carries artifacts deliberately instead.
type envelope struct {
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
}

func gzipJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gunzipJSON(blob []byte, v any) error {
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return err
	}
	defer func() { _ = zr.Close() }()
	// LimitReader + 1 so a record exactly at the bound is distinguishable
	// from one that exceeds it.
	raw, err := io.ReadAll(io.LimitReader(zr, maxDecompressed+1))
	if err != nil {
		return err
	}
	if len(raw) > maxDecompressed {
		return fmt.Errorf("decompressed record exceeds %d bytes", maxDecompressed)
	}
	return json.Unmarshal(raw, v)
}

// encodeRun projects a Run into a compressed envelope. It never mutates the
// caller's run.
func encodeRun(r *Run) ([]byte, error) {
	env := envelope{
		Version:    envelopeVersion,
		ID:         r.ID,
		State:      r.State,
		Phase:      r.Phase,
		Decisions:  r.Decisions,
		Pending:    r.Pending,
		Components: r.Components,
		StepIndex:  r.StepIndex,
		Err:        r.Err,
		StartedAt:  r.StartedAt,
		UpdatedAt:  r.UpdatedAt,
		Artifacts:  make(map[string][]byte, len(r.Artifacts)),
	}
	for k, v := range r.Artifacts {
		if ephemeralArtifacts[k] {
			continue
		}
		env.Artifacts[k] = v
	}
	blob, err := gzipJSON(env)
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "encoding run failed", err)
	}
	if len(blob) > maxPayload {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("run state too large to checkpoint: %d bytes compressed, limit %d", len(blob), maxPayload))
	}
	return blob, nil
}

// decodeRun reverses encodeRun. An unrecognized version is refused rather
// than partially decoded: guessing at a format written by a different image
// is how a newer record gets silently downgraded.
func decodeRun(blob []byte) (*Run, error) {
	var env envelope
	if err := gunzipJSON(blob, &env); err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "decoding run failed", err)
	}
	if env.Version != envelopeVersion {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("unsupported run schema version %d (this build writes %d)", env.Version, envelopeVersion))
	}
	r := &Run{
		ID:         env.ID,
		State:      env.State,
		Phase:      env.Phase,
		Decisions:  env.Decisions,
		Pending:    env.Pending,
		Components: env.Components,
		StepIndex:  env.StepIndex,
		Err:        env.Err,
		StartedAt:  env.StartedAt,
		UpdatedAt:  env.UpdatedAt,
		Artifacts:  env.Artifacts,
	}
	if r.Decisions == nil {
		r.Decisions = map[string]string{}
	}
	if r.Artifacts == nil {
		r.Artifacts = map[string][]byte{}
	}
	return r, nil
}
```

- [ ] **Step 5: Run to verify they pass**

Run: `go test ./internal/engine/ -race -run 'TestEncode|TestDecode' -v`
Expected: PASS, all six.

- [ ] **Step 6: Extend the Store interface**

In `internal/engine/store.go`, extend the interface and its doc:

```go
// Store persists run state so a pod restart mid-demo does not wipe the
// timeline. The memory implementation is the development and test default;
// the ConfigMap-backed implementation is what makes restart recovery real.
//
// LoadCurrent exists because startup has no run ID to ask for: recovery's
// whole problem is finding what was in flight, not fetching something known.
type Store interface {
	Save(ctx context.Context, r *Run) error
	Load(ctx context.Context, id string) (*Run, error)
	LoadCurrent(ctx context.Context) (*Run, error)
	Delete(ctx context.Context) error
}
```

Add to `memoryStore` a `current string` field, set in `Save`, and:

```go
func (m *memoryStore) LoadCurrent(_ context.Context) (*Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.current == "" {
		return nil, aicrerrors.New(aicrerrors.ErrCodeNotFound, "no current run")
	}
	r, ok := m.runs[m.current]
	if !ok {
		return nil, aicrerrors.New(aicrerrors.ErrCodeNotFound, "no current run")
	}
	return r.Clone(), nil
}

func (m *memoryStore) Delete(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.runs, m.current)
	m.current = ""
	return nil
}
```

- [ ] **Step 7: Bite-proof the artifact round trip**

Change `encodeRun`'s artifact loop to skip every artifact (`continue` unconditionally).
Run: `go test ./internal/engine/ -race -run 'TestEncode|TestDecode' -v`
Expected: `TestEncodeDecodeRoundTripsArtifacts` FAILS; the other five PASS.
Restore the file exactly and confirm with `git status --short`.

- [ ] **Step 8: Qualify and commit**

```bash
make qualify
git add internal/engine/
git commit -S -m "feat(engine): versioned, compressed run envelope

Run.Artifacts is json:\"-\", so a ConfigMap store that marshals a Run would
persist a record missing the one thing recovery needs. The tag is
load-bearing for the HTTP API and stays; the store gets its own envelope
that carries artifacts deliberately.

bundle.path is dropped on encode -- it points into the chart's emptyDir, so
restoring it aims a recovered Apply at a directory that no longer exists.

Decompression is bounded because the pod is capped at 512Mi: unbounded, a
malformed record is an OOM kill rather than a decode error."
```

---

## Task 2: The ConfigMap store

**Files:**
- Create: `internal/engine/cmstore.go`, `internal/engine/cmstore_test.go`

**Interfaces:**
- Consumes: `encodeRun`, `decodeRun` (Task 1).
- Produces: `NewConfigMapStore(client kubernetes.Interface, namespace, name string, owner metav1.OwnerReference) Store`.

- [ ] **Step 1: Write the failing tests**

Create `internal/engine/cmstore_test.go` (package `engine`). Use
`k8s.io/client-go/kubernetes/fake`, matching `internal/observer`'s pattern.

Required cases, each its own test function:

1. `TestConfigMapStoreSaveThenLoadCurrent` — save a run, `LoadCurrent` returns it with artifacts intact.
2. `TestConfigMapStoreCreatesWithOwnerReference` — after `Save`, the created ConfigMap carries exactly the owner passed to the constructor, and its `Kind` is `Deployment`. Assert on `Kind`, not just presence: a ReplicaSet owner would be garbage-collected on the next rollout, deleting run state.
3. `TestConfigMapStoreUpdatesExisting` — two saves produce one ConfigMap, not two, and the second's content wins.
4. `TestConfigMapStoreLoadCurrentNotFound` — no ConfigMap yields an error carrying `aicrerrors.ErrCodeNotFound`. Assert the *code*, not the message, because recovery keys on exactly this distinction. The pinned module exposes no `Code(err)` helper, so use the repo's established pattern: `var se *aicrerrors.StructuredError; errors.As(err, &se) && se.Code == aicrerrors.ErrCodeNotFound`.
5. `TestConfigMapStoreCorruptRecordIsNotNotFound` — a ConfigMap whose payload is garbage yields an error whose code is **not** `ErrCodeNotFound`.
6. `TestConfigMapStoreRetriesOnConflict` — prepend a reactor returning `apierrors.NewConflict` for the first `update`, then succeeding; assert `Save` succeeds and that exactly two update attempts were made.
7. `TestConfigMapStoreGivesUpAfterBoundedConflicts` — a reactor that always conflicts makes `Save` return an error rather than looping forever.
8. `TestConfigMapStoreRejectsForeignOwner` — an existing ConfigMap whose owner UID differs from the configured owner makes `Save` fail rather than overwrite. This is what stops the console clobbering a record from a different install that reused the name.
9. `TestConfigMapStoreDelete` — after `Delete`, `LoadCurrent` reports `ErrCodeNotFound`.
10. `TestConfigMapStoreSerializesWrites` — 20 concurrent `Save` calls under `-race` all return without error and leave one ConfigMap.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/engine/ -race -run TestConfigMapStore -v`
Expected: FAIL — `undefined: NewConfigMapStore`.

- [ ] **Step 3: Implement the store**

Create `internal/engine/cmstore.go`. Shape:

```go
package engine

import (
	"context"
	"sync"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// payloadKey is the single binaryData key holding the gzipped envelope.
// binaryData, not data: the payload is compressed bytes, and client-go
// handles the base64 transport encoding itself.
const payloadKey = "run"

// maxConflictRetries bounds optimistic-concurrency retries. The chart sets
// strategy: Recreate so overlapping writers should not exist; this is the
// belt to that braces, and it is bounded because an unbounded retry against a
// genuinely contended object is an infinite loop, not resilience.
const maxConflictRetries = 5

type configMapStore struct {
	client    kubernetes.Interface
	namespace string
	name      string
	owner     metav1.OwnerReference

	// mu serializes writes. Two concurrent Saves would each read-modify-write
	// the same object and one would silently lose; conflict retries recover
	// from *external* races, not from this process racing itself.
	mu sync.Mutex
}

// NewConfigMapStore returns a Store backed by a single ConfigMap.
//
// owner must reference the Deployment, never its ReplicaSet: a ReplicaSet is
// replaced on every rollout and ownerReference garbage collection would then
// delete the run state as a side effect of upgrading the console.
func NewConfigMapStore(client kubernetes.Interface, namespace, name string, owner metav1.OwnerReference) Store {
	return &configMapStore{client: client, namespace: namespace, name: name, owner: owner}
}
```

`Save` acquires `mu`, encodes, then loops up to `maxConflictRetries`:

- `Get` the ConfigMap.
- If `apierrors.IsNotFound`, `Create` one carrying `payloadKey` in `BinaryData` and
  `OwnerReferences: []metav1.OwnerReference{s.owner}`. A `Create` that races another
  creator returns `IsAlreadyExists` — treat that as a conflict and retry.
- Otherwise verify the existing object's owner: if it has an owner UID and it differs
  from `s.owner.UID`, return an error rather than overwrite.
- Set `BinaryData[payloadKey]` and `Update`. On `apierrors.IsConflict`, continue the loop.

`LoadCurrent` gets the ConfigMap; `IsNotFound` maps to `ErrCodeNotFound`; a missing
`payloadKey` or a `decodeRun` failure maps to `ErrCodeInvalidRequest` — deliberately
**not** `NotFound`, because Task 3 treats those two cases very differently.

`Load(ctx, id)` calls `LoadCurrent` and returns `ErrCodeNotFound` if the ID does not
match — this store holds one run, so any other ID genuinely is not found.

`Delete` deletes the ConfigMap, treating `IsNotFound` as success.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/engine/ -race -run TestConfigMapStore -v`
Expected: PASS, all ten.

- [ ] **Step 5: Bite-proof the owner check and the conflict bound**

Two mutations, each run separately with `-v`, each restored exactly afterwards:

**a.** Delete the foreign-owner check. Expected: `TestConfigMapStoreRejectsForeignOwner`
fails, the other nine pass.

**b.** Change the retry loop bound to loop forever on conflict. Expected:
`TestConfigMapStoreGivesUpAfterBoundedConflicts` fails (it will hang — give it
`-timeout 30s` so the failure is a timeout rather than a wedged run).

Confirm `git status --short` is empty after both.

- [ ] **Step 6: Qualify and commit**

```bash
make qualify
git add internal/engine/
git commit -S -m "feat(engine): ConfigMap-backed run store

One ConfigMap, whole-run writes, bounded conflict retries, and an owner-UID
check so a record left by a different install is refused rather than
overwritten. Writes are serialized in-process: conflict retries recover from
external races, not from this process racing itself.

LoadCurrent distinguishes NotFound from unreadable. Recovery treats them
very differently -- a transient API error must not look like a cold start,
because that is what lets a new run overwrite a record that was merely
unreadable at that moment."
```

---

## Task 3: Recovery

**Files:**
- Create: `internal/engine/recover.go`, `internal/engine/recover_test.go`
- Modify: `internal/engine/engine.go`

**Interfaces:**
- Consumes: `Store.LoadCurrent` (Task 2).
- Produces: `Engine.Recover(ctx) error`, `ErrStepConfig`.

- [ ] **Step 1: Write the failing tests**

Create `internal/engine/recover_test.go` (package `engine_test` where possible; use
`package engine` if it needs unexported state). Required cases:

1. `TestRecoverLandsNonTerminalRunsFailed` — table over `StateIdle`, `StateRunning`,
   `StateAwaitingDecision`: each recovers as `StateFailed` with an `Err` containing
   "interrupted".
2. `TestRecoverLeavesTerminalRunsAlone` — table over `StateDone`, `StateActive`: state
   and `StepIndex` unchanged.
3. `TestRecoverRewindsAlreadyFailedRunAtApply` — a run persisted **`StateFailed`** with
   `StepIndex` at Apply recovers with `StepIndex` at the Bundle step. This is the
   blocker case: the run was already failed before the crash, so a rewind keyed on
   "was it non-terminal" would miss it and leave Retry dead.
4. `TestRecoverRewindsInterruptedRunAtApply` — same, but persisted `StateRunning`.
5. `TestRecoverDoesNotRewindBeforeBundle` — `StepIndex` at Discover stays put.
6. `TestRecoverDoesNotRewindTerminalRuns` — a `StateDone` run past Bundle keeps its index.
7. `TestRecoverNotFoundIsACleanStart` — a store reporting `ErrCodeNotFound` leaves
   `Current()` nil and returns no error.
8. `TestRecoverUnreadableRecordDoesNotInstallOrOverwrite` — a store whose `LoadCurrent`
   returns a non-NotFound error leaves `Current()` nil, returns no error, **and** a
   subsequent `Start` performs no `Save` against that store. Assert the last part by
   counting `Save` calls on the fake.
9. `TestRecoverRejectsInvalidRecord` — table over an empty ID, an unknown `State`, and a
   `StepIndex` beyond the step slice; each takes the unreadable path.
10. `TestRecoverRequiresExactlyOneBundleStep` — engines built with zero and with two
    `PhaseBundle` steps return an error matching `ErrStepConfig`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/engine/ -race -run TestRecover -v`
Expected: FAIL — `undefined: Recover`.

- [ ] **Step 3: Implement recovery**

Create `internal/engine/recover.go`:

```go
package engine

import (
	"context"
	"errors"
	"log/slog"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// ErrStepConfig reports a step slice this engine cannot recover against. It
// is a programming error, not a runtime condition: main treats it as fatal
// rather than degrading, because discovering an ambiguous rewind target
// during a real recovery is the worst possible time to find out.
var ErrStepConfig = errors.New("engine step configuration invalid")

// recoveredErr is what a recovered run carries. The wording matters: the
// cockpit shows it, and an operator needs to tell a restart apart from a
// failed helm install before deciding whether Retry is safe.
const recoveredErr = "interrupted by a console restart"

// bundleStepIndex returns the index of the single PhaseBundle step.
func (e *Engine) bundleStepIndex() (int, error) {
	found := -1
	for i, s := range e.steps {
		if s.Phase() != PhaseBundle {
			continue
		}
		if found >= 0 {
			return 0, fmt.Errorf("%w: %d steps report PhaseBundle, want exactly 1", ErrStepConfig, 2)
		}
		found = i
	}
	if found < 0 {
		return 0, fmt.Errorf("%w: no step reports PhaseBundle", ErrStepConfig)
	}
	return found, nil
}

// Recover loads any persisted run and installs it as the current run.
//
// It returns an error only for a configuration fault the process cannot run
// with. Store failures are handled here and reported as a degraded start:
// recovery is a convenience, and the console starting is not.
func (e *Engine) Recover(ctx context.Context) error {
	bundleIdx, err := e.bundleStepIndex()
	if err != nil {
		return err
	}

	r, err := e.store.LoadCurrent(ctx)
	if err != nil {
		// aicr@v0.19.0's errors package exposes no Code(err) helper -- New,
		// Wrap, IsTransient and friends only -- so the code is reached through
		// errors.As, matching how the rest of this repo inspects it.
		var se *aicrerrors.StructuredError
		if errors.As(err, &se) && se.Code == aicrerrors.ErrCodeNotFound {
			return nil // cold start, the common case
		}
		// Unreadable is NOT absent. Refusing to install it is half the
		// answer; the other half is in main, which swaps to a memory store so
		// nothing this process does can overwrite a record it could not read.
		slog.Error("persisted run unreadable; starting without it and leaving it untouched", "error", err)
		e.storeUnreadable = true
		return nil
	}

	if err := e.validateLoaded(r); err != nil {
		slog.Error("persisted run failed validation; starting without it and leaving it untouched", "error", err)
		e.storeUnreadable = true
		return nil
	}

	if isLive(r.State) || r.State == StateIdle {
		r.State = StateFailed
		r.Err = recoveredErr
	}

	// Rewind on retryability, not on how the run reached its state. The
	// bundle directory died with the emptyDir regardless of whether the run
	// was interrupted or had already failed, so a run that failed during
	// Apply before the crash needs the same rewind as one cut off mid-step.
	if r.State == StateFailed && r.StepIndex > bundleIdx {
		r.StepIndex = bundleIdx
	}

	e.mu.Lock()
	e.current = r
	e.recoveredPending = true
	e.mu.Unlock()
	return nil
}
```

`validateLoaded` checks: non-empty `ID`; `State` is one of the declared constants;
`Phase` likewise; `StepIndex` in `[0, len(e.steps)]`; `StartedAt` non-zero. Each
failure returns a descriptive error.

Add `storeUnreadable bool` and `recoveredPending bool` fields to `Engine`, plus an
exported `StoreUnreadable() bool` accessor for main.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/engine/ -race -run TestRecover -v`
Expected: PASS, all ten.

- [ ] **Step 5: Bite-proof the rewind's trigger condition**

Change the rewind guard from `r.State == StateFailed` to only rewinding runs the
recovery itself just failed (track a local `wasInterrupted` bool and gate on it) —
i.e. reintroduce exactly the blocker this task fixes.

Run: `go test ./internal/engine/ -race -run TestRecover -v`
Expected: `TestRecoverRewindsAlreadyFailedRunAtApply` FAILS while
`TestRecoverRewindsInterruptedRunAtApply` still PASSES. That asymmetry is the whole
point — a single rewind test would not have caught this.

Restore exactly; confirm `git status --short` is empty.

- [ ] **Step 6: Qualify and commit**

```bash
make qualify
git add internal/engine/
git commit -S -m "feat(engine): recover a persisted run at startup

Non-terminal runs land StateFailed so the existing Retry -- already gated on
that state and already resuming from StepIndex -- is the only resume path.
A second path would duplicate machinery that is tested and reviewed.

StepIndex rewinds to Bundle for any retryable run at or beyond it, keyed on
retryability rather than on how the run reached its state: the bundle
directory dies with the emptyDir either way, so a run that had already
failed during Apply needs the same rewind as one interrupted mid-step.

Unreadable is not absent. A non-NotFound load failure starts the console
without the record and marks the store unreadable, so nothing this process
does can overwrite state it could not parse."
```

---

## Task 4: The recovery bootstrap contract

**Files:**
- Modify: `internal/engine/engine.go`

**Interfaces:**
- Consumes: `recoveredPending` (Task 3).
- Produces: `Engine.Discard(ctx, runID) error`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/engine/engine_test.go`:

1. `TestStartIsRefusedWhileRecoveryIsPending` — after `Recover` installs a run,
   `Start` returns an error carrying `aicrerrors.ErrCodeConflict` (reached via `errors.As`, as above).
   This is the blocker: the SPA posts `/api/runs` automatically on load and `Start`
   rejects only `isLive` states, so a recovered `StateFailed` run was replaced on the
   normal path before the operator saw it.
2. `TestRetryClearsRecoveryPending` — after `Retry`, a later `Start` (once the run is
   terminal again) behaves normally.
3. `TestDiscardClearsRecoveryPendingAndDeletes` — `Discard` clears the flag, calls
   `Store.Delete`, leaves `Current()` nil, and a subsequent `Start` succeeds.
4. `TestDiscardRejectsUnknownRunID` — guards against a stale browser tab discarding a
   run the operator has since replaced.
5. `TestStartIsNormalWithoutRecovery` — the flag is false on a cold start.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/engine/ -race -run 'TestStartIsRefused|TestRetryClears|TestDiscard|TestStartIsNormal' -v`
Expected: FAIL — `undefined: Discard`, and the Start test fails because nothing refuses.

- [ ] **Step 3: Implement**

In `Start`, immediately after the existing `draining` check and before the `isLive`
check:

```go
	if e.recoveredPending {
		e.mu.Unlock()
		return nil, aicrerrors.New(aicrerrors.ErrCodeConflict,
			"a recovered run is waiting for retry or discard")
	}
```

In `Retry`, clear `e.recoveredPending = false` under the lock at the point it accepts
the run.

Add:

```go
// Discard drops a recovered run and its persisted record, freeing the console
// to start fresh. Without it, a recovered run would block Start forever --
// a worse wedge than the one the block exists to prevent.
func (e *Engine) Discard(ctx context.Context, runID string) error {
	e.mu.Lock()
	if e.current == nil || e.current.ID != runID {
		e.mu.Unlock()
		return aicrerrors.New(aicrerrors.ErrCodeNotFound, "run not found: "+runID)
	}
	e.current = nil
	e.recoveredPending = false
	e.mu.Unlock()

	// Store I/O deliberately outside the lock: the observer's scope accessor
	// takes e.mu on a per-watch-event path.
	if err := e.store.Delete(ctx); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeUnavailable, "deleting the persisted run failed", err)
	}
	return nil
}
```

- [ ] **Step 4: Run to verify they pass**

Run: same as Step 2, `-v`. Expected: PASS.

- [ ] **Step 5: Bite-proof the Start refusal**

Delete the `recoveredPending` check from `Start`.
Expected: `TestStartIsRefusedWhileRecoveryIsPending` FAILS; every other engine test
passes. Restore exactly; confirm `git status --short` is empty.

- [ ] **Step 6: Qualify and commit**

```bash
make qualify
git add internal/engine/
git commit -S -m "feat(engine): refuse Start while a recovered run is pending

web/src/App.tsx posts /api/runs automatically on load -- by design, since
Discover needs no decisions -- and treats 409 as expected and silent. Start
rejected only isLive states, and recovery produces StateFailed, so the
recovered run was destroyed on the normal path before the operator could
see it.

Start now returns 409 until Retry or Discard, which the SPA's existing 409
handling already does the right thing with. Discard exists because a block
with no release is a worse wedge than the bug."
```

---

## Task 5: Save-failure policy, `Decide`, and checkpoint ordering

**Files:**
- Modify: `internal/engine/engine.go`

- [ ] **Step 1: Write the failing tests**

1. `TestDecidePersistsBeforeAcknowledging` — a store whose `Save` fails makes `Decide`
   return an error, leaves `Decisions` unchanged, leaves `State` at
   `StateAwaitingDecision`, and does **not** signal `resume`. Assert the last by
   confirming the run does not advance within a short window.
2. `TestDecideSucceedsAndPersists` — the happy path saves exactly once with the new
   decisions present in the saved snapshot.
3. `TestDecideDoesNotHoldTheLockDuringIO` — a store whose `Save` blocks on a channel
   must not block a concurrent `CurrentID()`. Fail the test if `CurrentID` does not
   return within a generous timeout. This is the constraint the observer's per-event
   path depends on.
4. `TestStepSuccessCheckpointsCursorBeforeNextStep` — with a two-step engine, the save
   that follows step 1 carries the advanced `StepIndex`.
5. `TestBestEffortCheckpointFailureIsLogged` — a failing mid-step save does not fail
   the run, and emits a warning. Capture with an `slog` handler installed for the test.
6. `TestTerminalSaveFailureIsVisible` — a store failing only the terminal save produces
   an error-level log **and** a bus event.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/engine/ -race -run 'TestDecide|TestStepSuccess|TestBestEffort|TestTerminalSave' -v`
Expected: FAIL.

- [ ] **Step 3: Restructure `Decide`**

`Decide` currently holds `e.mu` for its whole body under a `defer` and never saves.
Rewrite it to: validate and mutate under the lock; take a snapshot; unlock; `Save`; on
failure re-acquire, roll back the mutation (guarded by an identity check that
`e.current.ID` is still the same run), and return the error; on success re-acquire only
to send `resume`.

The `resume` signal must not be sent before the save succeeds, or the step proceeds on
a decision that was never recorded.

- [ ] **Step 4: Apply the save-failure policy at every call site**

- Mid-step checkpoints: keep the `_ =` control flow, add
  `slog.Warn("run checkpoint failed", "run", id, "error", err)`. Six to thirty warnings
  across a run is the only signal that recovery has quietly stopped working.
- The step-success checkpoint: ensure it carries the advanced `StepIndex` and completes
  before the next step begins.
- `finish`: keep it unrecoverable, but log at error level and publish a bus event.
  Comment the real consequence — the persisted record is not absent, it is a **stale
  earlier checkpoint** that the next startup will recover and mark failed.

- [ ] **Step 5: Run to verify they pass**

Run: same as Step 2, `-v`. Expected: PASS, and the full engine suite stays green.

- [ ] **Step 6: Bite-proof the `Decide` ordering**

Move the `resume` send to before the `Save` call.
Expected: `TestDecidePersistsBeforeAcknowledging` FAILS; the other five pass.
Restore exactly; confirm `git status --short` is empty.

- [ ] **Step 7: Qualify and commit**

```bash
make qualify
git add internal/engine/
git commit -S -m "fix(engine): persist decisions before acknowledging them

Decide mutated Decisions, cleared Pending, set StateRunning, signaled
resume, and returned -- with no Save anywhere. A pod dying just after a 200
lost the operator's choice, and recovery re-parked for a decision they had
already made and been told was accepted.

The store call happens outside e.mu: the observer's scope accessor calls
CurrentID and Artifact on a per-watch-event path, so holding the lock across
a ConfigMap round trip would stall every observer publish for the length of
an API call.

Best-effort checkpoints now log. The terminal save's failure is visible and
its comment states the real consequence -- a stale earlier checkpoint that
the next startup recovers, not an absent record."
```

---

## Task 6: The component-state projection

**Files:**
- Modify: `internal/steps/apply.go`, `internal/engine/recover.go`

- [ ] **Step 1: Write the failing tests**

In `internal/steps/apply_test.go`:

1. `TestApplyMaintainsComponentState` — driving the applier's parsed events updates
   `run.Components`: one row per component, `Status` moving `installing` →
   `installed`, and a failure recording `failed`.
2. `TestApplyComponentStateIsAProjectionNotALog` — the same component reported twice
   updates its row in place rather than appending. This is what keeps the projection
   bounded against the ConfigMap cap.

In `internal/engine/recover_test.go`:

3. `TestRecoverRestoresComponentState` — a persisted run's `Components` survive and are
   present on the installed run.
4. `TestRecoverPublishesBootstrapEvents` — recovery publishes events carrying the run
   identity, its phase, and one event per component row, so the SPA's normal replay
   path renders a recovered run with no new fetch path.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/steps/ ./internal/engine/ -race -run 'TestApplyMaintains|TestApplyComponent|TestRecoverRestores|TestRecoverPublishes' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

In `internal/steps/apply.go`, as each parsed marker event is handled, upsert into
`run.Components` keyed by name. The engine's existing merge-back is what persists it.

In `Recover`, after installing the run, publish the bootstrap events.

- [ ] **Step 4: Run to verify they pass**

Expected: PASS.

- [ ] **Step 5: Qualify and commit**

```bash
make qualify
git add internal/steps/ internal/engine/
git commit -S -m "feat: persist a bounded per-component state projection

Finding 1's contract list requires enough persisted state to redraw the
pipeline after a restart. A projection satisfies it -- one row per
component, overwritten in place -- without persisting the event stream,
which is what would actually strain the 1MiB cap.

Recovery publishes bootstrap events rather than adding a current-run
endpoint: the stream is already the SPA's source of truth, and a second
source would need reconciling against it."
```

---

## Task 7: API contexts and the discard endpoint

**Files:**
- Modify: `internal/api/runs.go`, `internal/api/server.go`, `internal/engine/engine.go`

- [ ] **Step 1: Write the failing tests**

In `internal/api/runs_test.go`:

1. `TestCreateRunReturns409WhenRecoveryPending` — the engine's conflict maps to 503/409
   per `writeErr`'s existing mapping; assert the status the SPA's silent-409 path
   expects.
2. `TestDiscardRunDeletesAndAllowsRestart` — `DELETE /api/runs/{id}` returns 204 and a
   subsequent create succeeds.
3. `TestDiscardRunRequiresCSRFAndAuth` — it is a mutating endpoint, so it must sit
   behind the same middleware as the others, and behind `Drain`.
4. `TestRequestCancellationReachesTheStore` — a canceled request context causes the
   store call to observe cancellation, proving the context is threaded rather than
   replaced by `context.Background()`.

- [ ] **Step 2: Run to verify they fail**

Expected: FAIL — no route, and `Get`/`Retry` take no context.

- [ ] **Step 3: Implement**

Thread request contexts through `Engine.Start`, `Retry`, `Get`, and `Decide`. Keep the
one deliberate detachment: the **run's execution context** still outlives the request
via `context.WithoutCancel`, because Apply takes 10–20 minutes and a closed tab must
not cancel an install. Update the comments in `runs.go` — including the one at line 43
that predicted this change — to describe the split that now exists.

Register `DELETE /api/runs/{id}` behind the same auth, CSRF, and drain middleware as
the other mutating routes.

- [ ] **Step 4: Run to verify they pass**

Expected: PASS, and `internal/api`'s full suite stays green.

- [ ] **Step 5: Qualify and commit**

```bash
make qualify
git add internal/api/ internal/engine/
git commit -S -m "feat(api): thread request contexts and add run discard

internal/api/runs.go already carried a comment naming this phase as the
point background contexts stop being safe: with a real store, Load and Save
are ConfigMap API calls, so context.Background() there ignores genuine
caller cancellation instead of hitting an in-memory map.

The execution context stays detached -- Apply takes 10-20 minutes and a
closed tab must not cancel an install. The split is between that and the
store I/O for one API call, which is the caller's and bounded."
```

---

## Task 8: Wiring, the chart, and the single-writer guarantee

**Files:**
- Modify: `cmd/aicrme/main.go`, `cmd/aicrme/main_test.go`, `charts/aicrme/templates/deployment.yaml`, `test/chart/contract.sh`

- [ ] **Step 1: Write the failing tests**

1. `TestOwnerReferenceResolvesToTheDeployment` — the owner resolution helper returns a
   reference whose `Kind` is `Deployment`. A ReplicaSet owner would be garbage-collected
   on the next rollout, deleting run state.
2. `TestNoClientKeepsTheMemoryStore` — a nil `kubernetes.Interface` leaves the memory
   store in place with a warning, preserving `make build && ./bin/aicrme` outside a
   cluster.
3. In `test/chart/contract.sh`, assert `strategy.type == Recreate` in the rendered
   Deployment.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./cmd/aicrme/ -race -run 'TestOwnerReference|TestNoClient' -v` and
`./test/chart/contract.sh`.
Expected: FAIL — the helper does not exist, and the chart renders no strategy.

- [ ] **Step 3: Set the Deployment strategy**

In `charts/aicrme/templates/deployment.yaml`, under `spec:`:

```yaml
  # Recreate, not the default RollingUpdate: replicas is 1, but RollingUpdate
  # with the default maxSurge rounds up to 1, so old and new pods overlap
  # during every upgrade. Both would recover from and write to the same run
  # ConfigMap, against a design that assumes a single writer. A few seconds of
  # downtime on upgrade costs a single-operator demo console nothing; leader
  # election would be machinery for a problem this product does not have.
  strategy:
    type: Recreate
```

- [ ] **Step 4: Wire the store in `main`**

Resolve the owner reference to the **Deployment**, build the ConfigMap store when the
client is non-nil, and call `eng.Recover(ctx)` **before** `httpSrv.ListenAndServe`.

Treat `ErrStepConfig` as fatal (`slog.Error` + `os.Exit(1)`); anything else is already
handled inside `Recover`. If `eng.StoreUnreadable()` reports true, **log it — that is all
main does.** Per Ruling 4, `Recover` performs the store swap itself, because `Engine.store`
is private and main cannot know the record is unreadable until after `New` has already
taken the ConfigMap store. Do not go looking for a setter: there isn't one, and adding one
would reintroduce the chicken-and-egg that ruling resolved.

Recovery runs before serving so the SPA's automatic `POST /api/runs` cannot win the
race. This does not reintroduce 2b-i's startup-hang class: every ConfigMap call is
bounded by an explicit timeout and every failure falls through to the degraded path.

- [ ] **Step 5: Run to verify they pass**

Expected: PASS, and `./test/chart/contract.sh` reports PASS.

- [ ] **Step 6: Bite-proof the strategy assertion**

Remove the `strategy` block from the template; confirm `contract.sh` exits non-zero.
Restore exactly; confirm `git status --short` is empty.

- [ ] **Step 7: Qualify and commit**

```bash
make qualify
git add cmd/aicrme/ charts/ test/chart/
git commit -S -m "feat: wire the ConfigMap store and guarantee a single writer

The chart specified no update strategy, so replicas: 1 still overlapped two
pods on every upgrade under the default RollingUpdate -- two writers against
a design that assumes one. strategy: Recreate is the proportionate fix.

The store's ownerReference targets the Deployment, never its ReplicaSet: a
ReplicaSet is replaced on every rollout, and ownerReference garbage
collection would then delete run state as a side effect of an upgrade.

Recovery completes before the server serves, so the SPA's automatic
POST /api/runs cannot race it."
```

---

## Task 9: The bus epoch

**Files:**
- Modify: `internal/bus/bus.go`, `internal/bus/bus_test.go`, `internal/api/events.go`, `internal/api/events_test.go`, `web/src/useEvents.ts`, `web/src/useEvents.test.ts`

- [ ] **Step 1: Write the failing tests**

Go side:

1. `TestEpochDiffersAcrossBuses` — two `bus.New` calls yield different epochs.
2. `TestSinceBelowNextIDReplaysTheRemainder` — the ordinary case still works.
3. `TestSinceEqualToNextIDReplaysNothing` — the boundary.
4. `TestSinceAboveNextIDReplaysEverything` — a cursor from a previous process is
   impossible, so it replays from 0 rather than filtering everything out.
5. `TestEventsHandlerEmitsEpochControlEventFirst` — the control event is **named** and
   carries **no id**, so it cannot advance the client's cursor.

TypeScript side (`web/src/useEvents.test.ts`):

6. An epoch change clears state **and reconnects from zero**. Assert a new
   `EventSource` is constructed with `since=0`; resetting `lastId` in place is
   insufficient, because the server already chose its backlog from the original cursor
   when the connection opened.
7. Frames still queued from the stale `EventSource` after an epoch change are ignored.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/bus/ ./internal/api/ -race -run 'TestEpoch|TestSince|TestEventsHandlerEmits' -v` and `cd web && npm test`.
Expected: FAIL.

- [ ] **Step 3: Implement**

`bus.New` generates an epoch (an opaque string; use the existing random-ID helper's
approach rather than a timestamp, so two processes started in the same second differ).
Expose `Bus.Epoch() string`. In `since()`, treat `since > b.nextID` as 0.

`internal/api/events.go` writes the control event before any replay.

`web/src/useEvents.ts` stores the epoch, and on a change tears down the current
`EventSource`, clears accumulated state, and opens a new one at `since=0`, ignoring any
frames from the old source.

- [ ] **Step 4: Run to verify they pass**

Expected: PASS on both sides.

- [ ] **Step 5: Bite-proof the reconnect**

Change the SPA to reset `lastId` in place without reconnecting.
Expected: the reconnect test FAILS; the ignore-stale-frames test may also fail, which is
fine — but the ordinary-streaming tests must still PASS.
Restore exactly; confirm `git status --short` is empty.

- [ ] **Step 6: Qualify and commit**

```bash
make qualify
git add internal/bus/ internal/api/ web/src/
git commit -S -m "fix: reconnect the event stream across a process restart

nextID resets to 1 on restart while the browser holds a high lastId, so the
server filtered everything at or below a cursor it never issued: a live,
healthy-looking connection receiving nothing. detectGap could not fire,
because detecting a gap requires events to arrive.

The epoch is a named, ID-less control event -- giving it an id would advance
the very cursor it exists to correct. On a change the client reconnects from
zero rather than resetting lastId in place, because the server already chose
its backlog from the original cursor when the connection opened."
```

---

## Task 10: Update the handoff

**Files:**
- Modify: `docs/phase-2-handoff.md`

- [ ] **Step 1: Move what 2b-ii closed**

From "Constraints 2b-ii inherits" into a new "Resolved in 2b-ii" section: the ConfigMap
store, the bus `nextID` epoch, and Finding 1's restart-recovery contract items —
enumerating which of that list are now closed and which are not.

- [ ] **Step 2: Record what 2b-ii deliberately did not do**

Into "Constraints 2b-iii inherits": per-component live sub-status wired into the
cockpit rows, the Pod and Event informers, and the raw event stream's absence after a
restart. Say why each was deferred.

- [ ] **Step 3: Record the new constraints this phase creates**

- `strategy: Recreate` means an upgrade drops the console briefly; anything that later
  wants zero-downtime upgrades needs leader election first, not just more replicas.
- The envelope is versioned; a format change must bump it and decide what a
  previous-image record does.
- The 800 KiB guard is untested against a real cluster's snapshot — the Phase 4
  measurement.
- Phase 3's Reset must still route through `finish` before bumping `epoch`, and with a
  real store the consequence is now durable rather than in-memory.

- [ ] **Step 4: Sweep for stale references**

Run `grep -rn "2b-ii" --include=*.go --include=*.md . | grep -v "^./.superpowers\|node_modules\|docs/superpowers/"` and confirm every remaining hit is accurate now that the phase has shipped. The Phase 2a lesson applies: a scope limited to `docs/` orphans code comments that point at the phase.

- [ ] **Step 5: Commit**

```bash
make qualify
git add docs/phase-2-handoff.md
git commit -S -m "docs: record Phase 2b-ii in the handoff"
```

---

## Self-Review

**Spec coverage.** Section 1 (store shape, non-templated ConfigMap, `Recreate`,
envelope, gzip/guard/bounded decompression, `bundle.path`, component projection,
save-failure policy, serialized writes, degradation, interface changes) → Tasks 1, 2, 5,
6, 8. Section 2 (recovery before serving, the bootstrap contract, one resume path, the
rewind, validation, NotFound-vs-unreadable, error text) → Tasks 3, 4, 8. Section 3
(context threading, `Decide`) → Tasks 5, 7. Section 4 (epoch, reconnect, server guard)
→ Task 9. The spec's testing table is distributed across the task test steps.

**Placeholder scan.** Tasks 2, 3 (validateLoaded), 6, 7, 9, and 10 describe some tests
by required assertion rather than pasting full bodies. That is deliberate where the test
must adopt an existing harness — `internal/engine`'s fake store, `internal/api`'s
`httptest` setup, `internal/observer`'s fake-clientset shape, the SPA's existing
`useEvents` test scaffolding — and inventing a parallel one would be the defect. Every
such step names the file to read and lists each required assertion. Literal code is
present wherever a new type, constant, or non-obvious control flow is introduced.

**Type consistency.** `ComponentState` and `Run.Components` are defined in Task 1 and
consumed unchanged in Tasks 6 and 3. `encodeRun`/`decodeRun` are defined in Task 1 and
consumed in Task 2. `Store.LoadCurrent`/`Delete` are defined in Task 1 and consumed in
Tasks 2, 3, and 4. `Engine.Recover`/`ErrStepConfig` are defined in Task 3 and consumed
in Task 8. `recoveredPending` is introduced in Task 3 and read in Task 4.
`Engine.Discard` is defined in Task 4 and routed in Task 7.

**One hazard worth naming.** Task 5 restructures `Decide` to do I/O outside `e.mu`, and
Task 7 changes its signature to take a context. Both touch the same function; Task 5
lands first, so Task 7's implementer must read the restructured version rather than the
original. If any step asks for a symbol no task defines, that is a plan defect — raise
it rather than inventing the symbol.

**One place the plan knowingly leaves a decision to the implementer.** Task 8's owner
resolution can walk the pod's `ownerReferences` chain (pod → ReplicaSet → Deployment)
via the downward API, or take the release name from an env var the chart sets. The
requirement is fixed — the reference must be the Deployment — and the mechanism is the
implementer's call; whichever is chosen must be stated in the task report.

## Unresolved questions

1. **Does a real cluster's snapshot fit under 800 KiB compressed?** 66–73 KB is the
   KWOK figure. Phase 4 on EKS is the first honest measurement. The guard makes a wrong
   guess a legible error rather than a corrupted run.
2. **Should recovery distinguish "restarted" from "crashed"?** A pod gracefully drained
   by `CancelAndWait` should already have persisted a terminal state, so a recovered
   non-terminal run implies an ungraceful exit. Deferred until there is a real instance.
3. **Does the cockpit need a distinct discard control?** This phase ships the endpoint
   and the engine contract. Whether Retry and Discard both belong on the recovered-run
   screen is a 2b-iii question, once that screen exists.
