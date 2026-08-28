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
| Same-cluster reuse | Kind + KWOK | `repro-kai` workflow, and `reset.sh` assertion 3 on every push |
| helm 4 | real cluster | a full install *and* uninstall under v4.2.4 |

**`StateActive` is Prove's terminal success state.** The reference workload is `sleep infinity`
by design and holds its placement. Nothing polls for `succeeded`; it never comes.

### Measurements taken on real hardware

- **MIG resource names:** none. Nodes advertise only `nvidia.com/gpu`, so `gpuResource`'s single
  constant is sufficient.
- **Apply cost:** `kube-prometheus-stack` 137s and `cert-manager` 128s are **44% of Apply**;
  every other component is ≤49s. Both now carry a note in `web/src/slowSteps.ts`.
- **`gpu-operator` is not the long pole**, at least where the node image already ships a driver —
  GKE's H100 pools do, so it had nothing to compile and finished inside the ≤49s band. The
  console used to call it the longest step of the install; it no longer does.
- **Event volume:** 397 events for a full Apply+Prove against a 20000-entry replay ring — 2%.
  The ring is sized correctly.

---

## Open work

Ordered by what unblocks the most. Each item says what it costs and what it is waiting on.

1. **Confirm the same-cluster reuse fix on real GPU hardware.** It is proven on KWOK only. The
   original failure was on GKE, so the fix should be seen there before it is trusted. Needs a GPU
   cluster; costs one Discover→Prove→Reset→Discover→Prove cycle. Run `measure.sh` on it too: the
   slow-step notes are calibrated against one cluster, and a cluster whose node image ships no
   driver is the case they are least sure about.
2. **Close the helm 4 obligation.** Everything in `test/e2e/reset.sh` has run under helm 4 except
   the FAILED-reset assertion — a failed teardown blocking Start, Retry and Discard. Match on that
   description, not on an assertion number; the numbers have moved twice. Costs one local
   `reset.sh` run against a helm 4 host. **This gates the first release, not any merge.**
3. **Release automation.** goreleaser + brew + an install script, macOS and Linux, no Windows.
   Waiting on item 2.
4. **`ValidateState` false-passes on simulated nodes.** Known and deferred; it is why the demo
   claims Prove rather than Validate.
5. **AKS**, unblocked by AICR v0.20.0. **Verification-screen polish** via `VerifyBundle`. GitOps
   export is deprioritised.
6. **Collapse the docs.** When the work is done, rewrite from current state and delete
   `docs/phase-*.md` rather than editing them.

### Known and deliberately not fixed

- **Uninstall is best-effort about completeness, never about destructiveness.** Reset removes helm
  releases and the named objects a chart tells helm to keep (see below). It deletes no namespaces
  and chases no orphans — it reports them and prints the command. Do not add code to chase them.
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
