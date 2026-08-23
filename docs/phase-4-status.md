# Phase 4 — first contact with real hardware

**Date:** 2026-08-23. **Cluster:** GKE `aicr-uat-day-gh1-0-32641587213`, `us-central1`, project
`eidosx`. 6 nodes (scaled to 7 mid-run), Container-Optimized OS, 2× `nvidia-h100-mega-80gb`
with 8 GPUs each. Session ended when the control plane became unreachable, not when the work
finished.

**Headline: the arc reached `16/16 components installed on real H100s`.** Discover, Recommend,
Bundle and Apply all work on real hardware. Prove did not complete, for a reason that is
understood, fixed, and untested.

---

## What the cluster is, and why it matters

| | |
|---|---|
| GPU nodes | 2, tainted **`dedicated=gpu-workload:NoSchedule`** — NOT `nvidia.com/gpu` |
| GPU labels | `cloud.google.com/gke-accelerator=nvidia-h100-mega-80gb`; **no** `nvidia.com/gpu.present` |
| Driver | already installed by GKE — the snapshot reports `driver-loaded=true` |
| Node image | COS. Confirmed by Mark: nodewright restarts GPU nodes on **EKS and AKS**, not on COS |
| Prior AICR install | none — no releases, no AICR namespaces, no AICR CRDs |

The custom GPU taint is the single most consequential fact. It broke two different things in
the same way, and neither failure named the taint as its cause.

---

## Three defects, all found on hardware, all fixed

### 1. The snapshot agent could not reach a GPU node

`snapshotter.maybeInjectGPUNodeSelector` biases the agent Job onto a GPU node by injecting a
nodeSelector and **injects no toleration with it**. `pkg/client/v1` defaults tolerations for
validation only, never for `CollectSnapshot`, and the agent assigns `config.Tolerations`
straight through. We passed nil.

