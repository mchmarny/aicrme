# aicrme Phase 2b-i Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a real 10-to-20-minute Apply watchable (an observer narrating cluster state changes) and interruptible (a SIGTERM path that stops the run without stranding a Helm release).

**Architecture:** `internal/observer` runs shared informers over DaemonSets, Deployments, and Nodes, computes a small normalized state per object, and publishes a `bus` event **only when that state changes** — never relaying raw watch events. Separately, `Engine` gains `CancelAndWait`, and `cmd/aicrme/main.go` drains mutating HTTP routes before running HTTP shutdown and engine cleanup concurrently, so the process never returns before the `deploy.sh` process tree is reaped.

**Tech Stack:** Go (see `.go-version`), `k8s.io/client-go v0.36.3` (informers + fake clientset), `github.com/NVIDIA/aicr v0.19.0` (pinned), Helm chart.

**Spec:** `docs/superpowers/specs/2026-08-16-aicrme-phase-2b-i-design.md`

## Global Constraints

- **AICR is pinned to `v0.19.0`.** Never bump it. `make check-aicr-pin` enforces that `go.mod`, `.settings.yaml` `dependencies.aicr`, and `cmd/aicrme/main.go`'s `defaultSnapshotAgentImage` tag all agree.
- **`k8s.io/client-go` stays at `v0.36.3`.** Task 6 runs `go mod tidy`, which must change only `// indirect` markers — **verify no version changes appear in that diff.**
- **Coverage floor is 80%** (`.settings.yaml` `quality.coverage_threshold`).
- **All tests run under `-race`.** This phase is concurrency work; `-race` is the point, not a formality.
- **`make qualify` must pass before every commit** — exactly what CI runs: `web lint lint-shell test-chart test-web test-coverage check-aicr-pin`.
- **Commits are signed (`git commit -S`), no `Co-Authored-By`, no sign-off.**
- **Errors use `github.com/NVIDIA/aicr/pkg/errors`** — `aicrerrors.New(code, msg)`, `aicrerrors.Wrap(...)`. `internal/api`'s `writeErr` maps those codes to HTTP status.
- **The observer aggregates; it never relays.** Emit only when a computed state changes. Informer `UpdateFunc` fires on `managedFields` churn, and `internal/bus/bus.go` drops live events for any subscriber more than 256 behind.
- **Cache normalized state, never the formatted message.** The Node message is a transition (`0 → 8`); caching it would make a repeated update compute `8 → 8`, compare unequal, and emit again.
- **Prefer self-documenting code**, but keep a comment wherever a non-obvious decision was made — explain *why*, never *what*.
- **Do not touch** the ConfigMap store, the `bus.nextID` epoch, `StateActive`, or add any new `engine.State`. Those are 2b-ii or later.

---

## File Structure

**New**

| File | Responsibility |
|---|---|
| `internal/observer/observer.go` | `Observer`, `RunScope`, the guarded state cache, `New`, `Start` |
| `internal/observer/handlers.go` | The three resource handlers and their state/message computation |
| `internal/observer/observer_test.go` | Fake-clientset-driven tests for all handlers |

**Modified**

`internal/engine/engine.go` (cancel func, done channel, `CancelAndWait`, `CurrentID`, detached terminal save), `internal/engine/engine_test.go`, `internal/api/server.go` (draining), `internal/api/server_test.go`, `cmd/aicrme/main.go` (client construction, observer wiring, shutdown ordering), `cmd/aicrme/main_test.go`, `charts/aicrme/values.yaml`, `charts/aicrme/templates/deployment.yaml`, `test/chart/contract.sh`, `go.mod`.

---

## Task 1: Engine cancellation with a completion contract

**Files:**
- Modify: `internal/engine/engine.go`
- Modify: `internal/engine/engine_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - `func (e *Engine) CancelAndWait(ctx context.Context) error` — consumed by Task 2.
  - `func (e *Engine) CurrentID() (string, bool)` — consumed by Task 6. Returns the current run's ID without cloning.

### Why `CurrentID` exists

`Engine.Current()` calls `Run.Clone()`, which deep-copies **every artifact** — including `snapshot.yaml`, ~70 KB in the committed fixture and larger on a real cluster. Task 6's observer needs the current run ID on every watch event to decide whether its cached scope is stale. Calling `Current()` for that would copy megabytes per second. `CurrentID` returns just the string under the same lock.

- [ ] **Step 1: Write the failing tests**

Add to `internal/engine/engine_test.go`. `blockingStep` blocks until its context dies, which is what a cancelled `deploy.sh` does:

```go
type blockingStep struct {
	phase   engine.Phase
	entered chan struct{}
}

func (b *blockingStep) Phase() engine.Phase { return b.phase }
func (b *blockingStep) Requires() []string  { return nil }
func (b *blockingStep) Run(ctx context.Context, _ *engine.Run, _ engine.Emit) error {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestCancelAndWaitStopsAnInFlightRun(t *testing.T) {
	b := bus.New(64)
	step := &blockingStep{phase: engine.PhaseApply, entered: make(chan struct{}, 1)}
	e := engine.New(b, engine.NewMemoryStore(), step)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-step.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("step never entered")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.CancelAndWait(ctx); err != nil {
		t.Fatalf("CancelAndWait() error = %v", err)
	}

	// CancelAndWait must not return until the terminal state is persisted --
	// a caller that returns early would let main exit before the run is done.
	got, err := e.Get(run.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.State != engine.StateFailed {
		t.Errorf("State = %q, want %q", got.State, engine.StateFailed)
	}
	if !strings.Contains(got.Err, "cancelled") {
		t.Errorf("Err = %q, want it to say the run was cancelled", got.Err)
	}
}

func TestCancelAndWaitIsIdempotentAndSafeWithNoRun(t *testing.T) {
	e := engine.New(bus.New(8), engine.NewMemoryStore(), newFakeStep(engine.PhaseDiscover))
	ctx := context.Background()

	// No run has ever started.
	if err := e.CancelAndWait(ctx); err != nil {
		t.Fatalf("CancelAndWait() with no run error = %v", err)
	}

	run, err := e.Start(ctx)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateDone)

	// Run already finished on its own; cancelling is a no-op, twice.
	if err := e.CancelAndWait(ctx); err != nil {
		t.Fatalf("first CancelAndWait() error = %v", err)
	}
	if err := e.CancelAndWait(ctx); err != nil {
		t.Fatalf("second CancelAndWait() error = %v", err)
	}
}

func TestCancelAndWaitTimesOutRatherThanBlockingForever(t *testing.T) {
	b := bus.New(64)
	// A step that ignores its context entirely -- the pathological case
	// CancelAndWait's deadline exists for.
	stuck := &stuckStep{phase: engine.PhaseApply, entered: make(chan struct{}, 1), release: make(chan struct{})}
	e := engine.New(b, engine.NewMemoryStore(), stuck)

	if _, err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-stuck.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("step never entered")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := e.CancelAndWait(ctx); err == nil {
		t.Error("CancelAndWait() error = nil, want a timeout error")
	}
	close(stuck.release)
}

func TestCancelWhileParkedForDecisionsFinishesTheRun(t *testing.T) {
	b := bus.New(64)
	e := engine.New(b, engine.NewMemoryStore(), newFakeStep(engine.PhaseRecommend, "intent"))

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateAwaitingDecision)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.CancelAndWait(ctx); err != nil {
		t.Fatalf("CancelAndWait() error = %v", err)
	}

	// A run frozen at a gate with no goroutine is the wedge class Ruling 13
	// fixed for Save failures; cancellation must not reintroduce it.
	got, _ := e.Get(run.ID)
	if got.State != engine.StateFailed {
		t.Errorf("State = %q, want %q", got.State, engine.StateFailed)
	}
}

