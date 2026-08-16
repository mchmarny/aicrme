# aicrme Phase 2b-i — Observer and cancellation

**Date:** 2026-08-16
**Status:** Approved for planning
**Spec:** `approach.md` (§Live feedback design, §Architecture units table)
**Inputs:** `docs/phase-2-handoff.md` — especially "Constraints 2b inherits" and Finding 1's restart-recovery contract list
**Pinned dependency:** `github.com/NVIDIA/aicr v0.19.0`

---

## Why this is 2b-i and not 2b

The handoff describes Phase 2b as four things: the observer, the ConfigMap-backed `engine.Store`, the `bus.nextID` epoch problem, and — added by an external reviewer — graceful in-flight Apply termination on SIGTERM.

Those are not one spec's worth, and they do not couple evenly. The handoff itself says the store and the bus epoch "activate together" and must be "designed together," while the observer touches neither and cancellation touches only `engine` and `main`.

**Phase 2b-i is the two independent units: the observer and cancellation.** The store and the bus epoch are 2b-ii.

The ordering is deliberate. Phase 4 is the only thing that can validate the full component chain (the dry-run ceiling is a structural proof of this, not a preference), and what 2b owes Phase 4 is a real 10-to-20-minute Apply that is *watchable* and *interruptible*. The observer supplies the first; cancellation supplies the second. Restart survival matters, but it buys robustness for a demo that cannot yet complete.

---

## Scope

### In

- `internal/observer`: shared informers over DaemonSets, Deployments, and Nodes, converting cluster state changes into typed `bus` events.
- `Engine.Cancel`, and `cmd/aicrme/main.go` wiring SIGINT/SIGTERM to it ahead of HTTP shutdown.

### Out — deliberately

Pod and Event informers; benign-warning classification; image-pull narration; the ConfigMap store; the `bus.nextID` epoch; `StateActive`; an HTTP cancel endpoint; any new `engine.State`.

All of `approach.md`'s standing non-goals continue to apply — air-gapped operation, day-2 operations, multi-user auth, multi-cluster, in-UI editing. The `cluster-admin` grant remains deliberate and disclosed.

---

## The observer

### What it is for

`approach.md` §Live feedback design is explicit that "the dynamic feel does not come from the applier. It comes from a second, independent stream." Concretely: on real hardware, `deploy.sh`'s marker stream only fires at component boundaries. Between `┌─ [7/14] gpu-operator` and `└─ ✓ gpu-operator installed` there can be **seven minutes of complete silence** while the driver DaemonSet compiles a kernel module on each node.

Phase 2a shipped the slow-step callouts, which *explain* that stall before it happens. The observer is what *fills* it. That is the whole justification for this unit, and it is why the observer is scoped to the two signals that actually cover that window rather than to everything the spec lists.

### Signals

Three informers, three summaries:

| Resource | Emitted summary | Source fields |
|---|---|---|
| DaemonSet | `nvidia-driver-daemonset 2/8 nodes ready` | `status.numberReady` / `status.desiredNumberScheduled` |
| Deployment | `gpu-operator 1/1 ready` | `status.readyReplicas` / `status.replicas` |
| Node | `ip-10-0-2-7: nvidia.com/gpu allocatable 0 → 8` | `status.allocatable["nvidia.com/gpu"]` |

The DaemonSet signal is the one that covers the driver-compile gap. The Node signal is the payoff — it is the live version of the number the Discover screen opened with ("0 of 64 GPUs are usable by a workload today").

### It aggregates; it never relays

**This is the load-bearing design property.** An informer's `UpdateFunc` fires on *any* field change — `managedFields`, annotations, status heartbeats, resourceVersion churn. Relaying those would flood a bus whose ring holds 20 000 events and which **drops live events for any subscriber more than 256 behind** (`internal/bus/bus.go`'s `subscriberBuffer`). A browser watching a real install would silently lose the timeline it exists to show.

So each handler computes its summary string and emits **only when that string differs from the last one emitted for that object**. Comparing the computed summary rather than the object is what bounds volume to genuine state transitions. Resync period is 0, so periodic re-delivery does not manufacture events either.

