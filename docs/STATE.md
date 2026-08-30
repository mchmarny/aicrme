# State

Where this project is, what is proven and on what, and what is left. Present tense only — no
history. When something changes, edit this file rather than appending to it.

This is the only status document. The per-phase working notes it used to point at were deleted
when the repo went public: they were unmaintained and several of their claims were superseded.
Git history has them if a decision ever needs archaeology.

---

## What works

The whole arc — Discover, Recommend, Bundle, Apply, Validate, Prove, Reset — runs end to end,
driven from a laptop over the operator's own kubeconfig. `aicrme` is a local binary; it installs
nothing of itself into the cluster it configures. Every phase has now run against real GPU
hardware, but Validate has never had a check actually execute there — see the two Validation rows
below and Open work item 2.

| | Proven on | Evidence |
|---|---|---|
| Discover → Prove | real GKE H100s (2× a3-megagpu-8g, 16 GPUs) | Discover <45s, Apply 16/16 in 15m18s, Prove placed the gang one pod per H100 and the container body executed. Re-run end to end in a browser 2026-08-28 on the reworked UI: 16/16 in 10m54s, gang of 2 placed, Discover 6s |
| Discover → Prove | Kind + KWOK simulated H100s | `test/e2e/` — six jobs, on every push to main |
| Reset | real GKE H100s, helm 4.2.4 | 16 releases → 0 in 2m29s |
| Same-cluster reuse **via Reset** | **real GKE H100s**, and Kind + KWOK | 2026-08-28: Discover→Prove→Reset→Discover→Prove on one cluster; cycle 2 installed 16/16 and **placed its gang**, one pod per H100. Also `repro-kai` and `reset.sh` assertion 3 on every push |
| Same-cluster reuse **without a Reset** | **does not work, and is now refused** | see below |
| helm 4 | real cluster, **and CI on every push** | a full install *and* uninstall under v4.2.4 by hand; the `reset` e2e job now pins v4.2.4, so all six assertions — including the FAILED-teardown one — run under helm 4 while the other five jobs keep exercising helm 3.21.4 |
| Validation — the **skip** path | Kind + KWOK, **CI on every push** | `prove.sh` assert 7, first green 2026-08-29: the run record reads `{"skipped":"simulated cluster -- kwok-controller is running its fake nodes…"}` with no phase results. That cluster advertises 32 GPUs across four fake nodes, so the skip is keyed off `gap.Report.Simulated` (kwok-controller in the snapshot's image list), **not** a GPU count — a count check passes it and validates against fakes |
| Release automation | **v0.1.0 released and verified** | Published 2026-08-29 and checked end to end against the published artifacts: checksum, `gh attestation verify` (with wrong-repo and tampered-byte negative controls), the formula in `mchmarny/homebrew-tap`, and the downloaded binary serving its console. A `v*` tag runs `make qualify`, then goreleaser: four archives (darwin/linux × amd64/arm64), `checksums.txt`, a SLSA build-provenance attestation, and a Homebrew formula pushed to `mchmarny/homebrew-tap`. `ci`'s `release-dryrun` job builds the real artifacts on every push and runs `scripts/smoke-release.sh` against one, which is the only thing standing between a lost `before` hook and a shipped binary whose console is an empty directory listing |
| Validation — the **verdict** path | **ran on real GKE H100s; a PASSING verdict is still unproven** | 2026-08-29: the phase ran end to end, recorded a per-phase verdict, wrote `ctrf.json`, rendered counts on the Prove screen, and did not fail the run — all first confirmations. But all five checks failed on an image pull, because AICR was being handed aicrme's version and rewrote its validator image tags with it (fixed; see below). So the plumbing is proven and the checks are not: no validation has yet actually executed against real GPUs |

### Installing twice without a Reset

**A second run over an existing install produces a cluster that reports success and does not
work.** kai-scheduler's `SchedulingShard` is `resource-policy: keep` and owns the
`kai-scheduler-default` Deployment, which helm therefore does not own either. A second install
rolls every other kai component and leaves those two, so the cluster runs the *first* install's
scheduler against a control plane replaced underneath it — Apply reports 16/16, Prove places 0/2.

Observed 2026-08-28 on real H100s. The shard and the scheduler Deployment were two hours older
than the five kai Deployments beside them; the run records showed **no Reset had ever completed**,
though the operator believed one had, because Stop and Reset were silent and a new run appeared
the instant the old one ended. That auto-start is gone as of 2026-08-29 — it also left a stopped
run unresettable, since `engine.Reset` acts only on the current run.

`internal/steps`' `alreadyInstalled` refuses this at Apply, before anything is touched. The purge
cannot cover it: it runs only after a **confirmed uninstall**, and here there was none.

**The guard is deliberately narrow — kai-scheduler only.** Installing over a *pre-existing
dependency* is supported and tested: a cluster that already runs cert-manager is the ordinary
case, `helm upgrade --install` replaces the manifest correctly, and the ownership snapshot exists
so Reset then spares what it did not create. `test/e2e/reset.sh` is built on exactly that
collision — its bystander release is named `cert-manager`, in the `cert-manager` namespace. A
first version of the guard refused every pre-existing recipe release and failed that job on its
first run. What makes kai-scheduler different is a property, not a suspicion: its chart leaves a
cluster-scoped object that OWNS a workload, so helm cannot replace it. Add to `reinstallHazards`
only on that criterion.

**The signature, if it is ever seen again:** `kubectl -n kai-scheduler get deploy` — if
`kai-scheduler-default` is older than the Deployments beside it, that is this.

### Three residue classes that block cleanup, and nothing inventories

Found while cleaning up after the above. All three are the same shape as the kai objects —
cluster-scoped things a release created that helm does not own — but worse, because they stop a
teardown finishing at all:

- **Stale APIServices.** `prometheus-adapter` registers `v1beta1.custom.metrics.k8s.io` and
  `v1beta1.external.metrics.k8s.io`. Once its Service is gone, discovery fails cluster-wide and
  **every namespace deletion hangs**, including namespaces this tool never touched.
- **Orphaned admission webhooks.** `reset-residue.sh` R5 reports them and nothing acts. Skyhook's
  mutating webhook blocked the write that would have removed a finalizer — the CR could not be
  patched, so its CRD could not delete. A genuine deadlock, breakable only by deleting the webhook
  first.
- **Orphaned finalizers.** `Skyhook/tuning` held `skyhook.nvidia.com/skyhook` after its controller
  was uninstalled.

None is fixed. A full manual clean needs, in this order: `helm uninstall` every release, delete
the four kai objects the chart tells helm to keep, **delete the stale APIServices, delete the
orphaned webhooks**, and only THEN delete namespaces, strip finalizers and delete CRDs. The
ordering is not cosmetic and this file had it backwards until 2026-08-29: deleting namespaces
while `prometheus-adapter`'s APIServices still point at a dead Service hangs every namespace
deletion in the cluster. Walked by hand on real H100s 2026-08-29 and it works in this order.

**A component count and a deployment-action count are different numbers.** The GKE recipe resolves
**14 components**; `deploy.sh` runs **16 deployment actions**, because `gpu-operator-pre` and
`kubeflow-trainer-post` are generated. "16/16" throughout this file is actions. `pipeline.ts`
keeps the two apart deliberately (OVERRIDE 1) and so should any note about them.

**`StateActive` is Prove's terminal success state.** The reference workload is `sleep infinity`
by design and holds its placement. Nothing polls for `succeeded`; it never comes.

### What a simulated (KWOK) cluster cannot prove

The e2e suite and any local KWOK cluster run on fakes: **they never execute containers.** That
environment proves the install chain, recipe resolution and the telemetry, and it cannot prove
anything requiring a container to run on a GPU. Measured on that substrate rather than assumed:

- **The workload body never runs.** KWOK marks a gang pod `Succeeded` in the *same second* it
  binds it, without starting the container. The reference workload's `echo` is never printed and
  its image is never pulled.
- **The gang never holds its GPUs at once.** Each member completes instantly, so its resources
  are released before the next is bound — which is why both members routinely land on the same
  simulated node. Normal there, impossible on a real cluster where the pods keep running.
- **DRA is entirely unexercised.** The workload requests scalar `nvidia.com/gpu` and the fake
  nodes advertise scalar capacity only, publishing no DRA `ResourceSlices`. The DRA driver the
  recipe installs is never asked to bind a device.
- **Validation false-passes**, which is why it is skipped there — see the Validation rows above.

What it *does* prove about Prove: a GPU-aware scheduler admitted a gang, evaluated it as a group,
and bound every member to a node advertising GPUs. `test/e2e/prove.sh` asserts that and no more.

### Measurements taken on real hardware

- **MIG resource names:** none. Nodes advertise only `nvidia.com/gpu`, so `gpuResource`'s single
  constant is sufficient.
- **Apply cost**, three runs on two clusters (`measure.sh` Q4). Only two components are ever slow
  enough to explain, and only one of them is predictable:

  | | 08-26 | 08-28 cold | 08-28 warm |
  |---|---|---|---|
  | `cert-manager` | 128s | 129s | 125s |
  | `kube-prometheus-stack` | 137s | **441s** | 99s |
  | `gpu-operator` | ≤49s | 36s | 32s |
  | Apply+Prove total | 15m18s | 16m13s | 9m49s |

  `cert-manager` reproduces to within 4s across both clusters. `kube-prometheus-stack` varies 4×
  — the cold run pulled every image and provisioned Prometheus a volume, the warm one reused
  both — so `slowSteps.ts` gives it a range and **no component is called "the longest step"**:
  which one leads changed between two runs of the same recipe on the same cluster.
- **`gpu-operator` is not the long pole** where the node image already ships a driver. AICR
  detects that and logs `auto-disabled gpu-operator driver install: pre-installed driver detected
  in snapshot`. It also warns that this is decided from a **single sampled node** and applies
  cluster-wide, so a mixed pool can come up driverless (AICR #464). **A node image that ships no
  driver has still never been timed** — that is the open case for the slow-step notes.
- **Event volume:** 1706 events for a full Apply+Prove against a 20000-entry replay ring — 8.5%,
  of which 1648 are cluster events. The earlier 397 was the same measurement on a quieter run;
  the ring is sized correctly either way.

---

## Open work

Ordered by what unblocks the most. Each item says what it costs and what it is waiting on.

1. **Time a node image that ships no driver.** Every real run so far has been on GKE H100 pools
   with a pre-installed driver, so the one claim in `slowSteps.ts` nobody has measured is the
   `gpu-operator` driver compile. Costs one Apply on any GPU cluster whose nodes come up
   driverless.
2. **Validation shipped; real-hardware confirmation and evidence remain.** The `deployment` phase
   runs between Apply and Prove (`internal/steps/validate.go`) and records a verdict on the run —
   see the two Validation rows above, which separate what CI proves from what it cannot. `ValidateState` false-passes on KWOK — the validator schedules
   with a blanket toleration, lands on a fake node, and KWOK fakes exit 0 — so the simulated path
   is detected (`gap.Report.Simulated`, true when kwok-controller is present in the snapshot,
   regardless of the fake GPU count the nodes advertise) and skips validation rather than
   reporting the false pass. This reverses the Phase 3 reasoning that produced
   Prove-instead-of-Validate.

   What remains:

   - **A passing verdict.** Validate has now run on real H100s, but every check failed on an image
     pull rather than executing — AICR rewrites its validator catalog's `:latest` tags with
     whatever `WithVersion` receives, and it was receiving aicrme's version. Fixed by
     `aicrclient.AICRVersion` with a `make check-aicr-pin` guard, and **not yet re-run**. What
     remains is one more cluster run to see the checks actually execute. `make build && HELM_REGISTRY_CONFIG=~/.config/containers/auth.json ./bin/aicrme`
     on a real cluster still needs to show `validating the deployment` in the timeline, a
     per-phase verdict rather than a skip on the Prove screen, and
     `<work-dir>/runs/<id>/validation/ctrf.json` that parses.
   - **`conformance` and `performance` as post-Stop actions.** They need an out-of-band engine
     operation modelled on `engine.Reset` (guards, backgrounding, state), plus an API route and a
     form. That is a separable subsystem and should be its own plan.
   - **Evidence.** Increment 2 in the spec; depends on this plan's `[]*PhaseResult`.
     `Client.EmitRecipeEvidence(ctx, recipe, snapshot, results, opts)` is documented as
     unattended-safe and is strictly downstream of validation.

   Two constraints shape the shipped implementation: both AICR calls need a **live
   `*RecipeResult` owned by the current Client** (`assertOwns`; a summary will not do, so a
   recovered run re-resolves from the persisted snapshot — `validateStep.resolve`), and
   **per-component attribution is not derivable** — `ctrf.Builder` hardcodes `Suite` to the phase
   name, so no output identifies a component.
3. **AKS**, unblocked by AICR v0.20.0. **Verification-screen polish** via `VerifyBundle`. GitOps
   export is deprioritised.
4. **UI follow-ups** from the 2026-08-28 real-hardware pass. 27 of 30 shipped; what is left is in
   `docs/ux-feedback.md` entry 3 — the timeline still renders Discover's gap findings as bare
   amber warnings on the decision screen (the framing heading only reached the Discover panel),
   the mark is absent from the Confirm screen, and the primary button is a large expanse of
   accent. None blocks a demo.
5. **Three residue classes block a manual cleanup** — see below. Nothing inventories them, and two
   of them deadlock a teardown rather than merely lingering.
6. **`GET /api/runs/{id}/bundle` 404s for a recovered run.** `bundle.path` lives in
   `ephemeralArtifacts` and is dropped on encode, so the download is broken on exactly the path
   where debugging matters most. The run-log export does not fix it.
7. **Write up what the console actually does that nothing else does.** The README's gap table
   names live telemetry in one line; the distinctive part is unwritten. Chiefly: a cluster
   condition is attributed to the component that was installing when it appeared, **temporally
   and without claiming causation** — "cluster activity while gpu-operator installs", never
   "gpu-operator caused this". That restraint is what makes the attribution trustworthy, and it
   is the thing to lead with. Also worth capturing: the gap report before anything is installed,
   the run record surviving a restart, and validation refusing to attest to a drifted recipe.
8. **Keep the docs collapsed, and move tracking to GitHub issues.** `docs/` is now STATE.md and
   the UX list. The upstream-asks file is gone: a genuine SDK gap goes to
   [NVIDIA/aicr](https://github.com/NVIDIA/aicr/issues) as an issue rather than being carried
   here. `docs/ux-feedback.md` retires the same way once its open findings are closed — after
   that, work items live as issues on this repo. Resist growing a second status document.

### Known and deliberately not fixed

- **Uninstall is best-effort about completeness, never about destructiveness.** Reset removes helm
  releases and the named objects a chart tells helm to keep (see below). It deletes no namespaces
  and chases no orphans — it reports them and prints the command. Do not add code to chase them.
- **A Reset leaves Prometheus's 50Gi volume behind, and this is correct.** `kube-prometheus-stack`
  brings up a StatefulSet whose `volumeClaimTemplate` creates the PVC *outside* the release
  manifest, so `helm uninstall` never owned it and has nothing to delete. The next install reuses
  it by name, still holding the previous run's TSDB. It is the same class as the kai objects the
  purge deletes, but it must **not** join them: that table exists because those four break the
  next install, and a volume holds the operator's data. `reset-residue.sh` R8 reports PVCs and
  their reclaim policy — `kubectl get all`, and so R1, cannot see them, which is how a namespace
  holding 50Gi read as "0 core objects" until 2026-08-28.
- **AICR's kai-scheduler values set `postCleanup.enabled: false`**, disabling the chart's own
  post-delete cleanup. Its stated reason — the hook "does not inherit global.tolerations" — is not
  true of the pinned chart v0.14.1, whose `post-delete-job.yaml` renders them. This costs nothing
  now that Reset purges the objects itself, and it is an upstream AICR recipe change rather than
  an aicrme one.

---

## The one non-obvious thing in Reset

`helm uninstall` removes a release's manifest. It does **not** remove an object annotated
`helm.sh/resource-policy: keep`, and it never owned a pre-install hook resource at all.
kai-scheduler's chart creates four objects of exactly those two kinds, so all four outlive the
release by design:

```
SchedulingShard/default      resource-policy: keep
Queue/default-parent-queue   resource-policy: keep
Queue/default-queue          resource-policy: keep
Config/kai-config            a pre-install hook
```

The shard is the one that matters. It owns the `kai-scheduler-default` Deployment — which helm
therefore does not own either — so a reinstall finds the shard present and matching, never
recreates the Deployment, and the cluster keeps running the *previous* install's scheduler pod
against a control plane replaced underneath it. That pod does not schedule new gangs, which is
why a second run used to install everything and then fail in Prove with nothing placed.

`internal/teardown/purge.go` deletes those four by name after a **confirmed** uninstall.
Two properties are load-bearing and should not be refactored away:

- **The purge runs inside the uninstall branch and nowhere else.** A skipped release (adopted, or
  in a namespace whose pre-Apply state could not be recorded) belongs to somebody else, and so do
  its objects. A *failed* uninstall may leave controllers running that would recreate what was
  just deleted.
- **The table lists names, not rules.** Deleting every custom resource whose CRD this run
  installed would take an operator's own Queues and every workload's PodGroups with it. The narrow
  scope is also sufficient — measured, not assumed.

If a gang does not place, the pod-grouper's `already exists` / `object has been modified` warnings
are **not** the cause. They appear on healthy first installs too. The signal is the *absence* of
any `Scheduled` or `Bound` event.

---

## How to verify things

```sh
make qualify        # the full local gate; must match CI exactly
```

**End-to-end suites** (`test/e2e/`) each create and destroy their own Kind cluster and need a
container runtime. Every cluster call is pinned to `kind-${CLUSTER}` and refuses to run when
`CLUSTER` is unset, so they cannot be aimed at an ambient context.

```sh
test/e2e/reset.sh   # ownership, teardown, and the kai purge
test/e2e/prove.sh   # gang placement, console restart over a live workload
```

**Reproduce the same-cluster reuse failure** (~7 minutes, no GPU needed). Runs in CI because it
needs Docker and a few GB of memory:

```sh
gh workflow run repro-kai.yaml --ref main -f purge_scope=chart
```
`purge_scope=chart` deletes the four named objects, which is what the product does.
`purge_scope=all` deletes every kai CR and the namespace, and exists only to show that `chart`
is sufficient rather than merely smaller. Expect roughly `10s / TIMEOUT / 3s` across the three
cycles. It writes its own throwaway `KUBECONFIG` and cannot touch yours — **copy that pattern**
in any new script that creates a cluster.

**Measure a real-hardware run.** Read-only, and it must run **before the console is stopped**:
the session cookie and the in-memory event bus both die with the process, and two of the answers
exist nowhere else.

```sh
test/hardware/measure.sh <run-id> 'http://127.0.0.1:PORT/?t=TOKEN'
```

**Inventory what a Reset left behind.** Read-only; the context is a required argument with no
default. Take one snapshot before and one after, and diff them.

```sh
test/hardware/reset-residue.sh <kube-context> before-reset > /tmp/r0.txt
test/hardware/reset-residue.sh <kube-context> after-reset  > /tmp/r1.txt
diff /tmp/r0.txt /tmp/r1.txt
```

---

## Running against a real GPU cluster

```sh
make build
./bin/aicrme --context <context>
```

**No variables.** A tainted GPU pool used to need `AICRME_GPU_TOLERATIONS`, and that was never a
discovery problem: Connect already read the nodes, computed the exact taint the agent could not
tolerate, and printed `AICRME_GPU_TOLERATIONS=<taint>` on screen with instructions to quit and
relaunch. It now derives that set and adopts it, reporting what it adopted as
"this run will tolerate …". The variable still works as an override and as a way to add a
toleration the derivation deliberately will not.

Two exclusions in `untoleratedGPUPoolTaints` are load-bearing and must survive any refactor:
**simulated nodes are skipped** (tolerating the KWOK taint turns the demo into a silent false
success — see `steps.AgentTolerations`) and **non-GPU nodes are skipped** (a taint on a CPU node
is somebody else's reservation). The derived toleration is a narrow `Equal` on key, value and
effect, never `Exists`, so `dedicated=gpu-workload` cannot also buy
`dedicated=someone-elses-reservation`.

One knob, still, rather than two: the same derived set is fed to the agent Job **and** the Prove
workload, because two that can disagree is how you fix Discover and still fail Prove twenty
minutes later.

**`AICRME_SNAPSHOT_NODE_SELECTOR` should not be set on a real cluster.** `console.go` and
`steps.DiscoverConfig` both document it as a KWOK-only knob — unset, AICR's own client auto-targets
a real GPU node — and it exists so the simulated e2e can pin the agent off fake-executing nodes.
Every real run so far has set it anyway, copied from this runbook, so **nobody has tested dropping
it**. Do that on the next real cluster; if Discover lands on a GPU node without it, delete this
paragraph.
