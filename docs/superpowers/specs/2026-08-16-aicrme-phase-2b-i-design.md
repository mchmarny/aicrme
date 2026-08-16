# aicrme Phase 2b-i — Observer and cancellation

**Date:** 2026-08-16
**Status:** Approved for planning (revised after external review)
**Spec:** `approach.md` (§Live feedback design, §Architecture units table, §Apply (cockpit))
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
- `Engine.CancelAndWait`, and `cmd/aicrme/main.go` draining and shutting down in an order that cannot strand a Helm release.

### Out — deliberately

Pod and Event informers; benign-warning classification; image-pull narration; the ConfigMap store; the `bus.nextID` epoch; `StateActive`; an HTTP cancel endpoint; any new `engine.State`.

**Also explicitly out: the per-component live sub-status.** `approach.md` §Apply(cockpit) asks for "for the active component a live sub-status sourced from the observer, for example `waiting on rollout: nvidia-driver-daemonset 3/8 ready`." **2b-i is timeline-only.** Observer events land in `bus` and render in the cockpit's Timeline rail; they are not correlated to the pipeline row of the component currently installing.

That correlation is a real design problem (mapping a workload to the component whose install triggered it, when the only reliable join key is namespace, which is not one-to-one) and it is not needed to deliver this unit's value: the Timeline is visible in the cockpit throughout Apply, so the driver-rollout signal fills the silent gap either way. The events will already be in the bus, so a later task can add the correlation without changing the observer. Declaring it out of scope rather than silently omitting it, because `approach.md` promises it and a reader comparing the two would otherwise assume it shipped.

All of `approach.md`'s standing non-goals continue to apply — air-gapped operation, day-2 operations, multi-user auth, multi-cluster, in-UI editing. The `cluster-admin` grant remains deliberate and disclosed.

---

## The observer

### What it is for

`approach.md` §Live feedback design is explicit that "the dynamic feel does not come from the applier. It comes from a second, independent stream." Concretely: on real hardware, `deploy.sh`'s marker stream only fires at component boundaries. Between `┌─ [7/14] gpu-operator` and `└─ ✓ gpu-operator installed` there can be **seven minutes of complete silence** while the driver DaemonSet compiles a kernel module on each node.

Phase 2a shipped the slow-step callouts, which *explain* that stall before it happens. The observer is what *fills* it. That is the whole justification for this unit, and it is why the observer is scoped to the two signals that actually cover that window rather than to everything the spec lists.

### Signals

Three informers, three summaries. Every message carries its namespace, because two Deployments in different namespaces can share a name and an unqualified message would be ambiguous:

| Resource | Emitted message | Source fields |
|---|---|---|
| DaemonSet | `gpu-operator/nvidia-driver-daemonset 2/8 nodes ready` | `status.numberReady` / `status.desiredNumberScheduled` |
| Deployment | `gpu-operator/gpu-operator-controller 1/2 ready` | `status.readyReplicas` / **`spec.replicas`** |
| Node | `ip-10-0-2-7: nvidia.com/gpu allocatable 0 → 8` | `status.allocatable["nvidia.com/gpu"]` |

**The Deployment denominator is `spec.replicas`, not `status.replicas`.** `status.replicas` is the count of pods currently created, so a scale-up in progress reports `1/1 ready` while the desired count is 8 — a message that says "done" during the exact stall the observer exists to narrate. For the same reason, a Deployment's status is **suppressed until `status.observedGeneration >= metadata.generation`**: before that, status describes the previous spec and is actively misleading.

### It aggregates; it never relays

**This is the load-bearing design property.** An informer's `UpdateFunc` fires on *any* field change — `managedFields`, annotations, status heartbeats, resourceVersion churn. Relaying those would flood a bus whose ring holds 20 000 events and which **drops live events for any subscriber more than 256 behind** (`internal/bus/bus.go`'s `subscriberBuffer`). A browser watching a real install would silently lose the timeline it exists to show.

So each handler computes a **normalized state** for the object and emits only when that state differs from the last one recorded for it. Resync period is 0, so periodic re-delivery does not manufacture events either.

This mirrors, deliberately, the discipline the applier already follows: `internal/applier/parse.go` publishes only recognized markers and drops all other `deploy.sh` output for exactly the same reason.

