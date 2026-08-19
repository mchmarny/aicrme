# Phase 3 — Prove Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the demo arc with a reference workload that stays running until the operator stops it.

**Architecture:** An optional `ActiveStep` interface lets one step finish its run at `StateActive` instead of `StateDone`. A new Prove step applies an embedded, label-owned workload into a dedicated namespace, watches gang placement, and returns with it running. Stop is the only exit, and every pre-Active failure cleans up and waits for absence before reporting.

**Tech Stack:** Go 1.26, `client-go` v0.36.3, `github.com/NVIDIA/aicr` v0.19.0 (pinned), React + Vite + Tailwind, Kind + KWOK.

**Spec:** `docs/superpowers/specs/2026-08-19-aicrme-phase-3-prove-design.md` (revision 2)

## Global Constraints

- **`github.com/NVIDIA/aicr` pinned at `v0.19.0`.** No version or `go.mod` changes.
- **Allocation is scalar `nvidia.com/gpu`, never DRA.** Measured: KWOK publishes no `ResourceSlices` and advertises scalar capacity only. Do not author `ResourceClaim`s or `ResourceSlice`s.
- **Coverage floor 80%** aggregate; all Go tests under `-race`.
- **Baseline counts that must not drop:** web 123, `internal/observer` 82, `internal/bus` 21.
- **`make qualify` must pass before every commit.**
- Commit with `-S`. **No `Co-Authored-By`, no sign-off (`-s`), no "Generated with" trailer.** Branch `phase-3-prove`, never `main`.
- **Never delete, skip, or weaken an existing test.**
- **13 components, 14 deployment actions.** Rows are actions.
- **Teardown is never automatic.** Stop is operator-initiated and operator-confirmed, always — not on restart, not on timeout, not as a side effect of starting another run.
- **Copy that makes a claim is a requirement.** Assert it in a test.
- **Prefer self-documenting code**; comments say *why*, never *what*.
- **The measured defect mode:** thirteen tests across these phases passed while the property they named was broken. For each test, ask what mutation would falsify it, and run the ones where the answer is unconvincing. **Print `git diff --numstat` and confirm non-zero before drawing any conclusion from a mutation.**

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/engine/active.go` (create) | `ActiveStep` interface; `isActive` helper |
| `internal/engine/engine.go` (modify) | `execute` finishes at `StateActive`; `Start` 409 guard; `Discard` rejection |
| `internal/engine/workload.go` (create) | `Workload` identity recorded on `Run` |
| `internal/prove/manifest.go` (create) | Embedded workload YAML + rendering + owned-kinds list |
| `internal/prove/workload.yaml` (create) | The workload itself, shape-first |
| `internal/prove/client.go` (create) | Apply / delete / wait-for-absence / list-owned against `kubernetes.Interface` |
| `internal/steps/prove.go` (create) | The Prove step |
| `internal/engine/reconcile.go` (create) | Startup reconciliation of orphaned workloads |
| `internal/api/prove.go` (create) | `POST /api/runs/{id}/stop` |
| `web/src/components/Prove.tsx` (create) | The Prove screen |
| `web/src/components/Wizard.tsx` (modify) | Route `active` to Prove; recovered-Active offers Stop not Discard |
| `test/e2e/prove.sh` (create) | Real-scheduler e2e |

---

## Task 1: `ActiveStep` and the `StateActive` transition

**Files:**
- Create: `internal/engine/active.go`
- Modify: `internal/engine/engine.go` (the `e.finish(ctx, epoch, StateDone, "")` at the end of `execute`)
- Test: `internal/engine/active_test.go`

**Interfaces:**
- Produces: `engine.ActiveStep` interface with `LeavesWorkloadRunning() bool`; `engine.isActive(s State) bool`.

- [ ] **Step 1: Write the failing tests**

```go
func TestRunWithActiveFinalStepEndsActive(t *testing.T) {
	e := newTestEngine(t, &fakeStep{phase: PhaseApply}, &fakeActiveStep{phase: PhaseProve, active: true})
	run := startAndWait(t, e)
	if run.State != StateActive {
		t.Errorf("State = %q, want %q", run.State, StateActive)
	}
}

// The interface is opt-IN. A step that implements it and returns false, and a
// step that does not implement it at all, must both finish at StateDone --
// otherwise every future step author has to know about this hook.
func TestRunWithNonActiveFinalStepEndsDone(t *testing.T) {
	for name, last := range map[string]Step{
		"does not implement ActiveStep": &fakeStep{phase: PhaseProve},
		"implements it, returns false":  &fakeActiveStep{phase: PhaseProve, active: false},
	} {
		t.Run(name, func(t *testing.T) {
			e := newTestEngine(t, &fakeStep{phase: PhaseApply}, last)
			if run := startAndWait(t, e); run.State != StateDone {
				t.Errorf("State = %q, want %q", run.State, StateDone)
			}
		})
	}
}

