# Phase 5 — Reset

**Date:** 2026-08-21
**Status:** Approved for planning
**Scope:** Reset only. Phase 5's other items (AKS, GitOps export, verification-screen
polish) are separate slices and are not designed here.

---

## What Reset is, and what it is not

Reset is the operator-initiated teardown of what one run installed: stop the Prove workload,
`helm uninstall` every release that run's Apply created in reverse install order, then remove
the namespaces those releases left empty. It exists so a demo is repeatable on the same
cluster without rebuilding it.

It is **not** a cluster cleaner. It acts only on a run this console still has a record of, and
only on releases that run installed. A release a human put in one of those namespaces is not
Reset's business, and neither is a cluster whose console record has been lost.

**Reset is never automatic.** Operator-initiated and operator-confirmed, always — the rule
`approach.md` states and `docs/phase-2-handoff.md` carries forward. Nothing may trigger it on
the operator's behalf: not a failed run, not a restart, not a timeout, not a discard.

## Why it matters

Today the only way to re-run the demo on a cluster is `make demo-down` and a full rebuild:
cluster creation, KWOK install, image build and load, chart install, then a six-minute
component install. Reset replaces the last two of those with about a minute, and it is the
last unbuilt piece of the arc the product's own README describes.

It is also the most destructive thing this product will ever do, which is why the design
below spends most of its length on what it refuses to do.

---

## 1. What Reset acts on

**A run whose record this console still holds, in a state that could have installed
something:** `StateDone`, `StateFailed`, or `StateActive`. Live states (`StateRunning`,
`StateAwaitingDecision`) are refused — there is an execute goroutine driving them, and
tearing down underneath it is the race `Discard`'s own guard exists to prevent.

A run whose `Components` projection is empty is refused with a message saying so: it never
reached Apply, so there is nothing to uninstall, and offering a destructive button that would
do nothing is worse than not offering it.

**Rejected alternative — discovery.** Reset could ask helm what is installed across the
recipe's namespaces and offer to remove it, which would cover "I reinstalled the console and
want the cluster cleaned". Rejected because helm releases carry no aicrme marker: the console
would have to decide what is "ours" from namespace membership alone, and would eventually
uninstall something a human installed. Record-scoped Reset can name every release before it
touches anything.

## 2. Where the release list comes from

**`run.Components`, extended with a namespace.**

`deploy.sh` prints one header per deployment action carrying exactly what an uninstall needs:

```
┌─ [1/14] cert-manager  →  cert-manager
┌─ [2/14] nfd  →  node-feature-discovery
┌─ [4/14] nodewright-operator  →  skyhook
```

`internal/applier/parse.go`'s `reHeader` already captures the fourth field, and
`applier.ComponentData` already carries it as `Namespace`. The engine drops it when it
projects a marker into `ComponentState`, which stores only `{Name, Index, Total, Status}`.

So the change is to stop dropping it: add `Namespace` to `engine.ComponentState`, carry it
through the marker path, and persist it. `run.Components` then holds every release this run
installed, with its namespace, in install order — already persisted, already recovered by
`Recover`, already drawn by the cockpit. Reset iterates it in reverse `Index` order.

This has three properties the alternatives do not:

- **It is ground truth from the script that did the installing**, not a re-derivation. It
  includes injected `*-pre` / `*-post` releases automatically — the reason this recipe has 13
  components and 14 deployment actions.
- **It cannot capture a bystander**, because deploy.sh only prints a header for a release it
  is about to install.
- **It needs no new AICR import.** `pkg/bundler/deployer/localformat`'s `Folder` type carries
  `Name`/`Namespace`/`Index` and would work, but it sits outside the `pkg/client/v1` surface
  AICR is freezing — `approach.md`'s Risk 1 already names two such imports as a structural
  weakness, and this would be a third.

The bundle directory itself is **not** a candidate: it lives in the pod's `emptyDir` workdir
and is gone after any restart, which is why `Recover` rewinds a failed run to Bundle.

**The trap this must not fall into.** `ComponentState` is nested inside `Run`, and Ruling
20's parity test (`TestEnvelopeRoundTripsEveryRunField`) walks `Run`'s *top-level* exported
fields. A `Namespace` added to `ComponentState` but not to `envelope.go`'s projection would
persist as empty, survive every test, and surface as a Reset that uninstalls nothing from the
right namespaces after a restart — the exact shape of the `CleanupUnconfirmed` defect that
went a full fix round undetected. **The parity test must be extended to walk nested struct
fields before the field is added.**

## 3. Execution