**Cache the state, not the message.** These are different things and conflating them is a real bug: the Node message is a *transition* (`0 → 8`), so if the message were the cache key, a second identical update would compute `8 → 8`, compare unequal to `0 → 8`, and emit again — the dedup would silently do nothing on precisely the signal that matters most. The cache holds the normalized value (for Nodes, the `resource.Quantity`, compared with `Quantity.Cmp` rather than string equality, since `8`, `8000m`, and `"8"` are equal quantities with different serializations); the transition message is formatted from the cached previous value and the new one at emit time.

### Handler semantics

The cache is keyed by `(kind, namespace, name, UID)` and guarded by a mutex — informer handlers run on their own goroutines, and a delete/recreate of the same name must not inherit the old object's state.

| Handler | Behavior |
|---|---|
| `AddFunc` | Record state. **Emit nothing.** An informer's initial list delivers every existing object as an Add, so emitting here would narrate the cluster's entire pre-existing state at pod start — and would report a node that already has 8 GPUs as `0 → 8`, which is false. |
| `UpdateFunc` | Compute state; emit only if it differs from the cached value; then update the cache. |
| `DeleteFunc` | Drop the cache entry, handling `DeletedFinalStateUnknown` (the tombstone client-go delivers when a watch gap means the final object was missed). Emit nothing. |

### Attribution

Events carry `Kind: bus.KindCluster` — already declared in `internal/bus/event.go` as "observer-sourced cluster telemetry" and unused until now.

**Why `RunID` matters, stated correctly.** `web/src/components/Timeline.tsx` renders every event it is given with no run filter, so an event with an empty `RunID` *would* appear in the timeline. What `RunID` actually buys is attribution: `web/src/pipeline.ts` and `Wizard.tsx`'s `deriveRunState` both filter by run ID, so a populated `RunID` keeps observer events correctly associated with the run they describe, and — because they are `KindCluster` rather than `KindComponent` — correctly *out* of the pipeline's component rows. It also disambiguates a page reload that replays two runs' events into one list.

**One atomic snapshot, injected.** The observer needs two things from the engine — the current run's ID, and the namespaces of its resolved recipe — and taking them from separate calls would let attribution and filtering come from different runs across a race. So the constructor accepts a single accessor returning both:

```go
type RunScope struct {
    RunID      string
    Namespaces map[string]struct{}   // nil/empty = filter nothing through
}

func New(client kubernetes.Interface, b *bus.Bus, scope func() RunScope) *Observer
```

`cmd/aicrme/main.go` supplies a closure that derives `RunScope` and **caches it**, refreshing only when the run ID changes. `Engine.Current()` deep-copies every artifact on every call — including `snapshot.yaml`, ~70 KB in the committed fixture and larger on a real cluster — so calling it per watch event would copy megabytes per second to obtain a string. A function rather than an `*engine.Engine` reference keeps `internal/observer` from importing `internal/engine`; the `approach.md` units table has `observer` depending only on `kubernetes.Interface`.

### Namespace filtering happens at emit time, not watch time

Nodes are cluster-scoped. DaemonSets and Deployments are namespaced, and the namespaces of interest come from the resolved recipe — which does not exist when the pod starts.

Watching narrowly would mean recreating informers once a recipe lands. Instead the factory watches all namespaces (the `cluster-admin` grant already permits it) and each handler filters at emit time against `RunScope.Namespaces`. Node events are always emitted. An empty namespace set — no run yet, or a run before Recommend — filters nothing through for namespaced kinds.

This keeps informer lifecycle trivial: start once at pod start, stop at shutdown.

### Observation health

Client construction succeeding does not mean observation stays healthy. The observer installs a **watch error handler** on each informer and logs at warn, and logs the result of `WaitForCacheSync`. A silently-dead informer is worse than no observer, because the timeline simply stops with no indication that it has.

### The Kubernetes client is new to this codebase

Nothing in the repo constructs a `kubernetes.Interface` today; the AICR module handles its own cluster access internally. The observer is the first, which has three consequences.

**Degrade, don't fail.** `main` builds the client with `rest.InClusterConfig()`. If that fails — running the binary outside a cluster, as `make build && ./bin/aicrme` does — it logs at warn and **runs without an observer**. The console's entire Discover → Apply arc works without it; refusing to start would trade a nice-to-have for a hard dependency. This mirrors how `parseNodeSelector` already degrades a malformed value to "no override" with a warning rather than crashing. A nil client must yield a no-op observer, not a panic.

