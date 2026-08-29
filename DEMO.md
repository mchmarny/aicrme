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
3. Label a worker for the AICR snapshot agent.
4. Build the console binary.
5. Run it in the foreground, with the real (non-dry-run) install on. It opens your browser at a
   tokenized loopback URL and prints the same URL.

**Why the simulated GPU nodes are not optional:** with no GPU nodes there is no derivable
accelerator, so every intent/platform pair fails AICR's coverage post-condition and Recommend
cannot resolve *anything*. A plain Kind or KWOK cluster will not demo this product.

The console runs in your terminal. **Ctrl-C stops it; the cluster stays up** until
`make demo-down`. Re-running `make demo` against an existing cluster reuses it.

## Drive it

Open the URL. There is no password: the `?t=` token in the URL is exchanged once for a session
cookie that dies with the process.

The first screen is **Connect** — it lists your kubeconfig's contexts and arrives preselected on
`kind-aicrme-demo`. Connecting reports the cluster's server version, node count and UID, and the
`bash`/`jq`/`helm`/`kubectl` this machine resolved, so you can confirm what you are about to
install into and with what. Then the arc is five screens:

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

**5. Validate.** The console asks AICR's SDK whether what Apply just installed actually
reconciled — a `deployment`-phase check run across the cluster (operator health, expected
resources, driver version, `nvidia-smi`, and — on GKE — the GPU NIC networks). On the KWOK
cluster this demo stands up, it **skips, loudly**: the same `kwok-controller` detection that
labels Prove's placement simulated (below) stops this step before it calls AICR at all, because
the validator would otherwise land on KWOK's fake nodes and report every check passed having
executed nothing. The timeline states the skip reason in one line, and a skip is never rendered
as a pass — there is no green verdict on this path. On real hardware this step actually runs and
records a per-phase pass/fail verdict, shown on the Prove screen beside the placement claim.

**6. Prove.** The console applies its reference workload — a 2-pod gang, 8 `nvidia.com/gpu`
each, `schedulerName: kai-scheduler` — into its own `aicrme-prove` namespace and waits for the
gang to be placed. On a KWOK cluster that takes about two seconds.

The screen shows the placement decision itself, one line per gang member naming the node it was
bound to, and it labels the cluster **simulated** without apology — even though KWOK's fake nodes
report a healthy 32-of-32 GPU count, the same `kwok-controller` detection Validate uses (not the
GPU count) is what the label is keyed on: nothing here computed a result and the screen claims
none. What is real is that a GPU-aware scheduler admitted a gang and bound every member of it.

**7. The run stays active.** This is the one state the arc ends in that is not "finished": every
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
| **A workload that computes anything** | Phase 4, on real hardware. Prove places a gang; the containers never execute here (see below). |

## Running on a real cluster

There is nothing special to do. The binary runs on your machine and drives whatever context you
pick, so a real cluster is just a different row on the Connect screen:

```sh
make build
./bin/aicrme --context <your-context>
```

Nothing is installed into the cluster to make the console work — no Deployment, no
ClusterRoleBinding, no image to push or pull. It acts with your kubeconfig's credentials, and
holds them only while it runs. `make demo`'s Kind + KWOK setup exists because a *simulated*
cluster needs building; a real one you already have.

**One thing to watch on a managed cluster.** The snapshot agent Job tolerates `nvidia.com/gpu`
and nothing else, which covers GKE's `nvidia.com/gpu=present:NoSchedule` and the EKS/AKS
equivalents. A GPU pool carrying some *other* taint will strand it, and Discover then sits
Pending for its full ten-minute timeout. Set `AICRME_GPU_TOLERATIONS` to that taint —
`key=value:Effect`, comma-separated — and it reaches the node. The built-in toleration is
deliberately narrow rather than a blanket `Exists`: a blanket one also accepts KWOK's fake-node
taint, and KWOK reports success for pods it never runs, which would make Discover lie.

## Check on it / tear it down

**To repeat the demo on the same cluster**, use the console's own **Reset**. It stops the Prove
workload, waits until it is confirmed gone, and uninstalls the run's helm releases in reverse
install order. It takes two clicks — the second one is made against a list of exactly what will
be removed.

**Uninstall is best effort, and the remainder is yours.** Whoever applies a bundle owns the
cleanup of what it applied; that is the entire job of a CD tool, and here that deployer is a
bash script. So Reset does the part it can prove is safe, then tells you the rest with the
command to finish it.