**An engine operation, backgrounded** — `POST /api/runs/{id}/reset` → `engine.Reset`, with its
own epoch, cancel func and `done` channel, installed in the same `e.cancel`/`e.done` slots a
run uses. Those slots are safe to share because the guards make a run and a Reset mutually
exclusive, and it is what makes `CancelAndWait` cover Reset at shutdown without a second
mechanism.

`Stop` was the model considered and rejected for the execution shape: Stop is a single
blocking call, and Reset is roughly fourteen helm invocations over minutes. It needs progress
and cancellation, which the backgrounded shape gives it and a blocking method does not.

**A new state, `StateResetting`**, live by `isLive`'s definition so `Start`, `Discard` and
`Stop` all refuse a run being torn down. Consequences to wire deliberately, each of which has
bitten this project before:

- `validState` (`recover.go`) must accept it, or a record persisted mid-Reset fails validation
  and takes the unreadable path, disabling persistence for the process.
- `Recover` flips live states to `StateFailed` with the interrupted-by-restart error. A Reset
  interrupted by a restart therefore comes back **failed, not resumed** — correct, because no
  goroutine survives and Reset may never restart itself.
- `isTerminal` must *not* include it, so the observer keeps its informers up and narrates the
  teardown's own pod deletions as they happen.

**Progress rides the existing `KindComponent` events.** Reset emits the same event shape Apply
does, one per release, so `web/src/pipeline.ts`'s `deriveComponents` renders the teardown in
the cockpit with no new UI machinery.

**A new `internal/teardown` package** owns the helm invocations behind its own minimal `Exec`
interface. `internal/applier.BashExec` satisfies it structurally, so production wires the same
process seam without `teardown` importing `applier`. `internal/engine/engine.go` is already
1295 lines; it should orchestrate this, not implement it.

**Helm environment needs no plumbing.** `HELM_CACHE_HOME`, `HELM_CONFIG_HOME` and
`HELM_DATA_HOME` are set on the container (`charts/aicrme/templates/deployment.yaml`) and
`exec.go` builds every command's env from `os.Environ()`, so a teardown subprocess inherits
the same writable caches that make `readOnlyRootFilesystem: true` work for Apply.

## 4. Order of operations

1. **Stop the Prove workload and confirm it is gone.** `engine.Stop`'s existing
   delete-then-wait-for-absence path.
2. **Uninstall each release in reverse `Index` order:**
   `helm uninstall <name> -n <namespace> --ignore-not-found --wait --timeout 5m`
   (the timeout is provisional — Open Question 1).
   `--ignore-not-found` is what makes a second Reset clean rather than a wall of "release: not
   found"; `--wait` is what makes step 3's emptiness check meaningful rather than racing
   deletion.
3. **Remove each namespace that is now empty** (§5).

**Step 1 is a hard precondition, not a parallel step.** If Stop fails, Reset aborts before
touching a single release and says why. Uninstalling kai-scheduler and the GPU operator out
from under a gang that still holds devices is the failure mode `approach.md` names, and a
Reset that proceeded on a failed Stop would produce it.

## 5. Namespaces

For each distinct namespace in the release list, after the uninstalls: list Pods, Deployments,
StatefulSets, DaemonSets, Jobs and PVCs. Delete the namespace only if none remain. Otherwise
leave it and report what is still there, by name.

An enumerated kind list, not "is the namespace empty": every namespace carries a default
ServiceAccount and a `kube-root-ca.crt` ConfigMap, so literal emptiness never occurs. The list
is the same explicit-enumeration discipline `prove.OwnedKinds` uses, and for the same reason —
"everything in the namespace" makes an object someone else created into collateral.

**Rejected alternative — delete every namespace the recipe named.** It is the literal reading
of `approach.md`'s "plus namespace cleanup" and gives the cleanest repeat demo. Rejected
because a namespace is a blunt instrument: anything a human parked in `gpu-operator` goes with
it, and terminating namespaces routinely hang on finalizers, converting a clean teardown into
a stuck one.

## 6. Failure policy — deliberately inverted from Apply

Apply runs **without** `--best-effort`: the first component to exhaust its retries ends the
run, because continuing past a failure finishes on a cluster that looks installed and is not.

Reset does the opposite: **it continues past a failed uninstall and reports every one.** The
inversion is not an inconsistency. Each uninstall is independent, and stopping at the first
failure leaves strictly more residue than finishing — the opposite of what the operation is
for.

Reset always reports counts, never a bare verdict: "14 of 14 released, 3 namespaces removed",
or "12 of 14 released, 2 failed" naming them. An operator has to be able to tell a clean
teardown from a partial one without reading the timeline.

## 7. What happens afterward

**On a clean Reset the run record is deleted** — `store.Delete` plus clearing `e.current`, the
same two steps `Discard` takes — so the console starts a fresh run and the cluster is ready
for another demo. That is the feature's whole purpose.