This mirrors, deliberately, the discipline the applier already follows: `internal/applier/parse.go` publishes only recognized markers and drops all other `deploy.sh` output for exactly the same reason.

### Attribution

Events carry `Kind: bus.KindCluster` — already declared in `internal/bus/event.go` as "observer-sourced cluster telemetry" and unused until now.

`RunID` matters more than it appears. `web/src/components/Wizard.tsx`'s `currentRunIdOf`/`deriveRunState` and `web/src/pipeline.ts` both filter events by run ID; an event with an empty `RunID` is excluded from run-state derivation. That exclusion is correct for the pipeline (observer events are not component markers and must not appear as pipeline rows) but wrong for the timeline, where they belong.

The observer therefore takes a `func() string` in its constructor, which `cmd/aicrme/main.go` wires to `eng.Current()`. A function rather than an `*engine.Engine` reference so `internal/observer` never imports `internal/engine` — the dependency would be backwards, and the `approach.md` units table has `observer` depending only on `kubernetes.Interface`.

### Namespace filtering happens at emit time, not watch time

Nodes are cluster-scoped. DaemonSets and Deployments are namespaced, and the namespaces of interest come from the resolved recipe — which does not exist when the pod starts.

Watching narrowly would mean recreating informers once a recipe lands. Instead the factory watches all namespaces (the `cluster-admin` grant already permits it) and each handler filters at emit time: a namespaced workload is emitted only if its namespace appears in the current run's recipe. Node events are always emitted.

This keeps informer lifecycle trivial — start once at pod start, stop at shutdown — which is what `approach.md`'s "from the moment the pod starts" asks for.

### The Kubernetes client is new to this codebase

Nothing in the repo constructs a `kubernetes.Interface` today; the AICR module handles its own cluster access internally. The observer is the first, which has two consequences.

**Degrade, don't fail.** `main` builds the client with `rest.InClusterConfig()`. If that fails — running the binary outside a cluster, as `make build` then `./bin/aicrme` does — it logs at warn and **runs without an observer**. The console's entire Discover → Apply arc works without it; refusing to start would trade a nice-to-have for a hard dependency. This mirrors how `parseNodeSelector` already degrades a malformed value to "no override" with a warning rather than crashing.

**`go.mod` markers become inaccurate.** `k8s.io/client-go v0.36.3` is currently marked `// indirect` because it arrives transitively through the aicr module. A direct import does not break the build — the comment is metadata — but it becomes wrong. This phase should run `go mod tidy` and commit the result, which corrects the `client-go` marker and incidentally closes the deferred finding that `gopkg.in/yaml.v3` is likewise mismarked despite `internal/steps/recommend.go` importing it directly. Verify no version changes in the diff; the pin is `v0.19.0` for aicr and must stay.

---

## Cancellation

### What exists and what is missing

The hard part is already built and proven. `internal/applier/exec.go` runs `deploy.sh` in its own process group (`SysProcAttr{Setpgid: true}`), sends `SIGTERM` to the **group** on context cancellation so `deploy.sh`'s own `trap 'rm -rf "${HELM_WORKDIR}"; exit 130' INT TERM` runs, and escalates to a group `SIGKILL` after `killGrace` via a watchdog racing a `reaped` channel. That mechanism has a real-grandchild test and an asymmetric bite-proof.

What is missing is that **nothing ever triggers it in production**. `Engine.Start` launches `go e.execute(context.WithoutCancel(ctx), epoch)` and `Retry` uses `context.Background()`; `Engine` exposes no `Cancel`; and `main`'s signal handler calls `httpSrv.Shutdown` and returns without touching the run. The handoff records this precisely: the machinery is "correct, well-tested, and currently unreachable in production."

### The wiring

`Start` and `Retry` derive a cancellable context from the detached one and store the cancel func alongside the run:

```go
ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
// stored under e.mu, alongside e.current and e.epoch
go e.execute(ctx, epoch)
```