// Only the FINAL step decides. An ActiveStep in the middle leaves nothing
// running once later steps have run past it.
func TestActiveStepThatIsNotLastDoesNotMakeRunActive(t *testing.T) {
	e := newTestEngine(t, &fakeActiveStep{phase: PhaseApply, active: true}, &fakeStep{phase: PhaseProve})
	if run := startAndWait(t, e); run.State != StateDone {
		t.Errorf("State = %q, want %q", run.State, StateDone)
	}
}

// A failing run never reaches StateActive, whatever the final step claims.
func TestFailedRunNeverEndsActive(t *testing.T) {
	e := newTestEngine(t, &fakeStep{phase: PhaseApply, err: errors.New("boom")},
		&fakeActiveStep{phase: PhaseProve, active: true})
	if run := startAndWait(t, e); run.State != StateFailed {
		t.Errorf("State = %q, want %q", run.State, StateFailed)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -race ./internal/engine/ -run 'Active' -v`
Expected: FAIL — `fakeActiveStep` undefined, and `StateActive` never produced.

- [ ] **Step 3: Implement**

`internal/engine/active.go`:

```go
package engine

// ActiveStep is implemented by a Step that leaves something running in the
// cluster after Run returns. The engine finishes such a run at StateActive
// rather than StateDone, so the console keeps tracking what the step left
// behind and the operator retains a way to stop it.
//
// Deliberately an optional interface rather than a method on Step: Discover,
// Recommend, Bundle and Apply leave nothing running, and none of them should
// have to say so.
type ActiveStep interface {
	LeavesWorkloadRunning() bool
}

// isActive reports whether step wants its run to end at StateActive. Only the
// final step is consulted -- an ActiveStep followed by other steps has had
// its work superseded by the time the run ends.
func isActive(step Step) bool {
	as, ok := step.(ActiveStep)
	return ok && as.LeavesWorkloadRunning()
}
```

In `engine.go`, replace the tail of `execute`:

```go
	// The final step decides the terminal state. StateActive means something
	// this run created is still running in the cluster and only an operator
	// action ends it -- see Stop. A failure earlier in the loop returns
	// before reaching here, so a failed run can never land Active.
	terminal := StateDone
	if len(e.steps) > 0 && isActive(e.steps[len(e.steps)-1]) {
		terminal = StateActive
	}
	e.finish(ctx, epoch, terminal, "")
```

- [ ] **Step 4: Run to verify they pass**

Run: `go test -race ./internal/engine/ -run 'Active' -v`
Expected: PASS.

- [ ] **Step 5: Bite-proof**

Change `isActive` to ignore the boolean (`return ok`). Confirm `TestRunWithNonActiveFinalStepEndsDone/implements_it,_returns_false` fails **alone** — the other subtest and the not-implemented case must stay green. Print `git diff --numstat`, confirm non-zero, restore, re-run.

- [ ] **Step 6: `make qualify` and commit**

```bash
git add internal/engine/active.go internal/engine/active_test.go internal/engine/engine.go
git commit -S -m "feat(engine): let a final step finish its run at StateActive"
```

---

## Task 2: Guard a live workload — `Start` 409 and `Discard` rejection

**Files:**
- Modify: `internal/engine/engine.go` (`Start`'s live check; `Discard`'s `isLive` guard at ~`:862`)
- Test: `internal/engine/active_test.go`

**Interfaces:**
- Consumes: `StateActive` from Task 1.

**This closes a hole that exists in shipped code.** `Discard` guards only on `isLive(e.current.State)`, and `isLive` is `StateRunning || StateAwaitingDecision`. `StateActive` is not live, so today's `Discard` would nil `e.current`, delete the persisted record, leave the workload holding GPUs, and free `Start` — while telling the operator the run was discarded.

- [ ] **Step 1: Write the failing tests**

```go
func TestStartRejectsWhileWorkloadActive(t *testing.T) {
	e := newTestEngine(t, &fakeActiveStep{phase: PhaseProve, active: true})
	startAndWait(t, e)

	_, err := e.Start(context.Background())
	if err == nil {
		t.Fatal("Start() succeeded over a live workload, want conflict")
	}
	var se *aicrerrors.Error
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeConflict {
		t.Errorf("Start() error = %v, want ErrCodeConflict", err)
	}
	// The remedy has to be in the message: the operator's only way out is
	// Stop, and a bare "conflict" leaves them guessing.
	if !strings.Contains(err.Error(), "stop") {
		t.Errorf("Start() error = %q, want it to name stopping the workload", err)
	}
}

func TestDiscardRejectsActiveRun(t *testing.T) {
	e := newTestEngine(t, &fakeActiveStep{phase: PhaseProve, active: true})
	run := startAndWait(t, e)

	err := e.Discard(context.Background(), run.ID)
	if err == nil {
		t.Fatal("Discard() succeeded on an active run -- it would orphan the workload")
	}
	var se *aicrerrors.Error
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeConflict {
		t.Errorf("Discard() error = %v, want ErrCodeConflict", err)
	}
	if !strings.Contains(err.Error(), "stop") {
		t.Errorf("Discard() error = %q, want it to name stopping the workload", err)
	}
	// And the run must survive: a rejected Discard that still nils e.current
	// is the bug wearing a different hat.
	if got := e.CurrentID(); got != run.ID {
		t.Errorf("CurrentID() = %q after rejected Discard, want %q", got, run.ID)
	}
}

// Discard must still work for the states it exists to serve.
func TestDiscardStillAcceptsFailedRun(t *testing.T) {
	e := newTestEngine(t, &fakeStep{phase: PhaseApply, err: errors.New("boom")})
	run := startAndWait(t, e)
	if err := e.Discard(context.Background(), run.ID); err != nil {
		t.Fatalf("Discard() on a failed run error = %v, want nil", err)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -race ./internal/engine/ -run 'Discard|StartRejects' -v`
Expected: `TestStartRejectsWhileWorkloadActive` and `TestDiscardRejectsActiveRun` FAIL; `TestDiscardStillAcceptsFailedRun` passes already.

- [ ] **Step 3: Implement**

In `Start`, extend the existing live check:

```go
	// StateActive is not isLive -- it has no execute goroutine -- but it does
	// hold a workload in the cluster, and starting over it would abandon that
	// workload with nothing tracking it. Teardown is never a side effect of
	// starting something (approach.md, Reset).
	if e.current != nil && e.current.State == StateActive {
		e.mu.Unlock()
		return Run{}, aicrerrors.New(aicrerrors.ErrCodeConflict,
			"a workload from the previous run is still running; stop it before starting a new run")
	}
```

In `Discard`, immediately after the `isLive` guard:

```go
	// Not folded into isLive: isLive means "a goroutine owns this run", which
	// StateActive does not. This is a different claim -- "the cluster holds
	// something this run created" -- and discarding would delete the record
	// that is the only pointer to it.
	if e.current.State == StateActive {
		e.mu.Unlock()
		return aicrerrors.New(aicrerrors.ErrCodeConflict,
			"run holds a running workload; stop the workload before discarding")
	}
```

- [ ] **Step 4: Run to verify they pass**

Run: `go test -race ./internal/engine/ -v`
Expected: PASS, including every pre-existing engine test.

- [ ] **Step 5: Bite-proof**

Delete the `Discard` guard. Confirm `TestDiscardRejectsActiveRun` fails and `TestDiscardStillAcceptsFailedRun` still passes — the asymmetry is the assertion. Print `git diff --numstat`, restore.

- [ ] **Step 6: `make qualify` and commit**

```bash
git add internal/engine/engine.go internal/engine/active_test.go
git commit -S -m "fix(engine): reject Start and Discard while a workload is running"
```

---

## Task 3: The workload manifest and its ownership

**Files:**
- Create: `internal/prove/workload.yaml`, `internal/prove/manifest.go`
- Test: `internal/prove/manifest_test.go`

**Interfaces:**
- Produces:
  - `prove.Labels(runID string) map[string]string`
  - `prove.Render(runID, namespace string) ([]byte, error)`
  - `prove.Namespace = "aicrme-prove"`
  - `prove.OwnedKinds() []schema.GroupVersionResource`
  - `prove.WorkloadName(runID string) string`

- [ ] **Step 1: Write the failing tests**

```go
func TestRenderCarriesOwnershipLabels(t *testing.T) {
	out, err := prove.Render("run-abc", prove.Namespace)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	var obj map[string]any
	if err := yaml.Unmarshal(out, &obj); err != nil {
		t.Fatalf("Render() produced invalid YAML: %v", err)
	}
	labels := obj["metadata"].(map[string]any)["labels"].(map[string]any)
	for k, want := range map[string]string{
		"app.kubernetes.io/managed-by": "aicrme",
		"aicrme.dev/run-id":            "run-abc",
		"aicrme.dev/component":         "prove-workload",
	} {
		if got, _ := labels[k].(string); got != want {
			t.Errorf("label %q = %q, want %q", k, got, want)
		}
	}
}

// Identity must survive a restart without the persisted record, so the name
// is derived from the run ID rather than generated.
func TestWorkloadNameIsStableForARunID(t *testing.T) {
	if a, b := prove.WorkloadName("run-abc"), prove.WorkloadName("run-abc"); a != b {
		t.Errorf("WorkloadName not stable: %q vs %q", a, b)
	}
	if a, b := prove.WorkloadName("run-abc"), prove.WorkloadName("run-xyz"); a == b {
		t.Errorf("WorkloadName collides across runs: %q", a)
	}
}

// Scalar allocation, NOT DRA. KWOK publishes no ResourceSlices and AICR
// disables full-GPU DRA advertising by default, so a resourceClaim here would
// never bind. Two pods, eight GPUs each.
func TestWorkloadRequestsScalarGPUsAndNotDRA(t *testing.T) {
	out, _ := prove.Render("run-abc", prove.Namespace)
	s := string(out)
	if !strings.Contains(s, "nvidia.com/gpu") {
		t.Error("workload does not request nvidia.com/gpu")
	}
	for _, forbidden := range []string{"resourceClaims", "resourceClaimTemplate", "ResourceClaim"} {
		if strings.Contains(s, forbidden) {
			t.Errorf("workload uses DRA (%q); Phase 3 is scalar-only", forbidden)
		}
	}
}

func TestRenderTargetsTheGivenNamespace(t *testing.T) {
	out, _ := prove.Render("run-abc", "somewhere-else")
	var obj map[string]any
	_ = yaml.Unmarshal(out, &obj)
	if got := obj["metadata"].(map[string]any)["namespace"]; got != "somewhere-else" {
		t.Errorf("namespace = %v, want somewhere-else", got)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -race ./internal/prove/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement**

`internal/prove/workload.yaml` — shape-first. On KWOK nothing executes, so only the resource shape is observable; Phase 4 replaces the body with the real NCCL all-reduce without the step changing:

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: PLACEHOLDER_NAME
  namespace: PLACEHOLDER_NAMESPACE
  labels:
    app.kubernetes.io/managed-by: aicrme
    aicrme.dev/run-id: PLACEHOLDER_RUN_ID
    aicrme.dev/component: prove-workload
spec:
  completions: 2
  parallelism: 2
  completionMode: Indexed
  backoffLimit: 0
  template:
    metadata:
      labels:
        app.kubernetes.io/managed-by: aicrme
        aicrme.dev/run-id: PLACEHOLDER_RUN_ID
        aicrme.dev/component: prove-workload
      annotations:
        # kai-scheduler gangs on this. Two pods, all-or-nothing: the demo's
        # claim is that the cluster can place a multi-node GPU job, and a job
        # that limps along one pod at a time would not show that.
        kai.scheduler/queue: default
    spec:
      schedulerName: kai-scheduler
      restartPolicy: Never
      containers:
        - name: allreduce
          image: busybox:1.36
          command: ["sh", "-c", "echo placement proven; sleep infinity"]
          resources:
            limits:
              nvidia.com/gpu: 8
```

`internal/prove/manifest.go`:

```go
// Package prove owns the reference workload the Prove step runs, and the
// identity that lets the console find it again after a restart.
//
// Identity rests on LABELS, not on the persisted run record. Terminal saves
// are best-effort and the store can degrade to memory (internal/engine/
// cmstore.go), so a console that could only find its workload via the record
// would lose track of it exactly when the record was lost -- while the
// workload kept holding GPUs.
package prove

import (
	_ "embed"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Namespace is dedicated and never shared with a recipe component, so Stop
// and reconciliation can reason about what they own.
const Namespace = "aicrme-prove"

//go:embed workload.yaml
var workloadYAML string

func Labels(runID string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "aicrme",
		"aicrme.dev/run-id":            runID,
		"aicrme.dev/component":         "prove-workload",
	}
}

// WorkloadName is derived from the run ID rather than generated, so the same
// run always names the same object -- the property a restart depends on.
func WorkloadName(runID string) string { return "prove-" + runID }

// OwnedKinds enumerates what Prove creates. Stop and reconciliation act on
// this list, never on "everything in the namespace", so an object someone
// else put there is not collateral damage.
func OwnedKinds() []schema.GroupVersionResource {
	return []schema.GroupVersionResource{
		{Group: "batch", Version: "v1", Resource: "jobs"},
	}
}

func Render(runID, namespace string) ([]byte, error) {
	r := strings.NewReplacer(
		"PLACEHOLDER_NAME", WorkloadName(runID),
		"PLACEHOLDER_NAMESPACE", namespace,
		"PLACEHOLDER_RUN_ID", runID,
	)
	return []byte(r.Replace(workloadYAML)), nil
}
```

- [ ] **Step 4: Run to verify they pass**

Run: `go test -race ./internal/prove/ -v`
Expected: PASS.

- [ ] **Step 5: Bite-proof**

Add `resourceClaims: []` to `workload.yaml`. Confirm `TestWorkloadRequestsScalarGPUsAndNotDRA` fails alone. Print `git diff --numstat`, restore.

- [ ] **Step 6: `make qualify` and commit**

```bash
git add internal/prove/
git commit -S -m "feat(prove): the reference workload and its ownership labels"
```

---

## Task 4: The cluster client — apply, delete, wait for absence, list owned

**Files:**
- Create: `internal/prove/client.go`
- Test: `internal/prove/client_test.go`

**Interfaces:**
- Consumes: `Labels`, `Namespace`, `OwnedKinds`, `WorkloadName` from Task 3.
- Produces:
  - `prove.NewClient(kubernetes.Interface) *Client`
  - `(*Client) EnsureNamespace(ctx) error`
  - `(*Client) Apply(ctx, runID string) error`
  - `(*Client) Delete(ctx, runID string) error` — foreground, idempotent
  - `(*Client) WaitAbsent(ctx, runID string, timeout time.Duration) error`
  - `(*Client) ListOwned(ctx) ([]OwnedWorkload, error)` where `OwnedWorkload{RunID, Name, Namespace string}`

- [ ] **Step 1: Write the failing tests**

```go
// Idempotent: stopping an already-stopped workload succeeds. An operator who
// clicks Stop twice, or a reconciliation that races one, must not see an error.
func TestDeleteIsIdempotent(t *testing.T) {
	c := prove.NewClient(fake.NewSimpleClientset())
	if err := c.Delete(context.Background(), "run-abc"); err != nil {
		t.Errorf("Delete() on absent workload error = %v, want nil", err)
	}
}

func TestDeleteUsesForegroundPropagation(t *testing.T) {
	cs := fake.NewSimpleClientset(existingJob("run-abc"))
	var policy *metav1.DeletionPropagation
	cs.PrependReactor("delete", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		policy = a.(k8stesting.DeleteActionImpl).DeleteOptions.PropagationPolicy
		return false, nil, nil
	})
	_ = prove.NewClient(cs).Delete(context.Background(), "run-abc")
	if policy == nil || *policy != metav1.DeletePropagationForeground {
		t.Errorf("propagation = %v, want Foreground -- background deletion returns before the pods are gone", policy)
	}
}

// WaitAbsent must not return while the object still exists. A cleanup that
// reports success early is how a "failed" run leaves GPUs allocated.
func TestWaitAbsentBlocksWhilePresent(t *testing.T) {
	c := prove.NewClient(fake.NewSimpleClientset(existingJob("run-abc")))
	err := c.WaitAbsent(context.Background(), "run-abc", 200*time.Millisecond)
	if err == nil {
		t.Fatal("WaitAbsent() returned nil while the workload still exists")
	}
}

func TestWaitAbsentReturnsOnceGone(t *testing.T) {
	c := prove.NewClient(fake.NewSimpleClientset())
	if err := c.WaitAbsent(context.Background(), "run-abc", time.Second); err != nil {
		t.Errorf("WaitAbsent() error = %v, want nil", err)
	}
}

// Reconciliation finds workloads by label, so a record-less console can still
// see what it left behind.
func TestListOwnedFindsByLabelNotByRecord(t *testing.T) {
	cs := fake.NewSimpleClientset(existingJob("run-abc"), unrelatedJob())
	got, err := prove.NewClient(cs).ListOwned(context.Background())
	if err != nil {
		t.Fatalf("ListOwned() error = %v", err)
	}
	if len(got) != 1 || got[0].RunID != "run-abc" {
		t.Errorf("ListOwned() = %+v, want exactly the aicrme-owned job for run-abc", got)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -race ./internal/prove/ -run Client -v`
Expected: FAIL — `NewClient` undefined.

- [ ] **Step 3: Implement `internal/prove/client.go`**

Key requirements the tests pin: `Delete` uses `metav1.DeletePropagationForeground` and treats `IsNotFound` as success; `WaitAbsent` polls until `IsNotFound` or the timeout elapses and returns an error on timeout; `ListOwned` uses `LabelSelector: "app.kubernetes.io/managed-by=aicrme,aicrme.dev/component=prove-workload"` and reads the run ID from the `aicrme.dev/run-id` label.

- [ ] **Step 4: Run to verify they pass**

Run: `go test -race ./internal/prove/ -v`

- [ ] **Step 5: Bite-proof**

Change `Delete` to `DeletePropagationBackground`. Confirm `TestDeleteUsesForegroundPropagation` fails alone. Print `git diff --numstat`, restore.

- [ ] **Step 6: `make qualify` and commit**

```bash
git add internal/prove/client.go internal/prove/client_test.go
git commit -S -m "feat(prove): cluster client with foreground, idempotent, wait-for-absence semantics"
```

---

## Task 5: The Prove step

**Files:**
- Create: `internal/steps/prove.go`
- Modify: `cmd/aicrme/main.go` (append to the step list)
- Test: `internal/steps/prove_test.go`

**Interfaces:**
- Consumes: `prove.Client` (Task 4), `engine.ActiveStep` (Task 1).
- Produces: `steps.NewProve(c *prove.Client, cfg steps.ProveConfig) engine.Step`, with `ProveConfig{GangTimeout time.Duration}`.

- [ ] **Step 1: Write the failing tests**

```go
func TestProveImplementsActiveStep(t *testing.T) {
	s := steps.NewProve(prove.NewClient(fake.NewSimpleClientset()), steps.ProveConfig{})
	as, ok := s.(engine.ActiveStep)
	if !ok || !as.LeavesWorkloadRunning() {
		t.Error("Prove must implement ActiveStep and leave its workload running")
	}
}

func TestProveAppliesWorkloadAndRecordsIdentity(t *testing.T) {
	cs := fake.NewSimpleClientset()
	run := newRun("run-abc")
	err := steps.NewProve(prove.NewClient(cs), steps.ProveConfig{GangTimeout: time.Second}).
		Run(context.Background(), run, func(bus.Event) {})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if run.Workload.Name != prove.WorkloadName("run-abc") || run.Workload.Namespace != prove.Namespace {
		t.Errorf("Workload = %+v, want the rendered identity", run.Workload)
	}
}

// A gang that never places is a failure -- and the workload must be GONE
// before the step returns, because a pending gang can still place later.
func TestProveCleansUpWhenGangNeverPlaces(t *testing.T) {
	cs := fake.NewSimpleClientset()
	// no pods ever become Running
	err := steps.NewProve(prove.NewClient(cs), steps.ProveConfig{GangTimeout: 100 * time.Millisecond}).
		Run(context.Background(), newRun("run-abc"), func(bus.Event) {})
	if err == nil {
		t.Fatal("Run() succeeded though the gang never placed")
	}
	if _, getErr := cs.BatchV1().Jobs(prove.Namespace).
		Get(context.Background(), prove.WorkloadName("run-abc"), metav1.GetOptions{}); !apierrors.IsNotFound(getErr) {
		t.Error("workload still exists after a gang timeout -- it can still place and hold GPUs")
	}
}

// If cleanup itself fails, the error must say so rather than reporting a
// clean failure over an uncleaned cluster.
func TestProveReportsCleanupFailureDistinctly(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("delete refused")
	})
	err := steps.NewProve(prove.NewClient(cs), steps.ProveConfig{GangTimeout: 50 * time.Millisecond}).
		Run(context.Background(), newRun("run-abc"), func(bus.Event) {})
	if err == nil || !strings.Contains(err.Error(), "cleanup") {
		t.Errorf("Run() error = %v, want it to name the failed cleanup", err)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -race ./internal/steps/ -run Prove -v`
Expected: FAIL — `NewProve` undefined.

- [ ] **Step 3: Implement**

`Phase()` returns `engine.PhaseProve`; `Requires()` returns `nil` (Prove adds no operator question); `LeavesWorkloadRunning()` returns `true`. `Run` ensures the namespace, applies the rendered workload, records `run.Workload`, waits up to `GangTimeout` for both pods to be placed, and emits one event per placement. On timeout it deletes, waits for absence, and returns an error; if the delete or the wait fails, the returned error names the cleanup failure.

Wire into `main.go` after `NewApply`.

- [ ] **Step 4: Run to verify they pass**

Run: `go test -race ./internal/steps/ -v`

- [ ] **Step 5: Bite-proof**

Remove the cleanup call from the timeout path. Confirm `TestProveCleansUpWhenGangNeverPlaces` fails alone. Print `git diff --numstat`, restore.

- [ ] **Step 6: `make qualify` and commit**

```bash
git add internal/steps/prove.go internal/steps/prove_test.go cmd/aicrme/main.go
git commit -S -m "feat(steps): the Prove step, cleaning up every path that does not reach Active"
```

---

## Task 6: `Run.Workload` and its persistence

**Files:**
- Create: `internal/engine/workload.go`
- Modify: `internal/engine/run.go` (add the field), `internal/engine/envelope.go` if the record needs it
- Test: `internal/engine/workload_test.go`

**Interfaces:**
- Produces: `engine.Workload{Namespace, Kind, Name string}`, `Run.Workload Workload` with `json:"workload,omitempty"`.

- [ ] **Step 1: Write the failing test**

```go
// The record carries the workload so the console can name it after a restart
// -- but correctness must not DEPEND on it, because terminal saves are
// best-effort and the store can degrade to memory. Task 7's reconciliation
// covers the case where this is missing; this test only pins the round trip.
func TestWorkloadSurvivesTheRecordRoundTrip(t *testing.T) {
	in := Run{ID: "run-abc", State: StateActive,
		Workload: Workload{Namespace: "aicrme-prove", Kind: "Job", Name: "prove-run-abc"}}
	out, err := decodeRun(mustEncodeRun(t, in))
	if err != nil {
		t.Fatalf("decodeRun() error = %v", err)
	}
	if out.Workload != in.Workload {
		t.Errorf("Workload = %+v, want %+v", out.Workload, in.Workload)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test -race ./internal/engine/ -run Workload -v`

- [ ] **Step 3: Implement**

- [ ] **Step 4: Run to verify it passes**

- [ ] **Step 5: `make qualify` and commit**

```bash
git add internal/engine/workload.go internal/engine/workload_test.go internal/engine/run.go
git commit -S -m "feat(engine): record the workload a run left running"
```

---

## Task 7: Stop

**Files:**
- Modify: `internal/engine/engine.go` (add `Stop`)
- Create: `internal/api/prove.go`
- Modify: `internal/api/server.go` (route)
- Test: `internal/engine/active_test.go`, `internal/api/prove_test.go`

**Interfaces:**
- Produces: `(*Engine) Stop(ctx context.Context, runID string) error`; `POST /api/runs/{id}/stop`.

- [ ] **Step 1: Write the failing tests**

```go
func TestStopEndsTheRunAtDone(t *testing.T) { /* Active -> Stop -> StateDone, workload gone */ }

func TestStopIsIdempotent(t *testing.T) { /* second Stop returns nil */ }

// A failed Stop must NOT move the run to Done. Reporting success over a
// workload that is still running is the one outcome that must never happen.
func TestFailedStopLeavesRunActive(t *testing.T) {
	// delete reactor errors
	// assert: Stop returns an error, run.State is still StateActive,
	// and a subsequent Start still returns ErrCodeConflict.
}

func TestStopRejectsNonActiveRun(t *testing.T) { /* StateDone -> conflict */ }
```

- [ ] **Step 2: Run to verify they fail**

- [ ] **Step 3: Implement**

`Stop` deletes via `prove.Client` with foreground propagation, waits for absence, and only then finishes the run at `StateDone`. On any failure it leaves the run `StateActive` and returns the error. Never called by anything except the handler — no timer, no restart path, no `Start`.

- [ ] **Step 4: Run to verify they pass**

- [ ] **Step 5: Bite-proof**

Make `Stop` finish at `StateDone` before waiting for absence. Confirm `TestFailedStopLeavesRunActive` fails alone. Print `git diff --numstat`, restore.

- [ ] **Step 6: `make qualify` and commit**

```bash
git add internal/engine/engine.go internal/api/prove.go internal/api/server.go internal/engine/active_test.go internal/api/prove_test.go
git commit -S -m "feat(api): operator-initiated Stop, the only exit from StateActive"
```

---

## Task 8: Startup reconciliation

**Files:**
- Create: `internal/engine/reconcile.go`
- Modify: `cmd/aicrme/main.go` (call after `Recover`)
- Test: `internal/engine/reconcile_test.go`

**Interfaces:**
- Consumes: `prove.Client.ListOwned` (Task 4).
- Produces: `(*Engine) ReconcileWorkloads(ctx context.Context, c *prove.Client) error`.

- [ ] **Step 1: Write the failing tests — one per spec case**

```go
// Case 1: workload exists, run recovered Active -> stays Active, Stop offered.
func TestReconcileKeepsActiveRunWithLiveWorkload(t *testing.T) { /* ... */ }

// Case 2: workload exists, NO record (the store lost it) -> adopt into a
// synthetic Active run so the operator can Stop it. NEVER silently delete.
func TestReconcileAdoptsOrphanedWorkload(t *testing.T) {
	// assert: CurrentID() names a run, State is StateActive, and the
	// workload still exists -- adoption must not delete it.
}

// Case 3: run recovered Active, workload absent -> finish at StateDone.
func TestReconcileFinishesActiveRunWhoseWorkloadIsGone(t *testing.T) { /* ... */ }
```

- [ ] **Step 2: Run to verify they fail**

- [ ] **Step 3: Implement**

- [ ] **Step 4: Run to verify they pass**

- [ ] **Step 5: Bite-proof**

Make case 2 delete the orphan instead of adopting it. Confirm `TestReconcileAdoptsOrphanedWorkload` fails alone. Print `git diff --numstat`, restore.

- [ ] **Step 6: `make qualify` and commit**

```bash
git add internal/engine/reconcile.go internal/engine/reconcile_test.go cmd/aicrme/main.go
git commit -S -m "feat(engine): reconcile orphaned workloads at startup"
```

---

## Task 9: The Prove screen

**Files:**
- Create: `web/src/components/Prove.tsx`, `web/src/components/Prove.test.tsx`
- Modify: `web/src/components/Wizard.tsx`

**Interfaces:**
- Consumes: run state `active`, `POST /api/runs/{id}/stop`.

- [ ] **Step 1: Write the failing tests**

```tsx
it('shows the allocation decision and a Stop control while active', () => { /* ... */ })

// The claim the screen must NOT make on a simulated cluster.
it('labels a simulated cluster and makes no throughput claim', () => {
  render(<Prove {...simulated} />)
  expect(screen.getByText(/simulated cluster, no GPU hardware/i)).toBeInTheDocument()
  expect(screen.queryByText(/GB\/s/)).not.toBeInTheDocument()
})

// Recovered-Active must offer Stop, never Discard: Discard is rejected by the
// engine and would leave the operator at a dead end.
it('offers Stop and not Discard for a recovered active run', () => {
  render(<Wizard {...recoveredActive} />)
  expect(screen.getByRole('button', { name: /stop workload/i })).toBeInTheDocument()
  expect(screen.queryByRole('button', { name: /discard/i })).not.toBeInTheDocument()
})
```

- [ ] **Step 2: Run to verify they fail** — `npx vitest run`

- [ ] **Step 3: Implement**

- [ ] **Step 4: Run to verify they pass** — web total must be ≥ 126 (123 baseline + 3)

- [ ] **Step 5: Bite-proof**

Render the Discard button for `active`. Confirm the recovered-Active test fails alone. Print `git diff --numstat`, restore.

- [ ] **Step 6: `make qualify` and commit**

```bash
git add web/src/components/Prove.tsx web/src/components/Prove.test.tsx web/src/components/Wizard.tsx
git commit -S -m "feat(cockpit): the Prove screen, with Stop instead of Discard when active"
```

---

## Task 10: e2e — prove a real scheduler decided

**Files:**
- Create: `test/e2e/prove.sh`
- Modify: `.github/workflows/e2e.yaml` (add the job)

**This task exists because fake-client unit tests cannot cover admission, controllers, finalizers, or watch behaviour.**

- [ ] **Step 1: Add the assertions**

1. **kai-scheduler runs on a real Kind worker.** KAI has a catch-all toleration, so its controllers can land on a KWOK node and receive synthesized `Ready` **without ever executing** — in which case nothing scheduled anything and every other assertion here is vacuous. Assert its pod's `spec.nodeName` matches `aicrme-e2e-*-worker*`.
2. **All gang members schedule together across exactly two distinct simulated GPU nodes**, each consuming 8 of 8 allocatable.
3. **Restart after Active** — delete the console pod; the run returns to `active` and Stop still works.
4. **Partial apply** — cleanup removes every created object.
5. **Gang timeout** — cleanup waits for absence; a late-placing gang does not survive.
6. **Failed Stop** — the run stays `active` and `Start` stays blocked (409).

Each assertion prints the counts it matched, and carries an inverted-input self-check, following `apply-real.sh`'s pattern — **an e2e assertion that matches nothing passes silently**.

- [ ] **Step 2: Run it locally and paste real output**

- [ ] **Step 3: Commit**

```bash
git add test/e2e/prove.sh .github/workflows/e2e.yaml
git commit -S -m "test(e2e): prove a real scheduler placed the gang"
```

---

## Task 11: Documentation

**Files:**
- Modify: `docs/phase-2-handoff.md`, `DEMO.md`, `approach.md`

- [ ] **Step 1** Record the three test gaps next to the code and in the handoff: no workload executes on KWOK; the workload body is unexercised; DRA is entirely unexercised.
- [ ] **Step 2** Update `DEMO.md` — the arc now ends at a running workload with a Stop control, not at a completed Apply.
- [ ] **Step 3** Note in `approach.md` that Reset (Phase 5) must call Stop before uninstalling components.
- [ ] **Step 4** `make qualify`, commit.

---

## Self-Review

**Spec coverage.** §1 scalar allocation → Task 3. §2 `ActiveStep` → Task 1. §3 ownership/durability → Tasks 3, 6, 8. §4 the step → Task 5. §5 `Start` guard → Task 2. §6 `Discard` rejection → Task 2. §7 Stop → Task 7. §8 failure cleanup → Tasks 4, 5. §9 screen → Task 9. Testing → Task 10. Gaps → Task 11.

**Placeholder scan.** Tasks 6–8 and 10 describe some tests by required assertion rather than pasting bodies. That is deliberate where the test must adopt an existing harness — `internal/engine`'s `newTestEngine`/`startAndWait`, the fake clientset's reactor idiom, `apply-real.sh`'s `jq` filters — and inventing a parallel one would be the defect. Literal code appears wherever a new type or non-obvious control flow is introduced.

**Type consistency.** `prove.Namespace`, `prove.Labels`, `prove.WorkloadName`, `prove.OwnedKinds`, `prove.Render` are defined in Task 3 and consumed unchanged in 4, 5, 8. `engine.Workload` is defined in Task 6 and written by Task 5 — **Task 5's implementer must be told Task 6's field exists**, or they will invent one.

**One ordering hazard.** Task 5 writes `run.Workload`, which Task 6 defines. Either swap them at execution time or have Task 5's dispatch carry the exact struct. Swapping is cleaner: **execute Task 6 before Task 5.**

## Unresolved questions

1. **`GangTimeout` default.** The spec leaves it open; best answered against a real `make demo`. Start at 3 minutes and revisit with evidence.
2. **Does the Prove screen replace the cockpit or extend it?** Task 9 assumes it replaces it, matching the wizard's existing one-screen-per-phase shape. Revisit if the pipeline should stay visible.
3. **Should adopting an orphaned workload require operator confirmation?** Task 8 adopts silently. A click would be more honest about the console having found something it did not start.
