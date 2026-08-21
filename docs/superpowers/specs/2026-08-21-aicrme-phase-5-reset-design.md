# Phase 5 — Reset

**Date:** 2026-08-21
**Status:** Approved for planning (revision 2)
**Scope:** Reset only. Phase 5's other items (AKS, GitOps export, verification-screen
polish) are separate slices and are not designed here.

**Revision 2** rewrites §2, §3, §5, §7 and §9 against six findings from review, four of which
invalidated claims revision 1 made. The corrections are marked in place; the summary is that
revision 1 assumed ownership it could not prove, reused two mechanisms that do not compose,
and specified a UI the SPA cannot render unchanged.

**Assumptions this design rests on**, stated because every finding below traced back to one of
them: a single console replica with one current run; the durable record is authoritative;
restarts, cancellations and partial Kubernetes failures are normal, not exceptional.

---

## What Reset is, and what it is not

Reset is the operator-initiated teardown of what one run installed: stop the Prove workload,
`helm uninstall` every release that run's Apply **created** in reverse install order, then
remove the namespaces that run created and left empty. It exists so a demo is repeatable on
the same cluster without rebuilding it.

It is **not** a cluster cleaner. It acts only on a run this console still has a record of, only
on releases that run created, and only on namespaces that run created.

**Reset is never automatic.** Operator-initiated and operator-confirmed, always — the rule
`approach.md` states and `docs/phase-2-handoff.md` carries forward. Nothing may trigger it on
the operator's behalf: not a failed run, not a restart, not a timeout, not a discard.

## Why it matters

Today the only way to re-run the demo on a cluster is `make demo-down` and a full rebuild.
Reset replaces the last two steps of that with about a minute, and it is the last unbuilt piece
of the arc the README describes.

It is also the most destructive thing this product will ever do, which is why most of this
document is about what it refuses to do.

---

## 1. What Reset acts on

**A run whose record this console still holds, in a state that could have installed
something:** `StateDone`, `StateFailed`, or `StateActive`. Live states are refused — an execute
goroutine is driving them, and tearing down underneath it is the race `Discard`'s guard exists
to prevent.

A run whose `Components` projection is empty is refused: it never reached Apply, so there is
nothing to uninstall, and offering a destructive button that would do nothing is worse than not
offering it.

**Rejected alternative — discovery.** Reset could ask helm what is installed across the
recipe's namespaces. Rejected because helm releases carry no aicrme marker: the console would
have to infer ownership from namespace membership, and would eventually uninstall something a
human installed.

## 2. Establishing ownership — what Apply must record

**Revision 1 was wrong here.** It claimed `run.Components` "cannot capture a bystander, because
deploy.sh only prints a header for a release it is about to install". AICR's generated
`install.sh` runs:

```sh
helm upgrade --install ${FORCE_CONFLICTS_FLAG} {{ .Name }} "${CHART}" \
  --namespace {{ .Namespace }} --create-namespace ...
```

*upgrade* `--install`. A release a human already had at the same (name, namespace) is adopted
and upgraded, prints a header exactly like any other action, and lands in `run.Components`
indistinguishable from one the console created. Reset would then uninstall it. A deploy header
proves the console *touched* a release, never that it *created* one.

So Apply records three things before it starts, and Reset treats all of them as ownership
evidence:

| Recorded | How | Used for |
|---|---|---|
| Pre-existing releases | `helm list -n <ns> -o json` per recipe namespace, before `deploy.sh` runs | Reset uninstalls only releases **absent** from this snapshot |
| Namespace existence + **UID** | `Get` per recipe namespace, before `deploy.sh` runs | Reset deletes only namespaces absent from this snapshot, whose UID still matches if present |
| Per-release namespace | `ComponentState.Namespace`, carried from deploy.sh's own header | The uninstall target |

The snapshot is taken **before** Apply because that is the only moment the answer exists:
`--create-namespace` and `--install` both erase the distinction the instant they run. It is
persisted on `Run` (small — roughly fourteen name/namespace pairs and ten namespace records),
not as an artifact, so the envelope's size guard cannot shed it.

Ownership is computed at Reset time as `run.Components` minus the snapshot, so it is correct
whether Apply succeeded or failed partway. **Anything Reset cannot prove it created is skipped
and named in the report** — never uninstalled on a guess.

### Where the per-release namespace comes from

`deploy.sh` prints one header per deployment action carrying exactly what an uninstall needs:

```
┌─ [1/14] cert-manager  →  cert-manager
┌─ [2/14] nfd  →  node-feature-discovery
```

`internal/applier/parse.go`'s `reHeader` already captures the fourth field and
`applier.ComponentData` already carries it as `Namespace`; the engine drops it when projecting
into `ComponentState`. Carrying it through gives Reset the target namespace with no new source
and no import from outside AICR's frozen `pkg/client/v1` surface —
`pkg/bundler/deployer/localformat.Folder` carries the same data but would be a third
out-of-freeze import, which `approach.md`'s Risk 1 already names as a structural weakness.