It must also clear `recoveredPending`, for the reason Stop's own fix round found the hard way
(M2): a Reset that succeeds against a recovered run and leaves that gate set has `Start`
answering "a recovered run is waiting for retry or discard" about a run that no longer exists,
forcing a second, differently-named click for nothing.

**On any failure — including a cancellation — the record survives**, carrying what failed, so
the operator can see what is still installed and run Reset again. The bus keeps the timeline
either way, so the SPA can still show what happened after the record is gone.

## 8. The confirm gate

Apply — strictly less destructive — cannot be passed without a recorded decision. Reset
carrying a weaker contract would be backwards.

- **API:** the request body must carry an explicit confirmation (`{"confirm":"reset"}`).
  A bare POST is rejected. This is what stops a stray `curl` or a mis-wired button.
- **UI:** a two-step confirm that lists every release and namespace by name before the second
  click. The operator sees exactly what is about to be removed — which record-scoped Reset can
  always produce, and discovery-based Reset could not.

## 9. The Reset screen

Reset is offered from the terminal-state screens (`done`, `failed`, and — after Stop — the
Prove screen), never mid-run. While `StateResetting`, the cockpit renders the component rows
in reverse with their teardown status, and the only control is Cancel.

---

## Error handling

| Failure | Behaviour |
|---|---|
| Stop fails | Abort before any uninstall. Run returns to its prior state. Nothing removed. |
| A single `helm uninstall` fails | Record it, continue with the rest, report at the end. |
| A namespace is not empty | Leave it, name what remains. Not a failure. |
| A namespace delete fails or hangs | Record it, continue. Terminating namespaces are expected residue. |
| Cancelled mid-teardown | Stop after the in-flight uninstall completes; the run ends at `StateFailed` carrying what was done so far, and the record survives. Cancellation is a failure for §7's purposes, not a clean exit. |
| Console restarts mid-teardown | Recovered as `StateFailed`. Reset is never resumed automatically; the operator re-initiates. |

## Testing strategy

| Unit | Approach |
|---|---|
| `teardown` | Fake `Exec`, golden command sequences: reverse order, exact `-n` flags, `--ignore-not-found`, per-release failure isolation. |
| namespace predicate | `client-go` fake clientset: empty → deleted, each of the six kinds present → left alone and named. |
| `engine.Reset` | Fake exec + fake clientset: state guards, the Stop precondition aborting, cancellation, epoch discipline, record deletion on success and survival on failure. |
| `envelope` | Nested-field parity **before** `ComponentState.Namespace` is added. |
| `web` | Reset offered only in terminal states; the two-step confirm lists every release; Cancel present while resetting. |
| e2e | `test/e2e/reset.sh` on KWOK: real install, Reset, then assert zero releases remain, namespaces gone or honestly reported, and a new run starts. |

### Recorded test gaps

- **Finalizer-stuck namespaces are not exercised.** KWOK's namespaces delete cleanly; the
  hung-terminating case that motivates `deploy.sh`'s own preflight has no simulated
  reproduction. The handling is written from that preflight's evidence, not from a test.
- **CRD residue is unasserted** because Reset does not remove CRDs (see Out of scope) — the
  e2e will record what survives rather than assert it away.

## Out of scope

- **CRDs.** Helm deliberately does not delete them, AICR's bundle README says so explicitly,
  and removing them would delete every custom resource on the cluster, including ones from
  releases Reset is not touching. Reset stays best-effort about residue, as advertised.
- **Reset without a run record.** Decided: record-scoped only.
- **Uninstalling the console itself.** `helm uninstall aicrme` is the operator's own step; a
  console that removes itself cannot report what happened.
- **Phase 5's other slices** — AKS, GitOps export, verification-screen polish.

## Open questions

1. **Uninstall timeout per release.** `--wait` needs a bound. Apply's own retry budget is
   quadratic to 120s; 5 minutes per release is the starting proposal, to be revisited against
   a real Reset the way `GangTimeout` was revisited against a real `make demo`.
2. **Does Reset need its own `Retry`, or is clicking Reset again enough?** The proposal is
   again: `--ignore-not-found` makes a repeat pass idempotent, and a second code path for a
   destructive operation is a cost with no obvious buyer.
3. **Repeat-demo interaction with kai-scheduler.** `docs/phase-3-status.md` records that
   re-applying the recipe onto a cluster where kai-scheduler is already running can wedge the
   running scheduler until it is restarted. A Reset-then-Apply cycle removes kai-scheduler
   first, so it may simply not arise — worth confirming during the e2e rather than assuming
   either way.
