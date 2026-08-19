# Phase 3 — Prove

**Date:** 2026-08-19
**Status:** Design, revision 2 (after review — five findings, four P0)
**Scope:** The reference workload that closes the demo arc. Simulated on Kind/KWOK.

---

## What Phase 3 is, and what it stopped being

`approach.md` scoped Phase 3 as "Validate and Prove, simulated on Kind." **Validate is out**,
on measurement rather than preference: `ValidateState` reports `passed` for checks that never
executed on any cluster carrying KWOK's simulated GPU nodes — and those nodes are a
prerequisite of the demo path, because a cluster without GPU nodes has no derivable accelerator
and cannot resolve a recipe at all. Evidence: `docs/phase-2-handoff.md`, `docs/spikes/`.

That is deliberately *not* a test gap. A gap is "we do not know"; this is "we are told
something untrue."

**Phase 3 is Prove alone.**

## Why it matters

Everything through Apply produces a claim about the tool — *fourteen actions installed*. The
demo lands when the claim becomes one about the operator's cluster. Prove is where that
happens, and its payoff is the callback to Discover: the capability gap, answered.

---

## 1. Allocation is scalar, not DRA

**Revision 2 correction.** Revision 1 specified DRA device claims. That cannot work here, and
the reason is measured, not argued:

```
$ kubectl get resourceslices        # on a live make demo cluster, 2026-08-19
No resources found
$ kubectl get node gpu-0 -o jsonpath='{.status.capacity.nvidia\.com/gpu}'
8
```

KWOK's simulated nodes advertise **scalar `nvidia.com/gpu` capacity only** and publish no
`ResourceSlices` at all. Separately, AICR disables full-GPU DRA advertising by default and the
Kind overlay does not enable it — so even a recipe change would not produce slices without
deliberately flipping mutually exclusive recipe options.

**Decision: Phase 3 allocates with scalar `nvidia.com/gpu` requests.** Two pods, 8 GPUs each,
gang-scheduled by kai-scheduler across two distinct simulated nodes.

DRA moves to Phase 4, where real drivers publish real slices. Simulating DRA here would mean
authoring `ResourceSlices` by hand and flipping recipe options away from what the demo actually
installs — building a fake to demonstrate a real thing, which is the failure mode this project
has paid for repeatedly.

## 2. Reaching `StateActive`

`Engine.execute()` ends every successful run at `StateDone`. `StateActive` is declared
(`internal/engine/run.go:42`, its comment already anticipating Prove) and has never been
reachable.

**The hook is an optional interface, not a new `Step` method:**

```go
// ActiveStep is implemented by a Step that leaves something running after Run
// returns. The engine finishes such a run at StateActive rather than StateDone.
type ActiveStep interface{ LeavesWorkloadRunning() bool }
```

`execute()` type-asserts the final step. The four existing steps are untouched.

### Two latent behaviours this activates

**`isLive()` excludes `StateActive`** (`running || awaiting_decision`), so `Start` would begin a
new run over a live workload. Closed in §5.

**`isTerminal()` excludes it** (`StateDone || StateFailed`), so the observer's scoped informers
do not tear down. **Desirable** — Prove keeps narrating — and the reason Stop must be reachable
(§6, §7).

## 3. Workload identity and ownership

**Revision 2 addition.** Revision 1 said "the resolved recipe's namespace," which is ambiguous:
`internal/steps/recommend.go` carries only per-component namespaces, and `internal/engine/run.go`
stores no workload identity at all. A restart could recover stale state, or nothing, while the
workload keeps running.

Prove owns a **dedicated namespace**, `aicrme-prove`, created by the step and never shared with
a recipe component.

Every object it creates carries:

| Label | Value |
|---|---|
| `app.kubernetes.io/managed-by` | `aicrme` |
| `aicrme.dev/run-id` | the run's ID |
| `aicrme.dev/component` | `prove-workload` |

**Owned kinds are enumerated explicitly** — the workload object, its Service if any, and its
ServiceAccount. Stop and reconciliation act on that list, never on "everything in the
namespace," so an object someone else put there is not collateral.

**Names are stable and derived from the run ID**, so identity survives a restart without
requiring the persisted record to be intact. `Run` gains a `Workload` field recording namespace,
kind, and name — but correctness does not depend on it, because the labels alone are sufficient
to find the objects. That matters because terminal saves are best-effort and the store can
degrade to memory.

**Startup reconciliation.** On boot the console lists objects carrying
`app.kubernetes.io/managed-by=aicrme` with a `prove-workload` component label. Reconciling three
cases:

- **Workload exists, run recovered as `StateActive`** → normal; offer Stop.
- **Workload exists, no matching run record** (the store lost it) → adopt it into a synthetic
  `StateActive` run so the operator can Stop it. **Never silently delete it.**
- **Run recovered as `StateActive`, workload absent** → finish the run at `StateDone`; it
  already ended.

## 4. The Prove step

`steps.NewProve(...)`, `Phase() == PhaseProve`, implementing `ActiveStep`.

1. Ensures the `aicrme-prove` namespace.
2. Renders and applies the embedded workload — 2 pods × 8 `nvidia.com/gpu`, gang-scheduled.
3. Watches for gang placement, emitting one event per placement decision.
4. Returns with the workload running.

It does not wait for completion. `Requires()` adds no new operator question.

## 5. Guarding a live workload