Reset removes only what it can prove this run created, and names everything it leaves:

- **A release that already existed** at the same name and namespace before the run started.
  AICR's generated `install.sh` is `helm upgrade --install`, so such a release was *adopted*,
  not created, and uninstalling it would remove something you installed. The console records
  what was there before it installs anything, which is the only moment that answer exists.
- **Every namespace, always.** Reset deletes none of them. It lists each one it touched and
  says whether this run created it, and for the ones it did the console shows the
  `kubectl delete namespace …` to run. A namespace left standing costs you one command; a
  namespace deleted out from under something that was still using it cannot be undone.
- **CRDs.** `helm uninstall` does not remove them, and neither does Reset. They are
  cluster-scoped, shared, and removing them takes every custom resource on the cluster with
  them. Argo CD and Flux decline for the same reason.
- **Anything it could not check.** An RBAC denial or an unreachable API is not evidence that
  something is safe to delete, so the object stays and the run says why.

For a genuinely clean slate, AICR's own `tools/cleanup` goes further than this console does —
it removes AICR-owned CRDs and namespaces, with `--dry-run`, `--keep-crds` and `--exclude-ns`.
It is contributor tooling for testing and demos, which is exactly this situation, and it is the
right tool when you want everything gone rather than only what one run added.

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

**On KWOK, this workaround placed the gang within ten seconds:**

```sh
kubectl delete schedulingshard default              # the next reconcile rebuilds it
kubectl rollout restart deploy -n kai-scheduler     # no controller keeps a pre-reset cache
```

> **It does not work on real hardware. Measured 2026-08-26, GKE, 2× a3-megagpu-8g.** The second
> cycle installed 16/16 and Prove failed `0/2 members placed`. Both commands were run, all six
> kai-scheduler deployments rolled out cleanly, and the retried run failed **identically** — a
> fresh PodGroup UUID and the same pod-grouper loop (`already exists`, then `the object has been
> modified; please apply your changes to the latest version`).
>
> **The mechanism above is also not what happens there.** Every kai-scheduler pod was `Running`,
> `kai-scheduler-default` included, and the `scheduling.run.ai` CRDs had been created fresh
> minutes earlier. Nor was it leftover reservations: both GPU nodes reported all 8 GPUs
> allocatable, nothing held a GPU, and `kai-resource-reservation` was empty. Same symptom,
> different cause — and the real one is not yet understood.

**Rebuilding the cluster is the only path known to work on real hardware.** On KWOK
`make demo-down && make demo` does it; on a real cluster it means a new cluster. Either way it is
the safer choice if you are about to demo in front of someone.

This was measured, including an arm that ruled out "it just needed more time".

If a Reset does not finish, the console offers only Reset again: the run record is the only
inventory of what is still installed, so Start, Retry and Discard are all refused until the
cluster's state is known again.

**A second Reset is always accepted, but it cannot always finish.** If an uninstall was
interrupted part-way — a timeout, a crash, an admission webhook refusing a delete — helm leaves
that release in `uninstalling`, and it refuses to uninstall a release in that state. Retrying
achieves nothing, and the console will keep naming it with its error. Clear it by hand:

```sh
helm list -A --uninstalling            # find the wedged release
helm uninstall <name> -n <namespace>   # often succeeds once the blocker is gone
kubectl -n <namespace> delete secret -l name=<name>,owner=helm   # last resort: drop the release record
```

Measured 2026-08-23. This is the best-effort boundary in its sharpest form: the console reports
precisely what it could not remove, and removing it is yours.

**To remove the cluster entirely:**

```sh
make demo-status   # is the cluster up, and is a console running against it
make demo-down     # delete the cluster and its demo work directory
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

**`make demo` says the cluster already exists.** It reuses it. Run `make demo-down` first if
you want a clean start.

**The console refuses to start: "work directory is locked".** One console per work directory,
and a previous one is still running (or was SIGKILLed and left its lock). Stop the other one, or
point this one somewhere else with `--work-dir`.

**The console refuses to start: a missing executable.** It resolves `bash`, `helm` and `kubectl`
before touching a cluster, deliberately — finding that out at Apply, twenty minutes in with
releases already installed, is the failure this check exists to move to second zero.

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