`Engine.Cancel()` invokes it under the lock. The in-flight step's context dies, `BashExec`'s `cmd.Cancel` fires, and the existing group-signal path runs unchanged.

### Where a cancelled run lands

**`StateFailed`, with `"cancelled: console shutting down"`.** No new state.

This satisfies the handoff's contract item — "fail-closed plus explicit Retry rather than auto-resume on an interrupted Apply" — because `Retry` already resumes from the step cursor, and it requires exactly `StateFailed`.

A `StateCanceled` would read more honestly in a timeline, and was rejected. `StateActive` is already declared-and-unreachable and the handoff carries a paragraph explaining why nobody should delete it; adding a second speculative state before its consumer exists repeats that mistake. If Phase 3's Stop-workload control wants the distinction, it can introduce it with a real consumer.

`awaitDecisions` returning false on `ctx.Done()` currently leaves the run in `StateAwaitingDecision` and returns. It should also finish into `StateFailed`. Irrelevant with today's memory store — the process is exiting — but 2b-ii persists this, and a run frozen mid-gate with no goroutine is exactly the wedge class Ruling 13 fixed for `Save` failures.

### Shutdown ordering is the part that must not be got backwards

```
signal → eng.Cancel() → wait, bounded → httpSrv.Shutdown(ctx) → return
```

`main` currently does `httpSrv.Shutdown` and returns. With `ENTRYPOINT ["/usr/local/bin/aicrme"]` and no init process, aicrme is **PID 1**, so returning from `main` tears down the whole PID namespace. Shutting HTTP down first and returning would SIGKILL `helm` before `deploy.sh`'s trap ever ran — precisely the stranded-`pending-install` scenario the handoff documents, which the next `helm upgrade --install` refuses and `deploy.sh`'s preflight does not clean up.

The bound must exceed `killGrace` (currently 10s) so the escalation path can complete; ~15s is the target, with the existing 10s HTTP shutdown budget after it.

---

## Testing

| Unit | Approach |
|---|---|
| `observer` summaries | `client-go` fake clientset driving synthetic watch events, asserting emitted `bus.Event`s — the approach `approach.md`'s testing table already names |
| `observer` volume control | **An unchanged summary must emit nothing.** Feed an update that changes only `managedFields`/annotations and assert zero events. This is the property the whole design rests on; a test that only proves "a change emits an event" would pass against a relay that floods |
| `observer` attribution | `KindCluster`, and `RunID` taken from the injected accessor; namespaced workloads outside the recipe's namespaces emit nothing |
| `observer` degradation | A nil client yields a no-op observer rather than a panic |
| `Engine.Cancel` | A fake `Step` that blocks until its context dies; assert `StateFailed`, the error message, and that `Retry` still works afterwards |
| shutdown ordering | Assert `Cancel` is invoked before `Shutdown` — the ordering is the defect this exists to prevent, so it needs an assertion, not a comment |
| e2e | **No change.** `apply-dryrun.sh` fails at 3/14 before any rollout progresses, so the observer has nothing to show there. Real observer output first appears on Phase 4 hardware |

Standing constraints hold: 80% coverage floor, `-race`, `make qualify` green, signed commits.

---

## Open questions

1. **Node signal specificity.** `nvidia.com/gpu` is the resource that matters for this product, but a bare `allocatable` diff would also catch cpu/memory churn and be noisy. The design pins the GPU resource name explicitly; whether other accelerator resource names (e.g. `nvidia.com/mig-*`) deserve the same treatment is a Phase 4 calibration question, not a 2b-i one.
2. **Rate limiting beyond change-detection.** Change-only emission should bound volume adequately, but this is unverified against a real 8-node driver rollout — the first honest measurement is Phase 4. If it proves insufficient, a per-object minimum interval is the additive fix and does not change this design.
3. **Ownership and budget** — `approach.md` Open Question 1, still untouched, still gating Phase 4. The dry-run ceiling has since made Phase 4 a hard dependency for full-chain validation rather than a preference, which raises the cost of leaving this open.