func TestCurrentIDDoesNotRequireAClone(t *testing.T) {
	e := engine.New(bus.New(8), engine.NewMemoryStore(), newFakeStep(engine.PhaseDiscover))

	if _, ok := e.CurrentID(); ok {
		t.Error("CurrentID() ok = true before any run, want false")
	}

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	id, ok := e.CurrentID()
	if !ok || id != run.ID {
		t.Errorf("CurrentID() = %q, %v, want %q, true", id, ok, run.ID)
	}
}
```

Add `stuckStep` alongside `blockingStep`:

```go
type stuckStep struct {
	phase   engine.Phase
	entered chan struct{}
	release chan struct{}
}

func (s *stuckStep) Phase() engine.Phase { return s.phase }
func (s *stuckStep) Requires() []string  { return nil }
func (s *stuckStep) Run(_ context.Context, _ *engine.Run, _ engine.Emit) error {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-s.release
	return nil
}
```

Add `"strings"` and `"sync"` to the test file's imports if absent.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/engine/ -race -run 'TestCancel|TestCurrentID' -v`
Expected: FAIL — `e.CancelAndWait undefined`, `e.CurrentID undefined`.

- [ ] **Step 3: Add the fields and `CurrentID`**

In `internal/engine/engine.go`, add to the `Engine` struct after `epoch`:

```go
	// cancel stops the in-flight run's step context; done closes once its
	// execute goroutine has exited AND persisted a terminal state. A cancel
	// func alone would tell a caller the run was asked to stop but not when
	// it actually had -- and the whole point of shutdown ordering is not
	// returning from main until the deploy.sh process tree is reaped.
	cancel context.CancelFunc
	done   chan struct{}
```

and:

```go
// CurrentID returns the current run's ID without cloning the run. Current()
// deep-copies every artifact -- including the raw snapshot, which is tens of
// kilobytes -- so a caller that only needs the ID on a hot path (the
// observer, on every watch event) must not go through it.
func (e *Engine) CurrentID() (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil {
		return "", false
	}
	return e.current.ID, true
}
```

- [ ] **Step 4: Make `Start` and `Retry` cancellable and waitable**

In `Start`, inside the existing `e.mu.Lock()` block where `e.epoch++` happens, replace the goroutine launch. The run context derives from the detached one so an HTTP request ending still cannot kill a 20-minute Apply:

```go
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	e.cancel = cancel
	e.done = make(chan struct{})
	done := e.done
```

and launch with:

```go
	go func() {
		defer close(done)
		e.execute(runCtx, epoch)
	}()
```

`close(done)` is deferred around the whole of `execute`, so it fires **after** `execute`'s final `finish` has persisted — which is what makes `CancelAndWait` a real completion contract rather than a fire-and-forget.

Apply the identical change in `Retry`, deriving from `context.Background()` as it does today.

- [ ] **Step 5: Implement `CancelAndWait`**

```go
// CancelAndWait cancels the in-flight run and blocks until its execute
// goroutine has exited and persisted a terminal state, or ctx expires.
//
// Idempotent and safe with no run in flight: a second call sees an
// already-closed done channel and returns immediately. The deadline matters
// -- a step that ignores its context would otherwise block shutdown forever,
// and Kubernetes will SIGKILL the pod at terminationGracePeriodSeconds
// regardless of what this returns.
func (e *Engine) CancelAndWait(ctx context.Context) error {
	e.mu.Lock()
	cancel, done := e.cancel, e.done
	e.mu.Unlock()

	if cancel == nil || done == nil {
		return nil
	}
	cancel()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return aicrerrors.New(aicrerrors.ErrCodeTimeout,
			"timed out waiting for the in-flight run to stop")
	}
}
```

- [ ] **Step 6: Persist the terminal state under a detached context**

`finish` currently saves with the same context the step ran under, so once cancelled a real store's write fails and the run persists as `running` — the wedge class Ruling 13 fixed for `Save` failures, inert only because `memoryStore.Save` ignores its context.

Add near the top of `internal/engine/engine.go`:

```go
// terminalSaveTimeout bounds the detached write finish performs. Terminal
// state must persist even when the run was cancelled, so this write cannot
// use the (now-dead) step context -- but it also cannot block shutdown
// indefinitely against an unreachable API server.
const terminalSaveTimeout = 5 * time.Second
```

and in `finish`, replace `_ = e.store.Save(ctx, snapshot)` with:

```go
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), terminalSaveTimeout)
	defer saveCancel()
	_ = e.store.Save(saveCtx, snapshot)
```

- [ ] **Step 7: Finish the run when cancelled at a decision gate**

In `awaitDecisions`, the `case <-ctx.Done(): return false` branch leaves the run in `StateAwaitingDecision` with no goroutine. Change that branch to finish first:

```go
		case <-ctx.Done():
			// A run frozen mid-gate with no goroutine is the same wedge
			// class Ruling 13 fixed for Save failures. Harmless with the
			// memory store (the process is exiting) but 2b-ii persists this.
			e.finish(ctx, epoch, StateFailed, "cancelled: console shutting down")
			return false
```

Ensure `runStep`'s error path produces the same message when the step returns `ctx.Err()` — the `blockingStep` test asserts `"cancelled"` appears in `Run.Err`. If `ctx.Err()` alone does not contain it, wrap in `runStep` when `errors.Is(err, context.Canceled)`.

- [ ] **Step 8: Run to verify they pass**

Run: `go test ./internal/engine/ -race -count=1 -v`
Expected: PASS, all tests.

- [ ] **Step 9: Bite-proof the completion contract**

Move `close(done)` from a `defer` around `execute` to *before* the `execute` call (i.e. close it immediately, then run). Re-run `TestCancelAndWaitStopsAnInFlightRun` with `-count=5`. It must FAIL — `CancelAndWait` would return before the terminal state was persisted, so `Get` sees a non-terminal state. Restore, confirm green.

