# Running the demo

How to stand up `aicrme` on this machine and drive the whole arc in a browser.

**Local only for now.** Real-cluster and hardware demos land with Phase 4; this document grows
with them.

Everything below was executed on 2026-08-19, not written from intent. Where a number appears
(node counts, timings), it was observed.

---

## What you need

| Tool | Notes |
|---|---|
| Docker | running, with ~8GB available to it |
| `kind` | creates the local cluster |
| `kubectl` | |
| `helm` | |
| Go | version from `.go-version` |
| Node | for the SPA build |

Internet access to `ghcr.io`, `nvcr.io`, and the upstream Helm repositories is required. There
is no air-gapped path, by design.

## Bring it up

```sh
make demo
```

Takes about 5 minutes the first time. It will:

1. Create a Kind cluster — control-plane + 3 workers.
2. Install KWOK and apply AICR's **simulated H100 nodes** (2 system + 4 × `p5.48xlarge`).
3. Build the production console image and load it into the cluster.
4. Install the chart.
5. Pin the AICR snapshot agent to a labelled worker and turn the real (non-dry-run) install on.
6. Port-forward and print the URL, username, and generated password.

**Why the simulated GPU nodes are not optional:** with no GPU nodes there is no derivable
accelerator, so every intent/platform pair fails AICR's coverage post-condition and Recommend
cannot resolve *anything*. A plain Kind or KWOK cluster will not demo this product.

When it finishes you get:

```
  URL       http://localhost:8080
  username  admin
  password  <generated>
```

Re-running `make demo` against an existing cluster reuses it and upgrades the console in place.

## Drive it

Open the URL and log in. The arc is five screens:

**1. Discover.** The console runs AICR's snapshot agent as a Job on the cluster, reads node
topology, and reports **capability gaps** — what the cluster cannot do yet — rather than an
inventory. Takes about 10 seconds against the simulated nodes.

**2. Recommend.** Exactly two questions: **intent** and **platform**. Pick `training` and
`kubeflow` — that pair is what every test and this document exercise. Everything else (service,
accelerator, OS, component set, versions) is derived by AICR, not asked. You get a resolved,
version-pinned recipe of **13 components**.

Other pairs that resolve against the simulated nodes: `training/slurm`, `training/any`,
`inference/dynamo`, `inference/any`. The remaining seven fail because no catalog overlay
combines them, which is a real answer, not a bug.

**3. Review.** The bundle is shown, downloadable, and carries its attestation.

**4. Apply.** A confirm gate you cannot pass without an explicit decision, then a **real**
`helm upgrade --install` of all **14 deployment actions** inside the production image.

> 13 components, 14 actions — `kubeflow-trainer` contributes both an upstream chart and a
> `-post` local chart. Rows are actions.

Roughly 5 minutes. Watch the cockpit while it runs — that is the part worth watching:

- a live per-component pipeline, each action moving through its states
- **per-row cluster conditions**: pods stuck pulling images, crash loops, scheduling failures,
  attached to whichever action was installing when they appeared
- conditions arise, supersede one another, and clear as the cluster converges

The condition copy reads *"cluster activity while `<action>` installs"* and never says the
action **caused** it. That is deliberate: `deploy.sh` keeps converging after it exits, so the
correlation is temporal and the console does not claim more than it knows.

**5. Prove.** The console applies its reference workload — a 2-pod gang, 8 `nvidia.com/gpu`
each, `schedulerName: kai-scheduler` — into its own `aicrme-prove` namespace and waits for the
gang to be placed. On a KWOK cluster that takes about two seconds.

The screen shows the placement decision itself, one line per gang member naming the node it was
bound to, and it labels the cluster **simulated, no GPU hardware** without apology: nothing here
computed a result and the screen claims none. What is real is that a GPU-aware scheduler
admitted a gang and bound every member of it.

**6. The run stays active.** This is the one state the arc ends in that is not "finished": every
step is done and the workload is deliberately still there. **Stop workload** is the only way out
— Discard is refused while a workload is running, and Retry only applies to a failed run. Stop
deletes the workload and waits until its pods are actually gone before closing the run.

## Things worth trying

- **Kill the console mid-Apply** (`kubectl -n aicrme delete pod -l app.kubernetes.io/name=aicrme`).
  It comes back knowing what happened rather than losing the run — restart recovery, Phase 2b-ii.
- **Kill the console while the workload is running.** It comes back on the Prove screen with the
  run still active and Stop still working: the record is recovered from its ConfigMap and then
  reconciled against what is actually in the cluster. Delete the record instead
  (`kubectl -n aicrme delete cm aicrme-runs`) and restart, and the console *adopts* the workload
  it finds — it will never silently delete something it did not start.
- **Cancel a run** and watch it shut down gracefully rather than abandoning work.
- **Retry a failed run** — it resumes from the step that failed, and every component's
  `install.sh` is `helm upgrade --install`, so already-installed components are no-ops.
- **Reset the run** — tear down exactly what this run installed, on the same cluster, so the
  next demo does not need a rebuilt one. This replaces `make demo-down` for a repeat demo: the
  cluster survives, only the run's own footprint goes. Read the caveat below first — a repeat
  demo reinstalls cleanly but does **not** currently get as far as a placed gang.

## What is not built yet

| | |
|---|---|
| **Validate** | Deferred on measurement, not preference — AICR's `ValidateState` reports `passed` for checks that never executed on any cluster with simulated GPU nodes. See `docs/phase-2-handoff.md`. |
| **A workload that computes anything** | Phase 4, on real hardware. Prove places a gang; the containers never execute here (see below). |

## Check on it / tear it down

