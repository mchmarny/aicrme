# Phase 3 — Prove

**Date:** 2026-08-19
**Status:** Design, approved in outline; not yet planned
**Scope:** The reference workload that closes the demo arc. Simulated on Kind/KWOK.

---

## What Phase 3 is, and what it stopped being

`approach.md` scoped Phase 3 as "Validate and Prove, simulated on Kind." **Validate is out**,
on measurement rather than preference: `ValidateState` reports `passed` for checks that never
executed on any cluster carrying KWOK's simulated GPU nodes — and those nodes are a
prerequisite of the demo path, not an artifact of it, because a cluster without GPU nodes has
no derivable accelerator and cannot resolve a recipe at all. Full evidence in
`docs/phase-2-handoff.md` ("Constraints Phase 3 inherits") and `docs/spikes/`.

That is deliberately *not* treated as a test gap. A test gap is "we do not know"; this is "we
are told something untrue." Shipping it would put a green check beside a lie.

**So Phase 3 is Prove alone.**

## Why it matters

Everything through Apply produces a claim about the tool — *fourteen actions installed*.
`approach.md`'s framing is that the demo only lands when the claim becomes one about the
operator's cluster. Prove is where that happens, and its payoff is the callback to Discover:
the capability gap Discover reported, answered.

## The arc it closes

Discover → Recommend → Bundle → Apply → **Prove**. All four predecessors are built, merged,
and gated by `test/e2e/apply-real.sh`, which installs all 14 deployment actions for real and
asserts convergence.

---

## 1. Reaching `StateActive`

`Engine.execute()` currently ends every successful run at `StateDone`:

```go
e.finish(ctx, epoch, StateDone, "")
```

`StateActive` is already declared (`internal/engine/run.go`) and has never been reachable.
Phase 3 makes it reachable — and only for Prove.

**The hook is an optional interface, not a new `Step` method:**

```go
// ActiveStep is implemented by a Step that leaves something running after Run
// returns. The engine finishes such a run at StateActive rather than StateDone.
type ActiveStep interface{ LeavesWorkloadRunning() bool }
```

`execute()` type-asserts the final step. Discover, Recommend, Bundle, and Apply are untouched
and need no change — which is the point of the optional form.

### Two consequences that already exist in the codebase

Both are latent today because nothing reaches `StateActive`. Phase 3 activates them.

**`isLive()` does not include `StateActive`.** It is `running || awaiting_decision`. So today's
`Start` would happily begin a new run over a live workload. Closed in §2.

**`isTerminal()` does not include `StateActive`** either — it is `StateDone || StateFailed`,
and the observer's scoped Pod/Event informers tear down on exactly that. **This is the
behaviour Prove wants**: a run in `StateActive` keeps its watches, so the Prove screen keeps
narrating while the workload runs. It is also the source of a recorded 2b-iii finding — a
*recovered* `StateActive` run would start watches with nothing left to tear them down. §6
closes that.

## 2. The active-run guard

A run holding a live workload must not be silently replaced.

`Start` treats `StateActive` as live: it rejects with **409** and the response names the
remedy — stop the workload first. This mirrors the recovered-run guard 2b-ii already added
for `StateFailed`, and it follows the project's standing contract that teardown is never a
side effect of starting something.

**Explicitly rejected alternative:** a confirm that stops the workload and starts a new run in
one action. Convenient for repeated demos, and exactly the shape the never-automatic rule
exists to prevent.

## 3. The Prove step

`steps.NewProve(...)`, `Phase() == PhaseProve`, implementing `ActiveStep`.

It:
1. Renders the embedded workload manifest for the resolved recipe's namespace.
2. Applies it.
3. Watches for **gang placement** and **DRA device binding**, emitting one event per
   allocation decision.
4. Returns with the workload running.

It does **not** wait for completion, and does not tear anything down. `Requires()` returns the
Apply decision keys already present; Prove adds no new operator question.

## 4. The workload manifest

**Authored in this repo and embedded** (`go:embed`, the same mechanism the SPA uses).