**Make sure the mutation still compiles**, and run with `-v` so you can see which tests failed and which stayed green. A mutation that breaks the build proves nothing. Record the output.

- [ ] **Step 10: Qualify and commit**

```bash
make qualify
git add internal/engine/
git commit -S -m "feat(engine): CancelAndWait with a real completion contract

A cancel func alone tells a caller the run was asked to stop, not when it
actually stopped -- and the whole point of shutdown ordering is not
returning from main until the deploy.sh process tree is reaped. done now
closes only after execute has persisted a terminal state.

finish persisted with the step's own context, so once cancelled a real
store would reject the write and leave the run recorded as running --
the wedge class Ruling 13 fixed for Save failures, inert today only
because memoryStore ignores its context. Now a bounded detached write.

CurrentID exists because Current() deep-copies every artifact including
the raw snapshot; the observer needs the run ID on every watch event."
```

---

## Task 2: Drain-then-shutdown ordering

**Files:**
- Modify: `internal/api/server.go`
- Modify: `internal/api/server_test.go`
- Modify: `cmd/aicrme/main.go`
- Modify: `charts/aicrme/values.yaml`
- Modify: `charts/aicrme/templates/deployment.yaml`
- Modify: `test/chart/contract.sh`

**Interfaces:**
- Consumes: `Engine.CancelAndWait` (Task 1).
- Produces: `func (s *Server) Drain()` — marks the server draining; mutating routes then return 503.

### The hole this closes

Cancellation lands the run in `StateFailed`, which `isLive` does **not** consider live. So during a 15-second wait with HTTP still fully open, a `POST /api/runs` would cheerfully start a *new* run that shutdown then kills mid-flight. Draining first removes that window.

The invariant is **"do not return from `main` before the process tree is reaped"** — not "do not begin HTTP shutdown first." Those are different, and treating them as the same is what forced a sequential budget that does not fit Kubernetes' 30-second default grace period.

- [ ] **Step 1: Write the failing tests**

Add to `internal/api/server_test.go`, following the file's existing `httptest` helper style (read it first):

- `TestDrainRejectsMutations` — after `srv.Drain()`, `POST /api/runs` returns `503`.
- `TestDrainKeepsSafeMethodsServing` — after `srv.Drain()`, `GET /healthz` still returns `200` and `GET /api/events` still opens, so a connected browser sees the timeline through shutdown.
- `TestNotDrainingByDefault` — a fresh server accepts `POST /api/runs` normally, so the middleware cannot trivially pass by rejecting everything.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/api/ -race -run TestDrain -v`
Expected: FAIL — `srv.Drain undefined`.

- [ ] **Step 3: Implement draining**

In `internal/api/server.go`, add an `atomic.Bool` field to `Server`, a `Drain()` method that sets it, and a middleware wrapping the existing handler chain:

```go
// requireNotDraining rejects state-changing requests once shutdown has
// begun. Cancelling the in-flight run leaves it StateFailed, which isLive
// does not treat as live -- so without this, a POST /api/runs arriving
// during the shutdown wait would start a fresh run that shutdown then kills
// mid-flight. Safe methods keep serving so a connected browser watches the
// timeline through shutdown.
```

Place it inside the existing `securityHeaders(requireSameOrigin(mux))` chain. Exempt the same method set `requireSameOrigin` already exempts (`GET`, `HEAD`, `OPTIONS`). Return `503` with `Retry-After: 0` and a plain body.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/api/ -race -count=1`
Expected: PASS.

- [ ] **Step 5: Rewrite `main`'s shutdown**

Replace the block after `<-ctx.Done()` in `cmd/aicrme/main.go`:

```go
	<-ctx.Done()
	slog.Info("shutting down")

	// Drain first: cancelling the run lands it in StateFailed, which isLive
	// does not consider live, so an unguarded POST /api/runs during the wait
	// below would start a run that shutdown then kills mid-flight.
	srv.Drain()

	// HTTP drain and engine cleanup run concurrently. The invariant is "do
	// not return before the deploy.sh process tree is reaped" -- not "do not
	// begin HTTP shutdown first". aicrme is PID 1 under the image's
	// ENTRYPOINT with no init, so returning from main tears down the whole
	// PID namespace and SIGKILLs helm before deploy.sh's INT/TERM trap can
	// run, which is what strands a release in pending-install.
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		httpCtx, cancelHTTP := context.WithTimeout(context.Background(), httpShutdownTimeout)
		defer cancelHTTP()
		_ = httpSrv.Shutdown(httpCtx)
	}()

	go func() {
		defer wg.Done()
		engCtx, cancelEng := context.WithTimeout(context.Background(), runShutdownTimeout)
		defer cancelEng()
		if err := eng.CancelAndWait(engCtx); err != nil {
			slog.Error("in-flight run did not stop cleanly", "error", err)
		}
	}()

	wg.Wait()
```

with constants near the top:

```go
// runShutdownTimeout bounds how long shutdown waits for an in-flight run to
// stop. It must exceed the applier's own killGrace (10s) so the process-group
// SIGTERM -> SIGKILL escalation can complete; see internal/applier/exec.go.
const runShutdownTimeout = 15 * time.Second

// httpShutdownTimeout bounds the HTTP drain. Runs concurrently with the
// above, so the pod's total shutdown budget is the larger of the two, not
// their sum -- which is what lets both fit inside
// terminationGracePeriodSeconds.
const httpShutdownTimeout = 10 * time.Second
```

Add `"sync"` to the imports.

- [ ] **Step 6: Set the grace period in the chart**

`terminationGracePeriodSeconds` defaults to 30s, after which the kubelet SIGKILLs regardless. Make it explicit rather than relying on an unstated default.

In `charts/aicrme/values.yaml`:

```yaml
# Shutdown budget. cmd/aicrme drains HTTP and stops any in-flight run
# concurrently, bounded by runShutdownTimeout (15s) -- which itself must
# exceed the applier's 10s process-group kill grace. 45s leaves margin over
# that 15s worst case; the Kubernetes default of 30s would work today but
# leaves nothing if either budget grows.
terminationGracePeriodSeconds: 45
```

In `charts/aicrme/templates/deployment.yaml`, add to the pod spec (sibling of `serviceAccountName`):

```yaml
      terminationGracePeriodSeconds: {{ .Values.terminationGracePeriodSeconds }}
```

- [ ] **Step 7: Assert it in the chart contract**

Extend `test/chart/contract.sh` using its existing vocabulary (`render`, `doc KIND`, `val`, `pass`, `fail`, `# --- Invariant N ---` banners — read the file first). Assert the rendered Deployment carries `terminationGracePeriodSeconds: 45`, and that `--set terminationGracePeriodSeconds=60` flows through. A budget that only works against an unstated default is what breaks when someone edits the Deployment for an unrelated reason.