The bundle directory is not a candidate: it lives in the pod's `emptyDir` and is gone after any
restart.

**The trap this must not fall into.** `ComponentState` is nested inside `Run`, and Ruling 20's
parity test walks `Run`'s *top-level* exported fields. A `Namespace` added to `ComponentState`
but not to `envelope.go` would persist as empty, survive every test, and surface as a Reset
that uninstalls nothing after a restart — the `CleanupUnconfirmed` defect exactly. **Extend the
parity test to nested struct fields before adding the field.**

## 3. Execution

**An engine operation, backgrounded** — `POST /api/runs/{id}/reset` → `engine.Reset`, with its
own epoch, cancel func and `done` channel in the same `e.cancel`/`e.done` slots a run uses.
Those slots are safe to share because the guards make a run and a Reset mutually exclusive, and
it is what makes `CancelAndWait` cover Reset at shutdown without a second mechanism.

**A new state, `StateResetting`**, live by `isLive`'s definition so `Start`, `Discard` and
`Stop` all refuse a run being torn down. Consequences to wire deliberately:

- `validState` (`recover.go`) must accept it, or a record persisted mid-Reset fails validation
  and disables persistence for the whole process.
- `Recover` flips live states to `StateFailed`. A Reset interrupted by a restart comes back
  **failed, not resumed** — correct, since no goroutine survives and Reset may never restart
  itself.
- `isTerminal` must *not* include it, so the observer keeps its informers up and narrates the
  teardown's own pod deletions as they happen.

**A new `internal/teardown` package** owns the helm invocations behind its own minimal `Exec`
interface. `internal/applier.BashExec` satisfies it structurally, so production wires the same
process seam without `teardown` importing `applier`. `engine.go` is already 1295 lines; it
should orchestrate this, not implement it.

**Helm environment needs no plumbing.** `HELM_CACHE_HOME`, `HELM_CONFIG_HOME` and
`HELM_DATA_HOME` are container-level env (`charts/aicrme/templates/deployment.yaml`) and
`exec.go` builds every command's env from `os.Environ()`, so a teardown subprocess inherits the
same writable caches that make `readOnlyRootFilesystem: true` work for Apply.

### 3a. An incomplete teardown must not look like an ordinary failure

**Revision 1 landed every failure path in `StateFailed`, which is not inert.** For an ordinary
failed run the engine today permits `Retry` (gated only on `StateFailed`, resuming from
`StepIndex` — so it would re-run *Apply*, reinstalling what Reset had just half-removed),
`Start` (every guard passes, overwriting the only record of what is still installed), and
`Discard` (deleting that record outright). All three are reachable the moment a Reset fails.

The remedy has a precedent rather than needing invention: Ruling 12's `CleanupUnconfirmed`
already blocks `Start` and `Discard` on a run whose cleanup could not be confirmed, leaving one
remedy. A failed Reset **is** an unconfirmed cleanup. So:

- Reset persists a **teardown-incomplete** guard on any non-clean outcome — failure,
  cancellation, or restart — carrying the residue inventory (releases and namespaces still
  believed present).
- While set, the guard rejects `Start`, `Discard`, **and `Retry`** — the last is new; Ruling
  12's guard does not cover it because a pre-Prove cleanup failure has nothing Retry could make
  worse, whereas re-running Apply over a half-torn-down cluster does.
- The only accepted operation is **Reset again**, which is safe to repeat: `--ignore-not-found`
  makes a second pass idempotent, and ownership evidence (§2) is persisted independently of how
  far the first pass got.

The guard clears when a Reset completes with nothing left behind.

### 3b. Reset cannot call `Engine.Stop`

`stoppable()` accepts three shapes: `StateActive`; `StateDone` with a recorded workload; and
`StateFailed` with `CleanupUnconfirmed`. An ordinary failed run — a legitimate Reset target —
matches none, so `Stop` answers 409. Moving to `StateResetting` first does not help: that state
is not stoppable either. Calling `Stop` *before* claiming the run leaves a window in which the
run is not yet owned by the Reset.

So: **extract the idempotent delete-then-wait-for-absence primitive** out of `Stop` into
`internal/prove` (where `Delete` and `WaitAbsent` already live), leave `Stop` as one caller of
it, and have Reset persist `StateResetting` atomically first, then invoke the primitive
directly. For a run that never reached Prove, confirmed absence satisfies the precondition —
there is nothing to stop, which is not an error.

## 4. Order of operations