Why it matters more than it looks: the accelerator is derived from an **in-pod PCI probe**
(`fingerprint`'s `gpu.hardware.model`), with only an `nvidia.com/gpu.product` node-label
fallback that a pre-GPU-operator cluster does not have. An agent that runs anywhere else
produces a snapshot with no accelerator, and no recipe resolves — surfacing at Recommend, far
from the cause.

**Fixed:** a built-in `nvidia.com/gpu` toleration, plus `AICRME_GPU_TOLERATIONS` for the
cluster's own taint.

### 2. The reference workload could not either

Same root cause, different object. `workload.yaml` tolerates `kwok.x-k8s.io/node` and
`nvidia.com/gpu`. kai-scheduler refused the gang and said so verbatim:

```
no nodes with enough resources were found: 2 node(s) had untolerated taint(s).
5 node(s) didn't have enough resources: GPUs.
```

`workload.yaml`'s own comment had predicted this failure mode in writing; it just could not know
which taint a platform team would pick.

**Fixed:** operator tolerations appended to the decoded Job. One knob,
`AICRME_GPU_TOLERATIONS`, feeds both the agent and the workload — they have the same
requirement for the same reason, and two knobs that can disagree is a way to fix Discover and
still fail Prove twenty minutes later.

### 3. Architecture mismatch

`make image` had no `--platform`, so an arm64 laptop produced an image GKE's amd64 nodes could
not run. It fails as `exec format error`, which says nothing about its own cause.
`scripts/demo-remote.sh` now reads `kubernetes.io/arch` off the nodes and builds for that.

---

## The finding that is not a defect

**The console is killed by its own install.** It was replaced **39 times** while skyhook worked
through the nodes applying `nodewright-customizations`. Every time, the run recovered
(`recovered a persisted run … rewound=true`); afterwards a single Retry carried Apply to 16/16.
It then survived a **full image upgrade** mid-run and came back with state intact.

Two things follow.

**The restart-survival machinery paid for itself on first contact.** It is the code questioned
that same morning as possible over-engineering. Without it, those 39 events are 39 dead runs,
and the demo is impossible on any cluster running node tuning.

**And a demo that needs three retries is not yet a demo.** Absorption is not the same as
working. Whether pinning the console to a GPU-free pool reduces the churn to zero is the next
measurement, and it is cheap.

Mitigation shipped: the chart takes `nodeSelector`/`tolerations`, and `demo-remote.sh`
auto-detects a GPU-free node pool and pins there, covering the GKE, EKS and AKS pool labels.
Applied at the end of the session; **its effect was never measured**.

---

## Where the run stopped, and how to resume

Run `1f3b67780cddd780` was `running/prove` when the control plane went away. **It would have
failed regardless**: Prove **adopted the stale Job** from the previous attempt (identical pod
names) rather than recreating it. That is the Phase 3 adoption rule working as designed — it
will not delete a workload it did not start — but the adopted Job carries the pre-fix
tolerations, so the fix could not reach it.

```sh
kubectl -n aicrme-prove delete job --all   # drop the stale adopted Job
# then Retry the run; the new Job carries dedicated=gpu-workload
```

**This is a real interaction worth deciding on, not just an operational note.** A fix to the
workload spec cannot take effect while a previous workload is adoptable. Options: have Prove
recreate rather than adopt when the spec differs, or leave it manual and document it. Not
decided.

### Reproducing the environment

```sh
gcloud container clusters get-credentials aicr-uat-day-gh1-0-32641587213 \
  --region us-central1 --project eidosx
kubectl -n aicrme create secret docker-registry ghcr \
  --docker-server=ghcr.io --docker-username=mchmarny --docker-password="$(gh auth token)"
PULL_SECRET=ghcr scripts/demo-remote.sh up
kubectl -n aicrme set env deploy/aicrme \
  'AICRME_SNAPSHOT_NODE_SELECTOR=cloud.google.com/gke-accelerator=nvidia-h100-mega-80gb' \
  'AICRME_GPU_TOLERATIONS=dedicated=gpu-workload:NoSchedule'
```

The ghcr package `aicrme/aicrme` is **private**; GitHub exposes no REST endpoint to change that,
so either keep using the pull secret or flip it in the UI at
`github.com/users/mchmarny/packages/container/aicrme%2Faicrme/settings`.

---

## What GKE installs that KWOK never did

**16 install actions**, against KWOK's 14. Confirmed against the downloaded bundle:

| Only on GKE | Note |
|---|---|
| `004-nodewright-customizations` | the node-tuning component that causes the churn above |
| `007-gpu-operator-pre` | a generated pre-action; never seen before this run |
| `009-gke-nccl-tcpxo` | installs into **`kube-system`**, a namespace that always pre-exists |
| ~~`network-operator`~~ | **absent** — so `apply-dryrun.sh`'s pinned `EXPECTED_FAILING_COMPONENT="network-operator"` at index 3 is Kind-only and cannot apply here |

`gke-nccl-tcpxo` landing in `kube-system` is worth noting for Reset: the ownership snapshot
correctly records it as pre-existing and refuses to touch it. That is the behaviour shipped
earlier the same day, and it is the correct one.

AICR also **auto-disabled gpu-operator's driver install** (`pre-installed driver detected`),
with a warning that topology reports non-uniform GPU labels across nodes so the injected
`driver.enabled=false` applies cluster-wide. On a mixed cluster that could leave a
non-preinstalled GPU pool driverless — upstream tracks it as #464. Not hit here; both GPU nodes
are identical.

---

## Corrections made during the session, recorded so they are not re-derived

Three claims were stated confidently and were wrong. All were corrected in-session.

1. **"nodewright reboots the nodes."** The `interrupt: type: reboot` found in `tuning.yaml` is a
   conditional branch that did not render on this cluster; the rendered CR is
   `type: service` on `systemd-sysctl`. Mark's correction: the reboot path is real on **EKS and
   AKS**, not COS. So it is portability risk, not this cluster's problem.
2. **A containerd-restart theory** for the pod churn. No kubelet or containerd restarts occurred
   at all in the window.
3. **"`training/kubeflow` resolves to 13 components on main vs 14 today."** Running the same
   probe against v0.19.0 also gives 13; `kubeflow-trainer-post` is a bundler-generated action,
   not a recipe component. The set is unchanged.

**What was never established:** the exact mechanism killing the console pods. The churn was
real, tuning-driven, and stopped precisely when tuning reached 7/7 — that much is evidence. The
mechanism is not, and should not be asserted without new measurement.

The general lesson: the noise from these three inflated how structurally broken the situation
looked. The actual defect list is short and mostly mundane configuration.

---

## Still unmeasured

Everything `test/hardware/measure.sh` exists for. It was written for this session and **never
run** — the control plane went away first. It needs a completed run and a port-forward:

- MIG resource names (does `gpuResource`'s single constant need to widen)
- the 800 KiB envelope guard against a real snapshot, vs the 66-73 KB KWOK fixtures
- the "N of M GPUs usable" headline, first computed from real devices
- Apply duration for calibrating `slowSteps.ts` — **note the pool scaled 6→7 mid-run**, so any
  timing taken from this session is contaminated
- event volume against the 20000-entry replay ring
- whether `ValidateState` still false-passes (needs running it; the script deliberately does not)

---

## Open decisions

1. **Does Prove adopt or recreate** when the workload spec has changed? Today it adopted, and
   that silently defeated a fix.
2. **Does pinning to a GPU-free pool eliminate the console churn**, or only reduce it? Shipped,
   unmeasured.
3. **The premise question Mark raised mid-session** — whether the in-cluster design survives
   contact with what AICR does to a cluster. Current evidence: every defect found was
   configuration the AICR CLI would need too, and the one genuinely architectural tension
   (installer inside the cluster it reconfigures) was hit hard and absorbed. Not resolved, and
   should be re-asked once the finale either lands or fails for a non-configuration reason.

---

## Branch state

`phase-5-reset-shrink`, 16 commits, clean tree, Go suite green, `qualify` green at 89.7%.
**Not merged.** The KWOK `reset.sh` e2e last passed at commit `b745130`; five commits have
landed since, three of which touch scheduling (`discover` tolerations, `prove` tolerations,
chart `nodeSelector`). Those are believed KWOK-safe — the e2e pins the agent via NodeSelector,
and the added tolerations deliberately exclude `kwok.x-k8s.io/node` — but **that is reasoning,
not a green run.** Re-run `test/e2e/reset.sh` before merging.