- [ ] **Step 8: Verify and commit**

```bash
make qualify
git add internal/api/ cmd/aicrme/ charts/aicrme/ test/chart/contract.sh
git commit -S -m "feat: drain mutations, then shut down HTTP and the engine concurrently

Cancelling the run lands it in StateFailed, which isLive does not treat
as live -- so a POST /api/runs during the shutdown wait would start a
fresh run that shutdown then kills. Draining first closes that window;
safe methods keep serving so a connected browser watches the timeline
through shutdown.

The invariant is 'do not return before the process tree is reaped', not
'do not begin HTTP shutdown first'. Running both concurrently means the
pod's budget is the larger of the two rather than their sum, which is
what lets 15s + 10s fit inside a grace period at all --
terminationGracePeriodSeconds is now set explicitly at 45s and pinned by
the chart contract rather than inherited from an unstated 30s default."
```

---

## Task 3: Observer core and the DaemonSet signal

**Files:**
- Create: `internal/observer/observer.go`
- Create: `internal/observer/handlers.go`
- Create: `internal/observer/observer_test.go`

**Interfaces:**
- Consumes: `internal/bus`.
- Produces:
  - `type RunScope struct { RunID string; Namespaces map[string]struct{} }`
  - `func New(client kubernetes.Interface, b *bus.Bus, scope func() RunScope) *Observer`
  - `func (o *Observer) Start(stopCh <-chan struct{}) error`
  — all consumed by Task 6.

- [ ] **Step 1: Write the failing tests**

Create `internal/observer/observer_test.go`. These use `k8s.io/client-go/kubernetes/fake`, whose `Tracker` drives the informers:

```go
package observer_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/observer"
)

// collect drains the bus into a slice until deadline, so a test can assert
// both what was published and that nothing was.
func collect(t *testing.T, b *bus.Bus, within time.Duration) []bus.Event {
	t.Helper()
	sub, unsub := b.Subscribe(0)
	t.Cleanup(unsub)
	var (
		mu   sync.Mutex
		out  []bus.Event
		done = time.After(within)
	)
	for {
		select {
		case e := <-sub:
			mu.Lock()
			out = append(out, e)
			mu.Unlock()
		case <-done:
			mu.Lock()
			defer mu.Unlock()
			return out
		}
	}
}

func scopeFor(ns ...string) func() observer.RunScope {
	set := make(map[string]struct{}, len(ns))
	for _, n := range ns {
		set[n] = struct{}{}
	}
	return func() observer.RunScope { return observer.RunScope{RunID: "run-1", Namespaces: set} }
}

func daemonSet(ns, name string, ready, desired int32) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: "ds-uid"},
		Status: appsv1.DaemonSetStatus{
			NumberReady:            ready,
			DesiredNumberScheduled: desired,
		},
	}
}

func TestDaemonSetRolloutProgressIsNarrated(t *testing.T) {
	client := fake.NewSimpleClientset(daemonSet("gpu-operator", "nvidia-driver-daemonset", 0, 8))
	b := bus.New(256)
	o := observer.New(client, b, scopeFor("gpu-operator"))

	stop := make(chan struct{})
	defer close(stop)
	if err := o.Start(stop); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	ds := daemonSet("gpu-operator", "nvidia-driver-daemonset", 2, 8)
	if _, err := client.AppsV1().DaemonSets("gpu-operator").Update(
		context.Background(), ds, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	events := collect(t, b, 2*time.Second)
	var found bool
	for _, e := range events {
		if strings.Contains(e.Message, "nvidia-driver-daemonset 2/8 nodes ready") {
			found = true
			if e.Kind != bus.KindCluster {
				t.Errorf("Kind = %q, want %q", e.Kind, bus.KindCluster)
			}
			if e.RunID != "run-1" {
				t.Errorf("RunID = %q, want run-1", e.RunID)
			}
			if !strings.Contains(e.Message, "gpu-operator/") {
				t.Errorf("Message = %q, want it namespace-qualified", e.Message)
			}
		}
	}
	if !found {
		t.Fatalf("no rollout event published; got %d events", len(events))
	}
}

// The property the whole design rests on. An update that changes nothing the
// observer reports must publish nothing -- informer UpdateFunc fires on
// managedFields and annotation churn, and the bus drops subscribers 256
// events behind.
func TestUnchangedStateEmitsNothing(t *testing.T) {
	initial := daemonSet("gpu-operator", "nvidia-driver-daemonset", 2, 8)
	client := fake.NewSimpleClientset(initial)
	b := bus.New(256)
	o := observer.New(client, b, scopeFor("gpu-operator"))

	stop := make(chan struct{})
	defer close(stop)
	if err := o.Start(stop); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	noisy := daemonSet("gpu-operator", "nvidia-driver-daemonset", 2, 8)
	noisy.Annotations = map[string]string{"kubectl.kubernetes.io/restartedAt": "now"}
	if _, err := client.AppsV1().DaemonSets("gpu-operator").Update(
		context.Background(), noisy, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	if events := collect(t, b, time.Second); len(events) != 0 {
		t.Fatalf("published %d events for an unchanged rollout, want 0: %+v", len(events), events)
	}
}

// The informer's initial list delivers every existing object as an Add.
// Emitting there would narrate the cluster's entire pre-existing state at
// pod start.
func TestInitialListEmitsNothing(t *testing.T) {
	client := fake.NewSimpleClientset(daemonSet("gpu-operator", "nvidia-driver-daemonset", 8, 8))
	b := bus.New(256)
	o := observer.New(client, b, scopeFor("gpu-operator"))

	stop := make(chan struct{})
	defer close(stop)
	if err := o.Start(stop); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if events := collect(t, b, time.Second); len(events) != 0 {
		t.Fatalf("published %d events for the initial list, want 0: %+v", len(events), events)
	}
}

func TestWorkloadsOutsideTheRunScopeAreIgnored(t *testing.T) {
	client := fake.NewSimpleClientset(daemonSet("kube-system", "some-other-ds", 0, 3))
	b := bus.New(256)
	o := observer.New(client, b, scopeFor("gpu-operator"))

	stop := make(chan struct{})
	defer close(stop)
	if err := o.Start(stop); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	ds := daemonSet("kube-system", "some-other-ds", 1, 3)
	if _, err := client.AppsV1().DaemonSets("kube-system").Update(
		context.Background(), ds, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	if events := collect(t, b, time.Second); len(events) != 0 {
		t.Fatalf("published %d events for an out-of-scope namespace, want 0", len(events))
	}
}

func TestNilClientYieldsANoOpObserver(t *testing.T) {
	o := observer.New(nil, bus.New(8), scopeFor())
	stop := make(chan struct{})
	defer close(stop)
	if err := o.Start(stop); err != nil {
		t.Fatalf("Start() with nil client error = %v, want nil (degrade, not fail)", err)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/observer/ -race -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement the core**

Create `internal/observer/observer.go`:

```go
// Package observer converts Kubernetes cluster state changes into typed
// console events, so a long Apply is visible between deploy.sh's
// component-boundary markers rather than silent for minutes at a time.
//
// It aggregates; it never relays. An informer's UpdateFunc fires on any
// field change -- managedFields, annotations, status heartbeats -- and the
// bus drops live events for any subscriber more than 256 behind
// (internal/bus.subscriberBuffer). So each handler computes a small
// normalized state and publishes only when that state changes.
package observer