`Start` treats `StateActive` as live and rejects with **409**, naming the remedy: stop the
workload first. Mirrors 2b-ii's recovered-run guard, and follows the standing rule that
teardown is never a side effect of starting something.

**Rejected alternative:** a confirm that stops the workload and starts a new run in one action —
exactly the shape the never-automatic rule exists to prevent.

## 6. `Discard` must reject `StateActive`

**Revision 2 correction — this is a live hole, verified in code.** `Discard` guards only on
`isLive(e.current.State)` (`internal/engine/engine.go:862`). `StateActive` is not live, so
today's `Discard` would accept it: nil `e.current`, delete the persisted record, leave the
workload holding GPUs, and free `Start`. The operator would be told the run was discarded while
16 simulated GPUs stayed allocated with nothing tracking them.

`Discard` gains an explicit `StateActive` rejection, with a message pointing at Stop.

## 7. Stop workload

Operator-initiated, and **the only way a run leaves `StateActive`**.

Same contract as Reset, for the same reason: destructive to something the operator is watching.
Never automatic — not on restart, not on timeout, not as a side effect of starting another run.

**Semantics:**
- **Idempotent.** Stopping an already-stopped workload succeeds.
- **Foreground deletion**, so the call does not report success while finalizers still run.
- **Waits for absence** before finishing the run at `StateDone`.
- **On failure**, the run stays `StateActive` and says so. It never claims to have stopped
  something it did not.

## 8. Failure paths must not orphan the workload

**Revision 2 correction.** Revision 1 asserted "Apply fails → nothing is left running." That is
not guaranteed: a multi-object apply can partially succeed, and a gang that has not placed yet
can place *later*, after a timeout has already been declared. Meanwhile `StateFailed` permits a
new run — so an orphaned workload would sit holding GPUs while the next run starts.

**Every pre-`Active` failure path cleans up**, using the same owned-kinds list and the same
wait-for-absence semantics as Stop:

| Failure | Behaviour |
|---|---|
| Partial apply | Delete every object created so far; wait for absence; then fail. |
| Gang never places within the bounded wait | Delete the workload; **wait for absence** — a pending gang can still place; then fail. |
| Cleanup itself fails | Fail the run **and keep `Start` blocked**, surfacing the orphan and offering Stop. Never report a clean failure over an uncleaned cluster. |

That last row is the one that matters: an unverifiable cleanup must not look like a successful
one.

## 9. The Prove screen

- **The callback** — Discover's recorded capability gap, answered.
- **The allocation decision** — which nodes the gang landed on, and the GPU counts consumed.
- **Live workload telemetry** via the observer's existing cluster events.
- **Stop**, always visible while active.

On a simulated cluster it carries an explicit "simulated cluster, no GPU hardware" label and
**makes no throughput claim**.

**Recovered-`StateActive` UI is in scope by necessity.** The generic recovered-run panel offers
Discard, which §6 now rejects for `StateActive` — leaving an operator with a dead end unless
Stop is offered. Narrow scope: reject Discard, offer Stop. The broader recovered-run redesign
stays out (§12).

---

## Error handling

Covered inline: apply failure and gang timeout in §8, Stop failure in §7, restart in §3.

## Testing strategy

Unit tests with a fake clientset for the step; engine tests for the `StateActive` transition,
the 409 guard, the `Discard` rejection, and the three reconciliation cases; web tests for the
screen including the simulated-cluster label as asserted copy.

**Fake-client unit tests cannot cover admission, controllers, finalizers, or watch behaviour.**
The following are e2e-only, extending `test/e2e/apply-real.sh`:

- **kai-scheduler runs on a real Kind worker.** KAI has a catch-all toleration, so its
  controllers can land on KWOK nodes and receive synthesized `Ready` without ever executing. If
  that happens, nothing scheduled anything and the test proves nothing.
- **All gang members schedule together across exactly two distinct simulated GPU nodes**, each
  consuming 8 of 8 allocatable — not merely "the workload object exists."
- **Restart after `StateActive`** — recovery returns to `StateActive`, Stop still works.
- **Partial apply** — cleanup removes every created object.
- **Gang timeout** — cleanup waits for absence, and a late-placing gang does not survive.
- **Failed Stop** — the run stays `StateActive` and `Start` stays blocked.

### Recorded test gaps

Per the standing rule (`docs/phase-2-handoff.md`):

- **No workload executes on KWOK.** Fake nodes run no containers. Placement is observable;
  execution, throughput, and NCCL correctness are not.
- **The workload body is unexercised.** What ships is shape-correct and content-inert.
- **DRA is entirely unexercised** (§1). No slices exist to bind.

All three belong next to the code as well as here.

## Out of scope

- Validate; real throughput and the 387 GB/s finale (Phase 4); DRA (§1).
- Reset (Phase 5) — but note Reset must stop a running workload *before* uninstalling
  components, a Phase 5 dependency on §7.
- The inference Prove path.
- The broader recovered-run screen redesign — except the narrow Discard/Stop correction in §9.

## Open questions

1. **Bounded wait for gang placement — how long?** Best answered against a real `make demo`.
2. **Does the Prove screen replace the cockpit, or extend it?**
3. **Should adoption of an orphaned workload (§3, case 2) require operator confirmation?**
   Adopting silently is friendlier; requiring a click is more honest about the console having
   found something it did not start.
