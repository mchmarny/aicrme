# State

Where this project is, what is proven and on what, and what is left. Present tense only — no
history. When something changes, edit this file rather than appending to it.

The `docs/phase-*.md` files are working notes from the phase that produced them. They are not
maintained and several of their claims are superseded. Read this file instead.

---

## What works

The whole arc — Discover, Recommend, Bundle, Apply, Prove, Reset — runs against real GPU
hardware, driven from a laptop over the operator's own kubeconfig. `aicrme` is a local binary; it
installs nothing of itself into the cluster it configures.

| | Proven on | Evidence |
|---|---|---|
| Discover → Prove | real GKE H100s (2× a3-megagpu-8g, 16 GPUs) | Discover <45s, Apply 16/16 in 15m18s, Prove placed the gang one pod per H100 and the container body executed |
| Discover → Prove | Kind + KWOK simulated H100s | `test/e2e/` — six jobs, on every push to main |
| Reset | real GKE H100s, helm 4.2.4 | 16 releases → 0 in 2m29s |
| Same-cluster reuse | **real GKE H100s**, and Kind + KWOK | 2026-08-28: Discover→Prove→Reset→Discover→Prove on one cluster; cycle 2 installed 16/16 and **placed its gang**, one pod per H100. Also `repro-kai` and `reset.sh` assertion 3 on every push |
| helm 4 | real cluster, **and CI on every push** | a full install *and* uninstall under v4.2.4 by hand; the `reset` e2e job now pins v4.2.4, so all six assertions — including the FAILED-teardown one — run under helm 4 while the other five jobs keep exercising helm 3.21.4 |

**A component count and a deployment-action count are different numbers.** The GKE recipe resolves
**14 components**; `deploy.sh` runs **16 deployment actions**, because `gpu-operator-pre` and
`kubeflow-trainer-post` are generated. "16/16" throughout this file is actions. `pipeline.ts`
keeps the two apart deliberately (OVERRIDE 1) and so should any note about them.

**`StateActive` is Prove's terminal success state.** The reference workload is `sleep infinity`
by design and holds its placement. Nothing polls for `succeeded`; it never comes.

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

1. **Release automation.** goreleaser + brew + an install script, macOS and Linux, no Windows.
   Nothing blocks it: the helm 4 obligation that used to gate the first release closed on
   2026-08-28.
2. **Time a node image that ships no driver.** Every real run so far has been on GKE H100 pools
   with a pre-installed driver, so the one claim in `slowSteps.ts` nobody has measured is the
   `gpu-operator` driver compile. Costs one Apply on any GPU cluster whose nodes come up
   driverless.
3. **`ValidateState` false-passes on simulated nodes.** Known and deferred; it is why the demo
   claims Prove rather than Validate.
4. **AKS**, unblocked by AICR v0.20.0. **Verification-screen polish** via `VerifyBundle`. GitOps
   export is deprioritised.
5. **Collapse the docs.** When the work is done, rewrite from current state and delete
   `docs/phase-*.md` rather than editing them.

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
make demo           # the whole arc on Kind + KWOK, in a browser
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
AICRME_SNAPSHOT_NODE_SELECTOR='<a label only GPU nodes carry>' \
AICRME_GPU_TOLERATIONS='<the cluster's own GPU taint>' \
  ./bin/aicrme --context <context>
```

Both variables exist because a platform team routinely taints its GPU pool with something other
than `nvidia.com/gpu` (GKE used `dedicated=gpu-workload`). The first biases AICR's snapshot agent
onto a GPU node — the accelerator is read from an in-pod PCI probe, so an agent that lands
anywhere else produces a snapshot with no accelerator and *no recipe resolves*, which surfaces at
Recommend, far from the cause. The second is fed to the agent Job **and** the Prove workload,
deliberately one knob rather than two: two that can disagree is how you fix Discover and still
fail Prove twenty minutes later. A pool tainted with the standard `nvidia.com/gpu` needs neither.