import (
	"log/slog"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/mchmarny/aicrme/internal/bus"
)

// resyncPeriod is 0 deliberately: periodic resync would re-deliver every
// object as an update, and while change-detection would drop those, doing
// the work at all is pointless when informers already watch.
const resyncPeriod = 0

// RunScope is the engine state the observer needs, taken as one atomic
// snapshot. Reading the run ID and the namespaces separately would let
// attribution and filtering come from different runs across a race.
type RunScope struct {
	RunID string
	// Namespaces are the resolved recipe's namespaces. Empty means no run
	// has resolved one yet, and namespaced workloads are filtered out
	// entirely -- Nodes are cluster-scoped and always pass.
	Namespaces map[string]struct{}
}

type stateKey struct {
	kind      string
	namespace string
	name      string
	uid       types.UID
}

// Observer watches a small set of resources and narrates changes.
type Observer struct {
	client kubernetes.Interface
	bus    *bus.Bus
	scope  func() RunScope

	mu sync.Mutex
	// workload holds DaemonSet/Deployment readiness summaries.
	workload map[stateKey]string
	// gpuQty holds Node nvidia.com/gpu allocatable. Kept separate and
	// compared with Quantity.Cmp rather than string equality, since 8,
	// 8000m and "8" are the same quantity with different serializations.
	gpuQty map[stateKey]resource.Quantity
}

// New returns an Observer. A nil client yields a no-op: the console's whole
// Discover-to-Apply arc works without cluster telemetry, so failing to build
// a client must degrade rather than prevent startup.
func New(client kubernetes.Interface, b *bus.Bus, scope func() RunScope) *Observer {
	return &Observer{
		client:   client,
		bus:      b,
		scope:    scope,
		workload: make(map[stateKey]string),
		gpuQty:   make(map[stateKey]resource.Quantity),
	}
}

// Start registers handlers and starts the informers. It returns once caches
// have synced (or the stop channel closes); handlers then run until stopCh.
func (o *Observer) Start(stopCh <-chan struct{}) error {
	if o.client == nil {
		slog.Warn("observer disabled: no Kubernetes client available")
		return nil
	}

	factory := informers.NewSharedInformerFactory(o.client, resyncPeriod)

	dsInf := factory.Apps().V1().DaemonSets().Informer()
	if err := o.register(dsInf, o.onDaemonSet); err != nil {
		return err
	}

	factory.Start(stopCh)
	for typ, ok := range factory.WaitForCacheSync(stopCh) {
		if !ok {
			slog.Warn("observer cache did not sync", "type", typ.String())
		}
	}
	return nil
}

// register wires one informer's handlers and its watch-error handler. A
// silently-dead informer is worse than no observer: the timeline simply
// stops with no indication that it has.
func (o *Observer) register(inf cache.SharedIndexInformer, onUpdate func(any)) error {
	if err := inf.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		slog.Warn("observer watch failed", "error", err)
	}); err != nil {
		return err
	}
	_, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    o.onAdd,
		UpdateFunc: func(_, newObj any) { onUpdate(newObj) },
		DeleteFunc: o.onDelete,
	})
	return err
}

func (o *Observer) publish(ns, msg string) {
	sc := o.scope()
	if ns != "" {
		if _, ok := sc.Namespaces[ns]; !ok {
			return
		}
	}
	o.bus.Publish(bus.Event{
		RunID:   sc.RunID,
		Kind:    bus.KindCluster,
		Level:   bus.LevelInfo,
		At:      time.Now().UTC(),
		Message: msg,
	})
}
```

Note `SetWatchErrorHandler` must be called **before** `factory.Start`; client-go rejects it afterwards.

- [ ] **Step 4: Implement the DaemonSet handler and cache semantics**

Create `internal/observer/handlers.go`:

```go
package observer

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/client-go/tools/cache"
)

// onAdd records state without emitting. An informer's initial list delivers
// every existing object as an Add, so emitting here would narrate the
// cluster's entire pre-existing state at pod start -- and would report a
// node that already has 8 GPUs as "0 -> 8", which is false.
func (o *Observer) onAdd(obj any) {
	switch t := obj.(type) {
	case *appsv1.DaemonSet:
		o.mu.Lock()
		o.workload[dsKey(t)] = dsSummary(t)
		o.mu.Unlock()
	}
}

// onDelete drops the cache entry so a delete-then-recreate of the same name
// does not inherit the old object's state. DeletedFinalStateUnknown is the
// tombstone client-go delivers when a watch gap meant the final object was
// missed.
func (o *Observer) onDelete(obj any) {
	if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tomb.Obj
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	switch t := obj.(type) {
	case *appsv1.DaemonSet:
		delete(o.workload, dsKey(t))
	}
}

func dsKey(ds *appsv1.DaemonSet) stateKey {
	return stateKey{kind: "DaemonSet", namespace: ds.Namespace, name: ds.Name, uid: ds.UID}
}

func dsSummary(ds *appsv1.DaemonSet) string {
	return fmt.Sprintf("%d/%d nodes ready", ds.Status.NumberReady, ds.Status.DesiredNumberScheduled)
}

func (o *Observer) onDaemonSet(obj any) {
	ds, ok := obj.(*appsv1.DaemonSet)
	if !ok {
		return
	}
	summary := dsSummary(ds)
	key := dsKey(ds)

	o.mu.Lock()
	prev, had := o.workload[key]
	if had && prev == summary {
		o.mu.Unlock()
		return
	}
	o.workload[key] = summary
	o.mu.Unlock()

	// Namespace-qualified: two DaemonSets in different namespaces can share
	// a name, and an unqualified message would be ambiguous.
	o.publish(ds.Namespace, fmt.Sprintf("%s/%s %s", ds.Namespace, ds.Name, summary))
}
```

- [ ] **Step 5: Run to verify they pass**

Run: `go test ./internal/observer/ -race -count=1 -v`
Expected: PASS.

- [ ] **Step 6: Bite-proof the dedup**

Change `onDaemonSet` to publish unconditionally (delete the `prev == summary` early return, keeping the cache write so it compiles). Re-run with `-v`. `TestUnchangedStateEmitsNothing` and `TestInitialListEmitsNothing` must FAIL while `TestDaemonSetRolloutProgressIsNarrated` still passes — that asymmetry is what proves the dedup tests cover ground the happy-path test cannot. Restore, confirm green. Record the output.

- [ ] **Step 7: Qualify and commit**

```bash
make qualify
git add internal/observer/
git commit -S -m "feat(observer): core, run scope, and the DaemonSet rollout signal