1. **Ensure the Prove workload is gone**, via §3b's primitive. Confirmed absence counts.
2. **Uninstall each owned release in reverse `Index` order:**
   `helm uninstall <name> -n <namespace> --ignore-not-found --wait --timeout 5m`
   (timeout provisional — Open Question 1). Releases present in the pre-Apply snapshot are
   skipped and named.
3. **Remove each namespace this run created and left empty** (§5).

**Step 1 is a hard precondition, not a parallel step.** If it fails, Reset aborts before
touching a single release, sets §3a's guard, and says why. Uninstalling kai-scheduler and the
GPU operator out from under a gang that still holds devices is the failure mode `approach.md`
names.

## 5. Namespaces

**Revision 1's rule was unsafe twice over.** It checked six workload kinds — missing Services,
Secrets, ConfigMaps, RBAC, CronJobs, PDBs and every custom resource, all of which a namespace
delete destroys. And it assumed the namespace still exists to check: AICR downgrades
`--create-namespace` to false when a chart ships its own `Namespace` manifest
(`localformat/local_helm.go`), so for those components **helm uninstall deletes the namespace
itself**, with everything in it, before §5 ever runs.

The rule, tightened:

- **Only namespaces absent from the pre-Apply snapshot** are candidates. A namespace that
  existed before this run is never deleted, whatever is in it.
- **The UID must still match** what was recorded when the namespace was created. A namespace
  deleted and recreated by someone else in the interim is a different object wearing the same
  name.
- **Emptiness is established by discovery**, enumerating namespaced resource kinds from the
  API server's own discovery document rather than a hardcoded list, so a kind nobody thought of
  still counts as a bystander.
- **Fail closed.** Any error listing or discovering — RBAC, a partitioned API server, an
  unreachable aggregated API — leaves the namespace in place and is reported. An unanswered
  question is not an empty namespace.
- A namespace already gone (the chart-owned case) is success, not an error.

## 6. Failure policy — deliberately inverted from Apply

Apply runs **without** `--best-effort`: the first component to exhaust its retries ends the
run, because continuing past a failure finishes on a cluster that looks installed and is not.

Reset does the opposite: **it continues past a failed uninstall and reports every one.** Each
uninstall is independent, and stopping at the first failure leaves strictly more residue than
finishing — the opposite of what the operation is for.

Reset always reports counts, never a bare verdict: "14 of 14 released, 3 namespaces removed",
or "12 of 14 released, 2 failed" naming them, plus anything skipped for want of ownership. An
operator must be able to tell a clean teardown from a partial one without reading the timeline.

## 7. What happens afterward

**On a clean Reset** — everything owned removed, nothing left believed present — the run record
is deleted (`store.Delete` plus clearing `e.current`, the two steps `Discard` takes), so the
console starts a fresh run and the cluster is ready for another demo.

It must also clear `recoveredPending`, for the reason Stop's own fix round found (M2): a Reset
that succeeds against a recovered run and leaves that gate set has `Start` answering "a
recovered run is waiting for retry or discard" about a run that no longer exists.

**On any non-clean outcome — failure, cancellation, or restart —** the record survives with
§3a's teardown-incomplete guard set and the residue inventory attached. Reset-again is the only
accepted operation until it comes back clean. The bus keeps the timeline either way.

## 8. Cancellation

**Revision 1 said cancellation "stops after the in-flight uninstall"; the reused seam does not
do that.** `internal/applier/exec.go`'s `BashExec` signals the whole process group with SIGTERM
the moment its context is cancelled, escalating to SIGKILL after `killGrace`. Cancelling a
`helm uninstall --wait` mid-flight is how a release ends up half-removed — precisely the
residue Reset exists to eliminate.

The two cancellations are therefore different operations:

- **Operator cancel** is cooperative. The in-flight `helm uninstall` runs to completion against
  a context that is *not* the cancellable one; the cancellation is observed between releases.
  Worst case the operator waits out one uninstall timeout.
- **Shutdown drain** keeps today's behaviour: the pod is leaving, SIGTERM is unavoidable, and
  the run lands in §3a's guarded state exactly as a restart does.

Consequence for the implementation: `teardown` must not hand the cancellable context straight
to `Exec`. It takes both — one to run the command with, one to check between commands.

## 9. The Reset screen

**Revision 1 claimed the existing cockpit renders this unchanged. It does not.**
`web/src/pipeline.ts`'s `ComponentData.status` is install-specific
(`'started' | 'installed' | 'failed' | 'retrying'`), and `deriveComponents` builds row order
from first-seen events — so reusing `KindComponent` verbatim would label rows "installing" and
"installed" while they are being removed, in install order rather than teardown order, and
would offer whatever actions the install screen offers.

What is needed instead:

- An **operation boundary** in the stream, so the SPA can tell one run's install rows from its
  teardown rows rather than merging them by name.
- **Teardown-specific statuses** (`removing`, `removed`, `skipped`, `failed`) alongside the
  install ones, so a row's label matches what is happening to it.