**`go.mod` markers become inaccurate.** `k8s.io/client-go v0.36.3` is currently marked `// indirect` because it arrives transitively through the aicr module. A direct import does not break the build — the comment is metadata — but it becomes wrong. This phase runs `go mod tidy` and commits the result, which corrects the `client-go` marker and incidentally closes the deferred finding that `gopkg.in/yaml.v3` is likewise mismarked despite `internal/steps/recommend.go` importing it directly. **Verify no version changes appear in that diff**; `aicr` must stay at `v0.19.0`.

**Measure the image.** The handoff records the image at ~55 MB compressed and says to watch it. Linking `client-go`'s informer and typed-client machinery is a plausible step change even though the module is already in the graph. Measure before and after and record the delta; it is the first thing a cluster pulls.

---

## Cancellation

### What exists and what is missing

The hard part is already built and proven. `internal/applier/exec.go` runs `deploy.sh` in its own process group (`SysProcAttr{Setpgid: true}`), sends `SIGTERM` to the **group** on context cancellation so `deploy.sh`'s own `trap 'rm -rf "${HELM_WORKDIR}"; exit 130' INT TERM` runs, and escalates to a group `SIGKILL` after `killGrace` via a watchdog racing a `reaped` channel. That mechanism has a real-grandchild test and an asymmetric bite-proof.

What is missing is that **nothing ever triggers it in production**. `Engine.Start` launches `go e.execute(context.WithoutCancel(ctx), epoch)` and `Retry` uses `context.Background()`; `Engine` exposes no `Cancel`; and `main`'s signal handler calls `httpSrv.Shutdown` and returns without touching the run.

### The completion contract

Storing a cancel function is not enough. Cancelling tells the run to stop; it says nothing about *when it has stopped*, and the whole point is not to return from `main` until the process tree is reaped. So the engine exposes a waitable operation rather than a fire-and-forget one:

```go
// CancelAndWait cancels the in-flight run and blocks until its execute
// goroutine has exited and persisted a terminal state, or ctx expires.
// Idempotent: safe with no run in flight, and safe to call twice.
func (e *Engine) CancelAndWait(ctx context.Context) error
```

Internally, `Start` and `Retry` derive a cancellable context from the detached one and record both the cancel func and a `done chan struct{}` alongside the run and its epoch. `execute` closes `done` on exit — **after** terminal persistence has been attempted, not before, so a caller that returns when `done` closes knows the state was written, not merely that the goroutine is unwinding.

`CancelAndWait` invokes cancel, then selects on `done` and `ctx.Done()`. A timeout returns an error rather than blocking shutdown forever.

### Terminal state must persist even though the context is cancelled

`finish` currently saves with the same context the step ran under. Once that context is cancelled, a real store's API call fails immediately — so the run would be cancelled but persisted as `running`, which is exactly the wedge class the handoff's Ruling 13 fixed for `Save` failures.

Inert today (`memoryStore.Save` ignores its context), live the moment 2b-ii lands a ConfigMap store. **`finish` therefore persists under a bounded detached context** — `context.WithTimeout(context.WithoutCancel(ctx), …)` — so terminal state is written on a cancellation path, and `done` closes only after that attempt completes. This is specified now rather than deferred because 2b-ii will inherit `finish` as-is and would have no reason to look at it.

### Where a cancelled run lands

**`StateFailed`, with `"cancelled: console shutting down"`.** No new state.

This satisfies the handoff's contract item — "fail-closed plus explicit Retry rather than auto-resume on an interrupted Apply" — because `Retry` already resumes from the step cursor and requires exactly `StateFailed`.

A `StateCanceled` would read more honestly and was rejected: `StateActive` is already declared-and-unreachable and the handoff carries a paragraph explaining why nobody should delete it. Adding a second speculative state before its consumer exists repeats that. Phase 3's Stop-workload control can introduce it with a real consumer.

`awaitDecisions` returning false on `ctx.Done()` currently leaves the run in `StateAwaitingDecision` and returns. It must also finish into `StateFailed`, for the same durability reason as above.

### Shutdown ordering

The invariant is **"do not return from `main` before the process tree is reaped"** — not "do not begin HTTP shutdown first." Those are different, and conflating them creates a hole:

```
signal
  → mark draining (mutating handlers now reject with 503)
  → start httpSrv.Shutdown concurrently
  → eng.CancelAndWait(bounded ctx)
  → wait for both
  → return
```

**Draining first is what closes the hole.** Cancellation lands the run in `StateFailed`, which `isLive` does not consider live — so during a 15-second wait with HTTP still fully open, a `POST /api/runs` would happily start a *new* run that shutdown then kills mid-flight. Rejecting mutations at the door removes that window. Safe methods keep serving so a connected browser sees the timeline through shutdown.