Narrates the gap deploy.sh cannot: between the marker for a component
starting and the marker for it finishing there can be minutes of silence
while the driver DaemonSet compiles a kernel module per node.

Aggregates rather than relays -- emits only when a computed summary
changes, because UpdateFunc fires on managedFields churn and the bus
drops subscribers 256 events behind. Add records without emitting, since
the informer's initial list would otherwise narrate the cluster's entire
pre-existing state at startup. RunScope is taken as one atomic snapshot
so attribution and namespace filtering cannot come from different runs."
```

---

## Task 4: Deployment and Node signals

**Files:**
- Modify: `internal/observer/handlers.go`
- Modify: `internal/observer/observer.go` (register two more informers)
- Modify: `internal/observer/observer_test.go`

**Interfaces:**
- Consumes: Task 3's `stateKey`, `publish`, `register`, and the two caches.
- Produces: no new exported surface.

### The two traps

**Deployment:** `status.replicas` is the count of pods currently created, **not** the desired count. During a scale-up it reports `1/1 ready` while `spec.replicas` is 8 — a "done" message during the exact stall this exists to narrate. Use `spec.replicas`, and suppress emission entirely while `status.observedGeneration < metadata.generation`, because before that the status describes the *previous* spec.

**Node:** the message is a transition (`0 → 8`), so it cannot itself be the cache value — a repeated identical update would compute `8 → 8`, compare unequal to the cached `0 → 8`, and emit again, meaning the dedup silently does nothing on the signal that matters most. Cache the `resource.Quantity`; compare with `Cmp`; format the delta at emit time from the cached previous value.

- [ ] **Step 1: Write the failing tests**

Add to `internal/observer/observer_test.go`. Include helpers `deployment(ns, name string, ready, desired int32, gen, observedGen int64)` and `node(name string, gpus string)` mirroring the `daemonSet` helper's shape, then:

- `TestDeploymentUsesSpecReplicasAsDenominator` — a Deployment with `status.readyReplicas=1`, `status.replicas=1`, `spec.replicas=8` must publish `1/8 ready`, never `1/1 ready`.
- `TestDeploymentStaleStatusIsSuppressed` — `observedGeneration=1`, `generation=2` publishes nothing.
- `TestNodeGPUAllocatableTransitionIsNarrated` — allocatable moving `0` → `8` publishes `nvidia.com/gpu allocatable 0 → 8`, qualified with the node name.
- `TestNodeRepeatedIdenticalAllocatableEmitsOnce` — apply the same `8` twice after the initial `0`; exactly one event. This is the bug caching the message would introduce.
- `TestNodeEquivalentQuantitySerializationsAreOneState` — `8` then `8000m` publishes nothing the second time, proving `Cmp` rather than string equality.
- `TestNodeAlreadyAtCapacityIsNotNarratedAsZeroToEight` — a node present at `8` in the initial list, then updated to `8`, publishes nothing.
- `TestDeleteThenRecreateDoesNotInheritState` — delete a DaemonSet and re-add it with different readiness; the recreate's Add must not emit, and the following update must be compared against the *new* baseline.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/observer/ -race -run 'TestDeployment|TestNode|TestDeleteThen' -v`
Expected: FAIL — helpers/handlers undefined.

- [ ] **Step 3: Implement the Deployment handler**

Add to `handlers.go`:

```go
func deployKey(d *appsv1.Deployment) stateKey {
	return stateKey{kind: "Deployment", namespace: d.Namespace, name: d.Name, uid: d.UID}
}

// deploySummary reports readiness against spec.replicas, not status.replicas.
// status.replicas is the number of pods that currently exist, so a scale-up
// in progress would read "1/1 ready" while eight are desired -- a "finished"
// message during precisely the stall this observer exists to narrate.
func deploySummary(d *appsv1.Deployment) string {
	desired := int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	return fmt.Sprintf("%d/%d ready", d.Status.ReadyReplicas, desired)
}

func (o *Observer) onDeployment(obj any) {
	d, ok := obj.(*appsv1.Deployment)
	if !ok {
		return
	}
	// Before the controller observes the current generation, status
	// describes the PREVIOUS spec and is actively misleading.
	if d.Status.ObservedGeneration < d.Generation {
		return
	}
	summary := deploySummary(d)
	key := deployKey(d)

	o.mu.Lock()
	prev, had := o.workload[key]
	if had && prev == summary {
		o.mu.Unlock()
		return
	}
	o.workload[key] = summary
	o.mu.Unlock()

	o.publish(d.Namespace, fmt.Sprintf("%s/%s %s", d.Namespace, d.Name, summary))
}
```

Extend `onAdd` and `onDelete` with a `*appsv1.Deployment` case using `deployKey`/`deploySummary`.

- [ ] **Step 4: Implement the Node handler**

```go
// gpuResource is the allocatable resource this product cares about. A bare
// allocatable diff would also fire on cpu/memory churn on every node.
const gpuResource = "nvidia.com/gpu"

func nodeKey(n *corev1.Node) stateKey {
	return stateKey{kind: "Node", name: n.Name, uid: n.UID}
}

func nodeGPUs(n *corev1.Node) resource.Quantity {
	if q, ok := n.Status.Allocatable[gpuResource]; ok {
		return q
	}
	return *resource.NewQuantity(0, resource.DecimalSI)
}

func (o *Observer) onNode(obj any) {
	n, ok := obj.(*corev1.Node)
	if !ok {
		return
	}
	cur := nodeGPUs(n)
	key := nodeKey(n)

	o.mu.Lock()
	prev, had := o.gpuQty[key]
	// Cmp, not string equality: 8, 8000m and "8" are the same quantity with
	// different serializations, and an informer round trip can change which
	// one you get.
	if had && prev.Cmp(cur) == 0 {
		o.mu.Unlock()
		return
	}
	o.gpuQty[key] = cur
	o.mu.Unlock()

	if !had {
		// No prior value: this is the first sighting, not a transition.
		// Narrating it as "0 -> 8" would be false for a node that already
		// had capacity when the console started.
		return
	}
	// The message is a TRANSITION, formatted here from the cached previous
	// value -- it is deliberately not what gets cached, because a repeated
	// identical update would then compute "8 -> 8", compare unequal to
	// "0 -> 8", and emit again.
	o.publish("", fmt.Sprintf("%s: %s allocatable %s → %s",
		n.Name, gpuResource, prev.String(), cur.String()))
}
```