AICR's `demos/workloads/training/` holds exactly one file, `gke-nccl-test-tcpxo.yaml`, and
TCPXO is GKE's GPUDirect fabric — unusable on KWOK or EKS. There is nothing upstream to
consume for this path yet. (`demos/workloads/inference/` is well covered by seven files; that
matters if inference is ever the Prove path, and it is not this phase's.)

**Shape first.** On KWOK, fake nodes do not run containers, so the manifest's *content* is
inert and only its *resource shape* is observable: 2 nodes × 8 GPUs, gang-scheduled, with DRA
claims. The manifest is authored to be correct in shape now; Phase 4 replaces its body with
the real NCCL all-reduce without the step changing.

## 5. Stop workload

A new operator-initiated action, and **the only way a run leaves `StateActive`.**

Same contract as Reset, for the same reason: it is destructive to something the operator is
watching. Never automatic — not on restart, not on timeout, not as a side effect of starting
another run. It deletes the workload, then finishes the run at `StateDone`.

## 6. Recovering a `StateActive` run

A console restart while a workload runs must not orphan it.

Recovery lands a `StateActive` run **back in `StateActive`**, not `StateFailed`: the workload
genuinely is still running, and reporting otherwise would be false. The Prove screen offers
Stop. This closes the 2b-iii finding — the watches a recovered `StateActive` run starts now
have a way to end, because Stop is reachable.

## 7. The Prove screen

Shows, in order:
- **The callback.** Discover's recorded capability gap, answered — the demo's payoff line.
- **The allocation decision.** Which nodes the gang landed on, which devices DRA bound.
- **Live workload telemetry**, via the observer's existing cluster events.
- **A Stop workload control**, always visible while active.

On a simulated cluster it carries an explicit "simulated cluster, no GPU hardware" label and
**makes no throughput claim**. `approach.md` is emphatic that this is stated without apology.

---

## Error handling

- **Manifest apply fails** → the run fails normally at `StateFailed`; nothing is left running,
  so no `StateActive`, no orphan.
- **Gang never places** (insufficient capacity) → Prove emits the pending reason and fails
  after a bounded wait rather than hanging. This is a real KWOK outcome if the simulated node
  shape changes.
- **Stop fails** → the run stays `StateActive` and says so. It does not pretend to have
  stopped something it did not.
- **Console restart** → §6.

## Testing strategy

Unit tests with a fake clientset for the step, following `internal/steps`' existing shape.
Engine tests for the `StateActive` transition, the 409 guard, and recovery. Web tests for the
screen, including the simulated-cluster label as asserted copy — the same discipline 2b-iii
used for the temporal-correlation label, because copy that makes a claim is a requirement.

`test/e2e/apply-real.sh` extends to drive Prove and assert the run reaches `StateActive`, the
workload exists, Stop returns it to `StateDone`, and the workload is gone.

### Recorded test gaps

Per the standing rule (`docs/phase-2-handoff.md`, "Test gaps are recorded, not blocking"):

- **No workload actually executes on KWOK.** Fake nodes do not run containers. Gang placement
  and DRA binding are observable; execution, throughput, and NCCL correctness are not.
  Verified on real hardware before release.
- **The NCCL workload body is unexercised.** What ships is shape-correct and content-inert.
  Phase 4 is where its body is first run.

Both belong next to the code as well as here.

## Out of scope

- Validate (above).
- Real throughput numbers, the 387 GB/s finale, real DRA on real devices — Phase 4.
- Reset (Phase 5). Prove's Stop is narrower: it removes the workload it created, not the
  installed platform. Note Reset must stop a running workload *before* uninstalling
  components, which is a Phase 5 dependency on this phase's Stop.
- The inference Prove path.
- The recovered-run screen, still undesigned since 2b-ii.

## Open questions

1. **Bounded wait for gang placement — how long?** Too short fails a slow simulated cluster;
   too long hangs the demo. Best answered against a real `make demo` run.
2. **Does the Prove screen replace the cockpit, or extend it?** The cockpit is the component
   pipeline; Prove is one workload. `approach.md` implies a distinct screen but does not say
   whether the pipeline stays visible.
3. **Does Stop belong on the recovered-run screen too?** That screen is undesigned, and this
   phase should not design it — but a recovered `StateActive` run needs Stop somewhere.