**To repeat the demo on the same cluster**, use the console's own **Reset**. It stops the Prove
workload, waits until it is confirmed gone, uninstalls the run's helm releases in reverse
install order, and deletes the namespaces the run created and left empty. It takes two clicks —
the second one is made against a list of exactly what will be removed.

Reset removes only what it can prove this run created, and names everything it leaves:

- **A release that already existed** at the same name and namespace before the run started.
  AICR's generated `install.sh` is `helm upgrade --install`, so such a release was *adopted*,
  not created, and uninstalling it would remove something you installed. The console records
  what was there before it installs anything, which is the only moment that answer exists.
- **A namespace it did not create**, or that still holds anything at all — including a Secret,
  a ConfigMap, an RBAC rule or a custom resource. Emptiness is established from the API
  server's own list of namespaced kinds, not a fixed list of workload types.
- **CRDs.** `helm uninstall` does not remove them, and neither does Reset. They are
  cluster-scoped, shared, and removing them takes every custom resource on the cluster with
  them.
- **Anything it could not check.** An RBAC denial or an unreachable API is not evidence that
  something is safe to delete, so the object stays and the run says why.

In practice this means **most namespaces survive a Reset**, and that is not a bug. Measured on
the KWOK demo cluster: all 13 releases were removed, and 8 of 10 namespaces were kept — four
because an operator had left a leader-election `Lease` behind, one for a webhook-hook `Secret`,
one for a `Deployment`, and two because they existed before the run. Those objects are created
at runtime rather than by the chart, so `helm uninstall` does not remove them, and a namespace
holding one is not empty. The releases are what matter for a repeat demo; the empty-ish
namespaces are harmless and `kubectl delete ns` clears them if you want them gone.

**A repeat demo reinstalls, but its gang does not place — known, measured, with a one-line
workaround.** On the same KWOK cluster, the run after a Reset installed all 14 components
cleanly and then failed in Prove: the gang did not place inside the 3-minute budget, where a
first install places in about two seconds.

The cause is the runtime-object blind spot. kai-scheduler's Deployments are materialized by its
operator from custom resources rather than templated by the chart, so `helm uninstall` leaves
them; the next install recreates most of them but *not* `kai-scheduler-default`, because the
`SchedulingShard` that owns it survived and the operator saw its desired state already met. The
second cycle therefore runs a scheduler from the first against controllers from the second, its
pod-grouper cannot read back the PodGroup it just created, the scheduler sees zero PodGroups,
and the gang waits forever.

**The workaround, measured to place the gang within ten seconds:**

```sh
kubectl delete schedulingshard default              # the next reconcile rebuilds it
kubectl rollout restart deploy -n kai-scheduler     # no controller keeps a pre-reset cache
```

Then Retry the run. This is a restart, not a deletion of anything cluster-scoped — no CRD is
removed. Rebuilding the cluster (`make demo-down && make demo`) also works and is the safer
choice if you are about to demo in front of someone.

Full measurements, including the arm that ruled out "it just needed more time", are in
`docs/spikes/2026-08-23-kai-scheduler-reset-cycle.md`.

If a Reset does not finish, the console offers only Reset again: the run record is the only
inventory of what is still installed, so Start, Retry and Discard are all refused until the
cluster's state is known again.

**To remove the cluster entirely:**

```sh
make demo-status   # is it running, what is the URL and password
make demo-down     # delete the cluster and stop the port-forward
```

## When it goes wrong

**Discover hangs for ~10 minutes then fails with a scheduling error.** The snapshot agent could
not be placed. `make demo` pins it to a labelled worker for a specific reason: Kind leaves a
*single-node* cluster's control-plane untainted but taints it as soon as any worker exists, so
a control-plane selector silently stops working on a multi-node cluster.

**Apply drags far past 5 minutes.** Usually memory. The install is heavy and worker count is
the scheduling budget, not just memory — Kind reports each node's allocatable CPU as the
*host's* core count. Cutting the cluster to one worker took a real install from ~5 minutes to
over 40 without completing. Give Docker more memory rather than fewer nodes.

**`make demo` says the cluster already exists.** It reuses and upgrades in place. Run
`make demo-down` first if you want a clean start.

## What this demo cannot show you

The simulated GPU nodes are KWOK fakes: **they never run containers.** Pods "schedule" onto them
and their status is synthesized. So this environment proves the install chain, the recipe
resolution, and the telemetry — and it cannot prove anything that requires a container to
actually execute on a GPU. Those claims wait for real hardware in Phase 4.

Three specific consequences for the Prove step, measured on this cluster rather than assumed:

- **The workload body never runs.** KWOK marks a gang pod `Succeeded` in the *same second* it
  binds it, without starting the container — observed at 10:28:39/40 with the Job reporting
  `completionTime` 10:28:40. So `echo placement proven` is never printed by anything, and the
  container image is never even pulled.
- **The gang never holds its GPUs at once.** Because each member completes instantly, its
  resources are released before the next is bound — which is why both members routinely land on
  the *same* simulated node here. That is normal on this substrate and is not a scheduling fault;
  a real cluster, where the pods keep running, cannot do it.
- **DRA is entirely unexercised.** The workload requests scalar `nvidia.com/gpu`, and the fake
  nodes advertise scalar capacity only, publishing **no DRA `ResourceSlices`** (verified
  2026-08-19: `kubectl get resourceslices` returns nothing). The DRA driver the recipe installs
  is therefore never asked to bind a device by anything the console runs.

What this environment *does* prove about Prove: a GPU-aware scheduler admitted a gang, evaluated
it as a group, and bound every member of it to a node advertising GPUs. `test/e2e/prove.sh`
asserts exactly that and no more.