Extend `onAdd`/`onDelete` with a `*corev1.Node` case writing/removing `o.gpuQty`. Add `corev1 "k8s.io/api/core/v1"` and `"k8s.io/apimachinery/pkg/api/resource"` imports.

- [ ] **Step 5: Register the two informers**

In `Start`, after the DaemonSet registration:

```go
	deployInf := factory.Apps().V1().Deployments().Informer()
	if err := o.register(deployInf, o.onDeployment); err != nil {
		return err
	}

	nodeInf := factory.Core().V1().Nodes().Informer()
	if err := o.register(nodeInf, o.onNode); err != nil {
		return err
	}
```

- [ ] **Step 6: Run to verify they pass**

Run: `go test ./internal/observer/ -race -count=1 -v`
Expected: PASS.

- [ ] **Step 7: Bite-proof the Node dedup**

Change the Node cache to store the formatted transition message instead of the `Quantity` (the bug the spec calls out): keep a `map[stateKey]string`, compare strings. `TestNodeRepeatedIdenticalAllocatableEmitsOnce` must FAIL while the plain transition test still passes. Restore, confirm green, record the output with `-v`.

Then separately change `prev.Cmp(cur) == 0` to `prev.String() == cur.String()` and confirm `TestNodeEquivalentQuantitySerializationsAreOneState` still passes — if it does, that test is not actually proving `Cmp` is needed, and you should strengthen it (e.g. a serialization pair where `String()` genuinely differs) or say so in your report.

- [ ] **Step 8: Qualify and commit**

```bash
make qualify
git add internal/observer/
git commit -S -m "feat(observer): Deployment readiness and Node GPU-capacity signals

Deployment readiness reports against spec.replicas, not status.replicas
-- the latter counts pods that currently exist, so a scale-up in progress
reads '1/1 ready' during exactly the stall this narrates. Stale status is
suppressed until observedGeneration catches up with generation.

The Node signal caches the quantity, not the message. The message is a
transition, so caching it would make a repeated identical update compute
'8 -> 8', compare unequal to '0 -> 8', and emit again -- the dedup
silently doing nothing on the signal that matters most. Quantities are
compared with Cmp, since 8 and 8000m are one state."
```

---

## Task 5: Wire the observer, tidy the module, measure the image

**Files:**
- Modify: `cmd/aicrme/main.go`
- Modify: `cmd/aicrme/main_test.go`
- Modify: `go.mod`

**Interfaces:**
- Consumes: `observer.New`, `observer.Start`, `observer.RunScope` (Tasks 3-4); `Engine.CurrentID`, `Engine.Current` (Task 1).
- Produces: nothing downstream.

- [ ] **Step 1: Write the failing test for the scope accessor**

The scope closure is the one piece with real logic: it must return the current run's ID and its recipe's namespaces, **without calling `Current()` on every invocation**, because `Current()` deep-copies every artifact.

Add to `cmd/aicrme/main_test.go` (package `main`) a test for a small extracted helper:

```go
func TestRecipeNamespacesFromArtifact(t *testing.T) {
	raw := []byte(`{"name":"r","version":"1","componentCount":2,"components":[
		{"name":"a","namespace":"gpu-operator"},
		{"name":"b","namespace":"monitoring"}]}`)

	got := recipeNamespaces(raw)
	for _, want := range []string{"gpu-operator", "monitoring"} {
		if _, ok := got[want]; !ok {
			t.Errorf("namespace %q missing from %v", want, got)
		}
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
}

func TestRecipeNamespacesToleratesMissingOrCorruptArtifact(t *testing.T) {
	if got := recipeNamespaces(nil); len(got) != 0 {
		t.Errorf("nil artifact = %v, want empty", got)
	}
	if got := recipeNamespaces([]byte("not json")); len(got) != 0 {
		t.Errorf("corrupt artifact = %v, want empty", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/aicrme/ -race -run TestRecipeNamespaces -v`
Expected: FAIL — `undefined: recipeNamespaces`.

- [ ] **Step 3: Implement the helper and the cached scope accessor**

In `cmd/aicrme/main.go`:

```go
// recipeNamespaces extracts the namespaces the resolved recipe installs
// into. A missing or unparseable artifact yields an empty set, which the
// observer treats as "filter every namespaced workload out" -- the
// fail-quiet direction, since narrating unrelated cluster activity is worse
// than narrating nothing.
func recipeNamespaces(raw []byte) map[string]struct{} {
	out := map[string]struct{}{}
	if len(raw) == 0 {
		return out
	}
	var summary steps.RecipeSummary
	if err := json.Unmarshal(raw, &summary); err != nil {
		slog.Warn("recipe.json unparseable; observer will not narrate workloads", "error", err)
		return out
	}
	for _, c := range summary.Components {
		if c.Namespace != "" {
			out[c.Namespace] = struct{}{}
		}
	}
	return out
}

// newRunScopeFn returns an accessor the observer calls on every watch event.
// It caches by run ID and refreshes only when that changes: Engine.Current()
// deep-copies every artifact including the raw snapshot (tens of KB), so
// calling it per event would copy megabytes per second to obtain a string.
// CurrentID reads the ID under the same lock without cloning.
func newRunScopeFn(eng *engine.Engine) func() observer.RunScope {
	var (
		mu     sync.Mutex
		cached observer.RunScope
	)
	return func() observer.RunScope {
		id, ok := eng.CurrentID()
		if !ok {
			return observer.RunScope{}
		}
		mu.Lock()
		defer mu.Unlock()
		if cached.RunID == id {
			return cached
		}
		cached = observer.RunScope{RunID: id}
		if run := eng.Current(); run != nil && run.ID == id {
			cached.Namespaces = recipeNamespaces(run.Artifacts["recipe.json"])
		}
		return cached
	}
}
```

**Note the staleness this accepts:** the scope is refreshed only when the run ID changes, so within one run the namespace set is whatever `recipe.json` held the first time the observer asked. That is correct here — `recipe.json` is written once by Recommend and never mutated — but say so in a comment, because it would be wrong if a later phase made artifacts mutable.

- [ ] **Step 4: Build the client and start the observer**

In `main()`, after the engine is constructed and before the HTTP server starts:

