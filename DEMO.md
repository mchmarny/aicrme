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

**5. Done.** The arc currently ends here, at a completed Apply.

## Things worth trying

- **Kill the console mid-Apply** (`kubectl -n aicrme delete pod -l app.kubernetes.io/name=aicrme`).
  It comes back knowing what happened rather than losing the run — restart recovery, Phase 2b-ii.
- **Cancel a run** and watch it shut down gracefully rather than abandoning work.
- **Retry a failed run** — it resumes from the step that failed, and every component's
  `install.sh` is `helm upgrade --install`, so already-installed components are no-ops.

## What is not built yet

| | |
|---|---|
| **Validate** | Deferred on measurement, not preference — AICR's `ValidateState` reports `passed` for checks that never executed on any cluster with simulated GPU nodes. See `docs/phase-2-handoff.md`. |
| **Prove** | Phase 3, in design. This is why the arc ends at Apply rather than at a workload producing a result. |
| **Reset** | Phase 5. Use `make demo-down` to tear the cluster down. |

## Check on it / tear it down

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

Relatedly, the fake nodes advertise scalar `nvidia.com/gpu` capacity only and publish **no DRA
`ResourceSlices`** (verified 2026-08-19: `kubectl get resourceslices` returns nothing).