HTTP draining and engine cleanup then run **concurrently** rather than in sequence, which matters for the grace budget below.

Getting the return wrong is what strands a release: with `ENTRYPOINT ["/usr/local/bin/aicrme"]` and no init process, aicrme is **PID 1**, so returning from `main` tears down the whole PID namespace and SIGKILLs `helm` before `deploy.sh`'s trap runs — the `pending-install` scenario the handoff documents, which the next `helm upgrade --install` refuses and `deploy.sh`'s preflight does not clean up.

### The grace budget must fit Kubernetes'

`terminationGracePeriodSeconds` defaults to **30 seconds**, after which the kubelet SIGKILLs the pod regardless. The engine wait must exceed `killGrace` (10s) so the applier's own escalation can complete, and the HTTP drain needs room too — sequential 15s + 10s leaves almost nothing.

So: run them concurrently, set the engine wait to ~15s, and **set `terminationGracePeriodSeconds` explicitly in the chart** rather than inheriting the default, with a chart-contract assertion pinning it. A budget that only works by accident against an unstated default is the kind of thing that breaks when someone edits the Deployment for an unrelated reason.

---

## Testing

| Unit | Approach |
|---|---|
| observer summaries | `client-go` fake clientset driving synthetic watch events, asserting emitted `bus.Event`s — the approach `approach.md`'s testing table already names |
| **observer volume control** | **An unchanged state must emit nothing.** Feed an update changing only `managedFields`/annotations and assert zero events. This is the property the whole design rests on; a test proving only "a change emits an event" would pass against a relay that floods |
| observer Node dedup | A repeated identical allocatable value emits once, not twice — the specific bug caching the *message* instead of the *state* would introduce. Quantities compared via `Cmp`, so `8` and `8000m` are one state |
| observer Add semantics | An initial list of pre-existing objects emits nothing; a node already at 8 GPUs is never narrated as `0 → 8` |
| observer Delete | Delete-then-recreate with the same name does not inherit stale state; `DeletedFinalStateUnknown` tombstones are handled |
| observer Deployment staleness | `observedGeneration < generation` suppresses emission; denominator is `spec.replicas`, so a scale-up in progress never reports `1/1 ready` |
| observer filtering | Namespaced workloads outside `RunScope.Namespaces` emit nothing; Nodes always emit; an empty scope filters all namespaced kinds |
| observer degradation | A nil client yields a no-op observer, not a panic |
| `CancelAndWait` | Fake `Step` blocking until its context dies: asserts `StateFailed`, the message, that `done` closes only after persistence, and that `Retry` works afterwards |
| cancellation edges | Cancel while parked in `awaitDecisions`; idempotent double-cancel; cancel with no run in flight; the cancel/step-completion race; a fresh context after `Retry` |
| terminal persistence | A store that **rejects cancelled contexts** must still receive the terminal write — the failure mode 2b-ii would otherwise inherit |
| shutdown ordering | Assert draining rejects a mutation, that safe methods still serve, and that `CancelAndWait` completes before return. The ordering *is* the defect this exists to prevent, so it needs an assertion, not a comment |
| chart | `terminationGracePeriodSeconds` rendered and pinned in `test/chart/contract.sh` |
| e2e | **No change.** `apply-dryrun.sh` fails at 3/14 before any rollout progresses, so the observer has nothing to show there. Real observer output first appears on Phase 4 hardware |

Standing constraints hold: 80% coverage floor, `-race`, `make qualify` green, signed commits.

---

## Open questions

1. **Rate limiting beyond change-detection.** Change-only emission should bound volume adequately, but this is unverified against a real 8-node driver rollout — the first honest measurement is Phase 4. If it proves insufficient, a per-object minimum interval is an additive fix that does not change this design.
2. **Node signal specificity.** `nvidia.com/gpu` is the resource that matters here; whether MIG-profile resource names (`nvidia.com/mig-*`) deserve the same treatment is a Phase 4 calibration question.
3. **Per-component sub-status correlation.** Declared out of scope above. The open part is the join key: namespace is the only reliable one and it is not one-to-one with components. Worth designing against real Phase 4 output rather than against KWOK.
4. **Ownership and budget** — `approach.md` Open Question 1, still untouched, still gating Phase 4. The dry-run ceiling has since made Phase 4 a hard dependency for full-chain validation rather than a preference, which raises the cost of leaving this open.