```go
	// A failure here must not stop the console: the entire Discover-to-Apply
	// arc works without cluster telemetry, and `make build && ./bin/aicrme`
	// outside a cluster is a supported development path. Same degrade-with-a-
	// warning posture as parseNodeSelector.
	var kube kubernetes.Interface
	if cfg, err := rest.InClusterConfig(); err != nil {
		slog.Warn("no in-cluster config; live cluster telemetry disabled", "error", err)
	} else if c, err := kubernetes.NewForConfig(cfg); err != nil {
		slog.Warn("kubernetes client init failed; live cluster telemetry disabled", "error", err)
	} else {
		kube = c
	}

	obsStop := make(chan struct{})
	defer close(obsStop)
	if err := observer.New(kube, b, newRunScopeFn(eng)).Start(obsStop); err != nil {
		slog.Warn("observer failed to start; continuing without cluster telemetry", "error", err)
	}
```

- [ ] **Step 5: Run to verify**

Run: `go test ./cmd/aicrme/ -race -count=1`
Expected: PASS.

- [ ] **Step 6: Tidy the module and verify no version drift**

`k8s.io/client-go` is now imported directly but still marked `// indirect`.

```bash
cp go.mod /tmp/go.mod.before
go mod tidy
diff /tmp/go.mod.before go.mod
```

The diff must contain **only** `// indirect` marker removals — `k8s.io/client-go`, `k8s.io/api`, `k8s.io/apimachinery`, and `gopkg.in/yaml.v3` (the last a pre-existing mismarking recorded in the handoff's deferred list, which this incidentally closes). **If any version number changes, stop and report it** — `aicr` must remain `v0.19.0`.

Then confirm: `make check-aicr-pin`.

- [ ] **Step 7: Measure the image**

The handoff records ~55 MB compressed and says to watch it. Linking client-go's informer and typed-client machinery is a plausible step change.

```bash
git stash            # measure the pre-observer image
make image IMAGE=aicrme:before
git stash pop
make image IMAGE=aicrme:after
docker images --format '{{.Repository}}:{{.Tag}} {{.Size}}' | grep aicrme
```

Record both numbers and the delta in your report. This is a measurement, not a gate — but an unmeasured step change in the first thing a cluster pulls is exactly what the handoff asks someone to notice.

- [ ] **Step 8: Qualify and commit**

```bash
make qualify
git add cmd/aicrme/ go.mod go.sum
git commit -S -m "feat: wire the observer, tidy the module

The scope accessor caches by run ID and refreshes only when that changes:
Engine.Current() deep-copies every artifact including the raw snapshot,
so calling it per watch event would copy megabytes per second to obtain a
string. CurrentID reads the ID under the same lock without cloning.

Client construction degrades with a warning rather than failing startup
-- the whole Discover-to-Apply arc works without telemetry, and running
the binary outside a cluster is a supported development path.

go mod tidy corrects the // indirect markers now that client-go is
imported directly, and incidentally closes the pre-existing yaml.v3
mismarking the handoff carried as a deferred finding."
```

---

## Task 6: Update the handoff

**Files:**
- Modify: `docs/phase-2-handoff.md`

- [ ] **Step 1: Record what 2b-i closed**

Move from "Constraints 2b inherits" into a "Resolved in 2b-i" section: the cancellation gap (`internal/applier`'s process-group machinery is now reachable in production), and the observer's non-existence. State plainly that `Engine.CancelAndWait` is what reaches it and that shutdown drains before running HTTP and engine cleanup concurrently.

Remove the `yaml.v3 // indirect` item from the deferred list, noting Task 5's `go mod tidy` closed it.

- [ ] **Step 2: Record what 2b-i deliberately did not do**

Add to "Constraints 2b-ii inherits": the ConfigMap store, the `bus.nextID` epoch, and — new — **the per-component live sub-status**. `approach.md` §Apply(cockpit) promises `waiting on rollout: nvidia-driver-daemonset 3/8 ready` attached to the active component's row; 2b-i is timeline-only. Record the reason it was deferred (namespace is the only reliable join key between a workload and the component whose install created it, and it is not one-to-one) and that the events are already in the bus, so a later task can correlate without changing the observer.

- [ ] **Step 3: Record the measurements**

The image size before and after (Task 5 Step 7), and the observer's open calibration questions: whether change-detection alone bounds volume on a real 8-node rollout, and whether MIG resource names need the same treatment as `nvidia.com/gpu`. Both are Phase 4 measurements.

- [ ] **Step 4: Commit**

```bash
make qualify
git add docs/phase-2-handoff.md
git commit -S -m "docs: record Phase 2b-i in the handoff"
```

---

## Self-Review

**Spec coverage.** Every section of the spec maps to a task: §Cancellation's completion contract, terminal persistence, and `StateFailed` landing → Task 1; §Shutdown ordering and the grace budget → Task 2; §Observer signals, aggregation, handler semantics, attribution, filtering, health → Tasks 3-4; §"the Kubernetes client is new" (degradation, `go mod tidy`, image measurement) → Task 5; the timeline-only scope statement and open questions → Task 6. The spec's testing table is distributed across the task test steps, one row per assertion.

**Placeholder scan.** Task 4 Step 1 and Task 2 Step 1 describe test assertions rather than pasting full bodies. That is deliberate: `internal/observer`'s tests must reuse the helper shape Task 3 establishes (`daemonSet`, `collect`, `scopeFor`), and `internal/api`'s must adopt the existing `httptest` harness rather than build a parallel one. Each names the file to read and lists every required assertion. Everywhere a new type, handler, or non-obvious constant is introduced, literal code is present.

**Type consistency.** `RunScope{RunID, Namespaces}` is defined in Task 3 and consumed verbatim in Tasks 4-5. `stateKey` and the two caches (`workload map[stateKey]string`, `gpuQty map[stateKey]resource.Quantity`) are introduced in Task 3 and extended, not redefined, in Task 4. `Engine.CurrentID() (string, bool)` and `Engine.CancelAndWait(ctx) error` are defined in Task 1 and consumed in Tasks 2 and 5. `steps.RecipeSummary` is reused unchanged from Phase 2a for `recipeNamespaces`.

**One hazard worth naming.** Task 5's `newRunScopeFn` caches on run ID and therefore will not notice `recipe.json` changing *within* a run. That is safe today because Recommend writes it once, and the plan says so at the code site — but if a later phase makes artifacts mutable, this cache goes stale silently. If any step asks for a symbol no task defines, that is a plan defect: stop and raise it rather than inventing the symbol.

## Unresolved questions

1. **Does change-detection alone bound observer volume** on a real 8-node driver rollout? First honest measurement is Phase 4. The additive fix (per-object minimum interval) does not change this design.
2. **Does the image grow materially** from linking client-go's informer machinery? Task 5 measures it; the answer decides whether image size becomes a 2b-ii concern.
3. **`approach.md` Open Question 1 — ownership and budget.** Still untouched, still gating Phase 4, and the dry-run ceiling has made Phase 4 a hard dependency for full-chain validation rather than a preference.