- **Reverse ordering** for the teardown view.

Reset is offered from terminal-state screens (`done`, `failed`, and the Prove screen once
stopped), never mid-run. While `StateResetting`, the only control is Cancel. A run carrying the
teardown-incomplete guard shows the residue inventory and offers only Reset again.

## 10. The confirm gate

Apply — strictly less destructive — cannot be passed without a recorded decision. Reset
carrying a weaker contract would be backwards.

- **API:** the body must carry an explicit confirmation (`{"confirm":"reset"}`). A bare POST is
  rejected, so a stray `curl` or a mis-wired button cannot trigger it.
- **UI:** a two-step confirm listing every release and namespace by name, and separately
  anything that will be **skipped** for want of ownership, before the second click. The
  operator sees exactly what is about to be removed and what is not.

---

## Error handling

| Failure | Behaviour |
|---|---|
| Workload stop fails | Abort before any uninstall. Guard set. Nothing removed. |
| A single `helm uninstall` fails | Record it, continue with the rest, guard set at the end. |
| A release is not owned | Skip it, name it in the report. Not a failure. |
| A namespace is not owned, or its UID changed | Leave it, name it. Not a failure. |
| Namespace emptiness cannot be determined | Leave it, name the error. Fail closed. |
| A namespace delete fails or hangs | Record it, continue. Terminating namespaces are expected residue. |
| A namespace is already gone | Success. The chart owned it and helm removed it. |
| Operator cancels | Finish the in-flight uninstall, stop before the next. Guard set, residue inventory recorded. |
| Console restarts mid-teardown | Recovered as `StateFailed` with the guard set. Never resumed automatically. |

## Testing strategy

| Unit | Approach |
|---|---|
| `teardown` | Fake `Exec`, golden command sequences: reverse order, exact `-n` flags, `--ignore-not-found`, per-release failure isolation, and a cancellation that does **not** interrupt the in-flight command. |
| ownership | Pre-Apply snapshot containing a release: it must be skipped, not uninstalled. The bite-proof for §2. |
| namespaces | Fake clientset: not-in-snapshot + UID match + empty → deleted; UID changed → kept; any bystander kind → kept and named; discovery error → kept and named. |
| `engine.Reset` | State guards, the stop precondition aborting, the teardown-incomplete guard rejecting `Start`/`Retry`/`Discard`, epoch discipline, record deletion only on a clean outcome. |
| `envelope` | Nested-field parity **before** `ComponentState.Namespace` is added; round-trip for the snapshot and the guard. |
| `web` | Teardown statuses and reverse order render; Reset offered only in terminal states; the confirm lists removals and skips separately; a guarded run offers only Reset. |
| e2e | `test/e2e/reset.sh` on KWOK: real install, Reset, assert zero owned releases remain, namespaces gone or honestly reported, and a new run starts. Plus a pre-seeded bystander release in a recipe namespace that must survive. |

### Recorded test gaps

- **Finalizer-stuck namespaces are not exercised.** KWOK's namespaces delete cleanly; the
  hung-terminating case that motivates `deploy.sh`'s own preflight has no simulated
  reproduction. The handling is written from that preflight's evidence, not from a test.
- **CRD residue is unasserted** because Reset does not remove CRDs — the e2e records what
  survives rather than asserting it away.

## Out of scope

- **CRDs.** Helm deliberately does not delete them, AICR's bundle README says so explicitly,
  and removing them would delete every custom resource on the cluster, including ones from
  releases Reset is not touching.
- **Restoring an adopted release to its pre-Apply revision.** Reset skips releases it did not
  create; it does not roll them back. Helm cannot restore a release after an uninstall anyway,
  and rolling back an upgrade the operator asked for is a second destructive operation wearing
  the first one's confirm gate.
- **Reset without a run record.** Decided: record-scoped only.
- **Uninstalling the console itself.**
- **Phase 5's other slices** — AKS, GitOps export, verification-screen polish.

## Open questions

1. **Uninstall timeout per release.** `--wait` needs a bound; 5 minutes is the starting
   proposal, to be revisited against a real Reset the way `GangTimeout` was revisited against a
   real `make demo`.
2. **Discovery cost for the emptiness check.** Enumerating namespaced kinds per namespace is
   correct but chatty against ten namespaces. If it proves slow, the fallback is a cached
   discovery document per Reset rather than a narrower kind list — the list is what made
   revision 1 unsafe.
3. **Repeat-demo interaction with kai-scheduler.** `docs/phase-3-status.md` records that
   re-applying the recipe onto a cluster where kai-scheduler is already running can wedge the
   running scheduler until restarted. A Reset-then-Apply cycle removes kai-scheduler first, so
   it may not arise — confirm in the e2e rather than assuming either way.
