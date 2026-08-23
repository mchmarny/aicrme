# aicrme - Design Spec

**Date:** 2026-08-13
**Status:** Draft for review
**Author:** Mark Chmarny
**Repo:** `mchmarny/aicrme` (private)
**Scope:** Orthogonal project that consumes AICR as a pinned Go module dependency. It is not on
the AICR v1 roadmap. Nothing in this document changes `NVIDIA/aicr` except one optional
upstream contribution noted under Risks.

---

## Summary

A single Helm chart that turns a vanilla GPU cluster into a working AI platform through a
browser UI. Install the chart, get a URL and generated credentials, open it, and the console
discovers the cluster, recommends a validated AICR configuration, installs it while streaming
live cluster telemetry, then runs a reference workload that proves the cluster works.

The product is an **eval and demo accelerator**. It is not a production control plane, and the
security posture described below is only defensible under that framing.

---

## Goals

1. Shortest possible path from a raw GPU cluster to a workload producing a result.
2. Make the transformation *visible*. The user should see what is happening in their cluster
   continuously, not watch a spinner.
3. Stay honest about what is being installed. The bundle is shown, verified, downloadable, and
   exportable to GitOps, so a good demo converts into an adoption path.
4. Self-contained. No Argo CD, no Flux, no external services, no database.

## Non-goals

- Day-2 operations: upgrade, drift detection, reconciliation.
- Multi-user authentication, OIDC, or scoped RBAC.
- Multi-cluster management.
- In-UI editing of component sets, versions, namespaces, or values.
- Air-gapped operation, private registries, or registry mirroring. **Direct internet access to
  ghcr.io, nvcr.io, and the upstream Helm repositories is an assumed precondition.** A cluster
  that cannot reach them is out of scope, and the console should fail fast and say so rather
  than degrade.
- Production use of any kind.

---

## Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Product framing | Demo and eval accelerator | Removes day-2, upgrade, drift, and hardened-auth requirements. Disposable clusters. |
| Demo arc endpoint | Reference workload produces a result | "16 charts installed" is a claim about the tool. "64 GPUs online, 387 GB/s all-reduce" is a claim about the user's cluster. |
| Home | Private personal repo (`mchmarny/aicrme`) | Orthogonal to AICR, consuming it as a pinned Go module. Keeps AICR's v1 surface freeze and RBAC posture untouched, and keeps this off the v1 roadmap. Independent release cadence. |
| Target environments | EKS, GKE, AKS, Kind/KWOK | EKS and GKE are the best-validated overlays. AKS needs the ADR-015 pool projection. Kind/KWOK is the dev inner loop and the no-hardware demo. |
| Provenance | First-class: show, verify, export | Reuses `aicr bundle --attest` and `aicr bundle verify`, which already exist. This is AICR's actual differentiator. |
| Applier | Go orchestrator driving the bundle's own `install.sh` | Byte-identical to the downloadable artifact. No `helm.sh/helm/v3` dependency war with AICR's `k8s.io/*` pins. Clean per-component events. |
| Shell layout | Hybrid: focused wizard, expanding into a cockpit | Three decisions want calm. Fifteen minutes of telemetry wants every pixel. The expansion is a deliberate beat in the demo. |
| Cockpit hero | Component pipeline, not node table | The target user reasons in components and platforms, not infrastructure. Nodes are a compact rail. |
| Discover framing | Capability gaps, not inventory | The snapshot's payload is a gap list, and each gap maps one-to-one onto a component about to be installed. |
| User decisions | Exactly two: intent, and platform | Service, accelerator, OS, component set, versions and values are all derived by AICR. |

### Applier alternatives considered and rejected

**Vendor the Helm Go SDK.** Pure Go, no shelled binaries, direct access to helm action hooks.
Rejected because `helm.sh/helm/v3` carries a large dependency tree with its own `k8s.io/*`
pins that will fight AICR's, and because reconstructing the helm CLI's flag behavior
(`--force-conflicts` on Helm 4 but not 3, `--create-namespace`, per-component wait args)
reintroduces exactly the artifact drift the chosen approach eliminates. Remains available as a
later swap without changing the UI contract.

**Integrated Helmfile.** `aicr bundle --deployer helmfile` already exists, and `needs:` gives
declarative ordering. Rejected because it adds a third binary in exchange for a coarser
progress signal - helmfile shells to helm anyway, and its output is harder to structure than
driving components directly. The ordering it provides is already encoded in AICR's numbered
component directories.

**GitOps handoff via Argo CD.** Would give progress, retry, and rollback for free. Rejected on
the explicit self-containment requirement, and because installing Argo contaminates the
"vanilla cluster" premise of the demo.

---

## Architecture

One Go binary, one image, one chart. No sidecars, no operator, no CRDs, no database.

### Units

| Unit | Responsibility | Depends on | Tested with |
|---|---|---|---|
| `engine` | Run state machine: Discover, Recommend, Bundle, Apply, Validate, Prove | `Step` interface | fake steps, no cluster |
| `steps` | One `Step` implementation per phase, each `Run(ctx, *Run, emit func(Event)) error` | AICR packages | fake AICR clients per step |
| `applier` | Executes a bundle directory: ordered `install.sh`, retry/backoff, hook-Job cleanup, preflight | `Exec` interface | fake exec, golden command logs |
| `observer` | Shared informers over Pods, Events, Nodes, DaemonSets, Deployments, producing typed events | `kubernetes.Interface` | `client-go` fake clientset |
| `bus` | Fan-out to SSE subscribers plus a ring buffer for replay | nothing | pure unit tests |
| `api` | HTTP handlers over `engine` and `bus`. No business logic. | `engine`, `bus` | `httptest` |
| `web` | React SPA, built at CI time, embedded via `embed.FS` | nothing | component tests |

The `api` / `web` split mirrors AICR's own `pkg/cli` and `pkg/server` discipline: presentation
layers carry no business logic.

### Repo layout

```
mchmarny/aicrme
  cmd/aicrme/main.go
  internal/engine/          run state machine
  internal/steps/           discover, recommend, bundle, apply, validate, prove
  internal/applier/         helm/kubectl orchestration, retry, hook cleanup, preflight
  internal/observer/        informers to events
  internal/bus/             SSE fan-out and ring buffer
  internal/api/             HTTP handlers (thin)
  web/                      Vite + React + Tailwind, built to embed.FS
  charts/aicrme/            the single chart
```

### AICR integration points

```
Discover   -> pkg/snapshotter            deploys the existing privileged agent Job
Recommend  -> pkg/client/v1 Recipe/Query
Bundle     -> pkg/client/v1 Bundle       with --attest for the verify screen
Apply      -> internal/applier over the generated bundle directories
Validate   -> pkg/chainsaw + recipes/checks/<name>   per component and at the end
Prove      -> demos/workloads/{training,inference}
```

The console imports `github.com/NVIDIA/aicr` as a pinned Go module. It also adopts AICR's
`pkg/errors` for structured errors with codes, so error handling is consistent across the two
codebases.

### Data flow

One direction, deliberately boring:

- Browser opens `GET /api/events` (SSE, auto-reconnect, replays from the ring buffer so a
  late-joining or reconnecting tab sees the whole run).
- `engine` and `observer` publish into `bus` independently of each other.
- Mutations are ordinary `POST /api/runs` and `POST /api/runs/{id}/approve`.

SSE rather than WebSocket, because it survives `kubectl port-forward` and corporate proxies
without ceremony, and the traffic is server-to-client only.

### State

Single replica. Run state in memory, checkpointed to a ConfigMap so a pod restart mid-demo does
not wipe the timeline. Final artifacts (`snapshot.yaml`, `recipe.yaml`, bundle tarball) are
written to ConfigMaps and a Secret so they are `kubectl get`-able even if the browser dies -
which doubles as a fallback demo path.

---

## User experience

Six phases in a hybrid shell. Phases 1 to 3 render as a calm centered wizard because they
contain decisions. The moment Apply starts, the layout expands into the cockpit for phases 4
to 6.

### 1. Discover

Runs the AICR snapshot automatically on first load. Opens with a capability statement, not an
inventory:

> **This is an EKS cluster with 64 H100 GPUs.**
> 8 x p5.48xlarge, H100 SXM 80GB, EFA fabric, Kubernetes 1.33, Ubuntu 22.04

Then the gap list, which is the snapshot's real payload:

- No GPU driver installed, the kernel does not see the devices
- No device plugin, Kubernetes cannot schedule `nvidia.com/gpu`
- No GPU-aware scheduler, no gang scheduling for multi-node jobs
- No EFA plugin, multi-node collectives would fall back to TCP
- No GPU metrics, utilization is invisible

Landing on the number the finale pays off:

> **0 of 64 GPUs are usable by a workload today.**

Node-by-node detail, driver versions, taints, labels, and the raw snapshot YAML live behind a
fold. Every gap corresponds one-to-one to a component about to be installed, so this screen
pre-explains the next one.

### 2. Recommend

Asks the only thing a snapshot cannot know, then the only thing that is genuinely a preference:

1. **What is this cluster for?** Training or Inference.
2. **How do you want to submit work?** Kubeflow Trainer, Slurm (Slinky), Run:ai, or just the
   runtime. Each described by what the user types to use it, not by what it is.

Platform options are filtered to those with an overlay matching this cluster's coordinates.
Everything else - service, accelerator, OS, component set, versions, values - is derived by the
AICR recipe engine. The resulting component list is present and reviewable but folded:

> **16 components**, every version pinned - gpu-operator v26.3.3,
> kai-scheduler v0.14.1, kubeflow-trainer 2.2.0, +13

**"Signed" was removed from this line, and from the two screens that rendered it, on
2026-08-23.** Nothing backed it. `aicr.ComponentRef` — and `recipe.ComponentRef` beneath it —
carry name, kind, version, source, chart and namespace, with no digest and no signature field,
so the console has no per-component signing evidence to render. `steps.Bundle` passes no
`Attester` to `MakeBundle`, which AICR documents as selecting the no-op attester, so the
generated bundle is not attested either. AICR's one attestation path,
`Client.EmitRecipeEvidence`, builds its predicate from a completed `ValidateState` run — the
step scoped out on 2026-08-18. Backing the claim would mean reinstating Validate AND standing
up keyless signing, so the honest move on a screen whose whole job is pre-approval disclosure
was to make the claim smaller. **Pinned is true and stays:** versions come from the pinned
embedded catalog, and `assertMatchesApproved` fails the run if a re-resolve drifts from what
the operator approved.

Footnoted with the validation provenance: this recipe is exercised nightly in AICR's test
matrix for these coordinates.

### 3. Review and verify

The generated bundle, per component: chart, version, namespace, source repository, and cosign
/ SLSA verification status. Two actions: **Download bundle** and **Export to Argo CD / Flux**.

Both actions produce files for the user to take away. "Export to Argo CD / Flux" regenerates
the bundle through AICR's `argocd`, `argocd-helm`, or `flux` deployer and downloads it. It does
not install Argo CD or Flux, and the console never depends on either being present - the
self-containment requirement holds.

This is the screen that converts a demo into an adoption path, and it is the reason the applier
was chosen to run the bundle's own scripts - what is shown here is byte-identical to what gets
applied and to what the user downloads.

### 4. Apply (cockpit)

Layout expands. Hero is the ordered install pipeline; nodes and events are a right rail.

The pipeline shows each component with status, version, elapsed time, and for the active
component a live sub-status sourced from the observer, for example
`waiting on rollout: nvidia-driver-daemonset 3/8 ready`.

**Contextual slow-step explanations.** A static map keyed by component surfaces an inline
callout *before* a known multi-minute stall, for example:

> **Why this takes a while:** the driver DaemonSet compiles the kernel module against
> Ubuntu 22.04 / 5.15.0-1071-aws on each node, then loads it. Typically 4 to 7 minutes per
> node, running 3 at a time.

This is not decoration. Every GPU cluster install stalls while the kernel module builds, and an
unexplained stall is precisely where a demo audience concludes the tool is broken. Naming it
before it happens converts the worst moment into a credibility moment.

**Failure state.** Component failures on real clusters are common, not exceptional. When one
fails the cockpit shows the failing component, the helm error, the relevant pod events and
describe output, and a Retry button. `deploy.sh`'s existing diagnostic dumping is the model for
what to capture.

### 5. Validate

Runs the per-component chainsaw checks from `recipes/checks/` in-process via `pkg/chainsaw`,
plus the final recipe validation. Green checks land next to their components in the pipeline.

**Deferred as of 2026-08-18 — both halves of that description turned out to be unbuildable
today, and the finding is measured rather than argued.** `ValidateState`'s orchestrator Job Pod
tolerates every taint, so on any cluster carrying KWOK's simulated GPU nodes it lands on one,
KWOK fakes `ExitCode:0` without starting the container, and every check reports `passed` —
including for components that are not installed. Those simulated nodes are a *prerequisite* of
the demo path, not an accident of it: without GPU nodes there is no derivable accelerator and
recipe resolution fails, so every cluster this console can demo on triggers it. Separately,
"next to their components" has no data to stand on: `ctrf.Builder` hardcodes each result's
`Suite` to the phase name, so nothing in the output identifies a component.

Full evidence, the reliable non-execution signal, and the reasons this is not the dry-run
ceiling repeating are in `docs/phase-2-handoff.md` under "Constraints Phase 3 inherits".
Revisit when AICR's orchestrator scheduling changes — not before, and do not route around it
in this console.

### 6. Prove

| Path | Close |
|---|---|
| Training + Kubeflow | `TrainJob` running NCCL all-reduce across 2 nodes / 16 GPUs, live busbw GB/s against theoretical line rate |
| Inference + Dynamo/NIM | Model served, endpoint live, and a chat box in the UI the audience can type into |
| Kind/KWOK | No GPUs, so no throughput claim. Shows the allocation decision instead: kai-scheduler gang-placing a 2x8 job. Labeled "simulated cluster, no GPU hardware" without apology. **Two corrections from building it (Phase 3):** DRA is not part of this — the workload requests scalar `nvidia.com/gpu` and the simulated nodes publish no `ResourceSlices` — and the two pods routinely land on the *same* simulated node, because KWOK completes each one in the second it binds it and releases its resources before the next is scheduled. |

The payoff is the callback to Discover: **0 of 64 usable, to 64 of 64 at 387 GB/s.**

Source material is `demos/workloads/{training,inference}`, which already exists.

**The workload is left running.** It does not self-terminate when the demo narration ends,
because the most valuable minutes are the ones after: the audience typing their own prompts
into the inference chat box, or watching the training job's throughput while asking questions.
The Prove screen therefore stays live, keeps streaming workload telemetry, and carries an
explicit **Stop workload** button as the only way it ends.

This makes the workload a long-lived resource the console owns, with two consequences the
implementation must handle: the run state machine has a terminal-but-active state rather than a
terminal-and-done one, and Reset must tear the workload down before it uninstalls the
components underneath it.

### Reset

**Reset is never automatic.** It is always initiated by the operator and always passes an
explicit confirmation before anything is uninstalled — the same shape as Apply's confirm gate,
which the console cannot pass without a recorded decision, and Reset is strictly more
destructive than Apply. Nothing in the product may trigger it on the operator's behalf: not a
failed run, not a restart, not a timeout, not a recovered run being discarded. A run that ends
badly leaves the cluster exactly as it is and says so.

Teardown button that first stops any running Prove workload, then runs `helm uninstall` in
reverse order plus namespace cleanup, so the demo is repeatable on the same cluster without
rebuilding it. Tearing down components while a GPU workload still holds devices is the obvious
failure mode here, so workload shutdown is a hard precondition rather than a parallel step.

**Delivered in Phase 5.** `engine.Reset` requires the confirmed workload stop before it
uninstalls anything — via `prove.Client.EnsureAbsent`, the delete-then-wait-for-absence
sequence factored out of `Engine.Stop` so Reset can require the identical guarantee without
going through Stop's own state guard. Stop is not best-effort: it deletes the workload with
foreground propagation and does not return until the pods are actually gone. A Reset that
proceeded on a failed stop would be uninstalling `kai-scheduler` and the GPU operator out from
under a gang that is still holding devices, so a failure there aborts the teardown before a
single release is touched and says so.

**Ownership is snapshot-based, because `helm upgrade --install` adopts.** This is the finding
that shaped the whole slice. AICR's generated `install.sh` runs `helm upgrade --install`, so a
release a human already had at the same (name, namespace) is upgraded rather than rejected,
prints a deploy header like any other action, and lands in `run.Components` completely
indistinguishable from one this run created. `--create-namespace` does the same for namespaces.
Neither leaves a marker behind, so there is no way to ask the cluster afterward what was
already there.

The answer therefore has to be recorded BEFORE Apply runs — that is the only moment it exists.
`internal/steps.snapshotOwnership` records, per recipe namespace, the releases already present
(`helm list --all`, so a release left failed by an earlier attempt still counts as pre-existing)
and whether the namespace existed. Reset uninstalls only what is absent from that snapshot, and
anything it cannot account for is skipped and named rather than removed on a guess. The
snapshot never fails the install: a namespace it cannot read is recorded as unprovable, which
makes every release in it off limits to Reset — the fail-closed direction.

**Reset removes releases and reports namespaces. It deletes no namespace, ever** (revised
2026-08-23). Whoever applies a bundle owns the cleanup of what it applied — that is the whole
job of a CD tool, and this console is only the bash deployer — so uninstall is a best-effort
concept and the remainder is the operator's. Naming what is left, with the command to remove
it, is the answer; more code to chase it is not.

That framing is deliberately asymmetric. Best effort applies to **completeness**, never to
**destructiveness**: a namespace left standing is one `kubectl delete namespace` for the
operator, while one deleted out from under something is unrecoverable. The pre-Apply snapshot
survives because it only ever buys the second property — it never makes a teardown more
complete, only more careful.

What this replaced: namespace deletion gated on an emptiness check that walked the API server's
discovery document listing every namespaced kind, plus two UID fields recording the namespace's
identity before Apply and after, so a deletion could refuse a namespace destroyed and recreated
at the same name in between. It was correct and it rarely fired — on the real measurement it
declined 8 of 10 namespaces, because operators leave runtime-created Leases behind that
`helm uninstall` never removes. Deleting it took the discovery walk, both UID fields, the
post-Apply confirmation pass, and the discovery and dynamic clients from `main.go` with it.

Still advertised as best-effort, and the boundary is stated rather than implied: CRDs are
cluster-scoped and shared, `helm uninstall` does not remove them and neither does Reset;
finalizers routinely leave residue, which is why `deploy.sh` already carries stale-webhook and
terminating-namespace preflight. A fresh cluster remains the guaranteed path. What Reset
guarantees is narrower and more useful: it never removes something it cannot prove this run
created.

---

## Live feedback design

The dynamic feel does not come from the applier. It comes from a second, independent stream.

The `observer` runs shared informers from the moment the pod starts, over Pods, Events, Nodes,
DaemonSets and Deployments across the bundle's namespaces. It converts raw watch events into
typed, human-phrased console events. Examples of what this produces that the applier alone
cannot:

- `nvidia-driver-daemonset 2/8 nodes ready`
- `Warning FailedScheduling: 0/8 nodes match nvidia.com/gpu` - annotated as expected, resolves
  when drivers land
- `ip-10-0-2-7 pulling nvcr.io/nvidia/driver:580.65.06, 1.4/2.1 GB`
- `nvidia.com/gpu` allocatable per node moving 0 to 8

Both streams merge into one ordered timeline in `bus`. Warnings are surfaced rather than
buried, with known-benign ones annotated so they inform instead of alarm.

---

## Packaging, auth, exposure

### Install

```
helm install aicrme oci://ghcr.io/mchmarny/aicrme/charts/aicrme \
  -n aicrme --create-namespace
```

Because the source repo is private, the GHCR chart and image packages must be explicitly set to
public for this command to work without a pull secret. GHCR package visibility is independent
of repo visibility, so this is a one-time setting rather than a blocker - but it is the first
thing that will break for anyone other than the author.

Chart contents: Deployment (1 replica), ServiceAccount, ClusterRoleBinding, ClusterIP Service,
a Secret holding a `randAlphaNum 24` password guarded by a `lookup` so `helm upgrade` does not
rotate it, a ConfigMap for configuration, and NOTES.txt.

### URL and credentials

NOTES.txt prints the port-forward one-liner, the resulting `http://localhost:8080`, the
username, and the generated password.

`--set service.type=LoadBalancer` is available for the "it prints a real URL" version, with a
self-signed certificate generated at install and a loud warning in NOTES.txt. It is not the
default, for the reason below.

### Auth

Single user. Session cookie, HttpOnly, SameSite=Strict, Secure when TLS is present.
Constant-time password comparison. Login rate-limited. Session expiry 8 hours. No user
management, no OIDC.

### Security posture

The console installs gpu-operator, cert-manager, DRA drivers, CRDs, and privileged DaemonSets,
and creates namespaces. That is cluster-admin, and there is no honest way to narrow it - any
hand-enumerated role breaks the first time a recipe gains a component, and a narrower-looking
role that must be widened on every recipe change is security theater.

So: grant `cluster-admin`, state it plainly in NOTES.txt and in the README, and treat
**demo and eval clusters only** as a hard product boundary.

A cluster-admin console fronted by a public LoadBalancer and one password is a cluster-takeover
surface. This is the single strongest reason the product must stay labeled a demo tool and
never quietly drift into production use, and the reason ClusterIP plus port-forward is the
default.

The snapshot agent additionally runs privileged pods on GPU nodes in order to read `nvidia-smi`
and PCIe topology. This is existing AICR behavior, not new exposure, but it is part of the same
disclosure.

---

## Environment handling

**EKS and GKE** are the primary real-hardware targets, being the best-validated overlays in
AICR's matrix. GKE's COS path differs materially from EKS - drivers are preinstalled, and the
`gke-nccl-tcpxo` checks apply - which the recipe engine already handles.

**AKS** gets one extra step, shown only when the snapshot detects AKS: a pre-filled
`az aks nodepool list -g <rg> --cluster-name <name> -o json` command and a textarea to paste
the result, feeding the `--aks-gpu-pools` projection that ADR-015's `gpuStack` profile requires.
This avoids embedding Azure credentials in the console, at the cost of one honest manual step.

**Kind and KWOK** carry no GPUs. This is the development inner loop and the CI target, and it
is also a legitimate no-hardware demo path via the simulated finale described in phase 6.

---

## Testing strategy

Mirrors AICR's conventions: table-driven tests, `-race`, structured errors, an 80% coverage
floor.

| Unit | Approach |
|---|---|
| `engine` | Fake `Step` implementations. Full state machine coverage with no cluster. |
| `applier` | Fake `Exec`. Golden files asserting the exact command sequence, env, and retry behavior per bundle fixture. |
| `observer` | `client-go` fake clientset driving synthetic watch events, asserting emitted console events. |
| `bus` | Pure unit tests: fan-out, ring buffer replay, subscriber churn. |
| `api` | `httptest`, including SSE stream framing and reconnect replay. |
| `web` | Component tests against recorded event streams, so UI states are reproducible without a cluster. |
| End to end | Kind plus KWOK in CI, exercising the whole flow through simulated mode on every PR. |

Recorded event streams are worth calling out: capturing a real EKS run's event log once lets
the entire UI - including failure states and slow-step callouts - be developed and tested
without touching GPU hardware.

---

## Phases

Each phase is independently demoable.

| Phase | Deliverable |
|---|---|
| 0 | Skeleton: repo, CI, chart, auth, SSE bus, embedded SPA shell. Installs on Kind and does nothing. |
| 1 | Discover and Recommend against Kind/KWOK. Both wizard screens real. |
| 2 | Applier, cockpit, observer. The bulk of the work. |
| 3 | Prove, simulated on Kind. Full arc end to end with no hardware. **Validate was scoped out on 2026-08-18** — measured, not assumed; see the Validate section below. **Delivered:** a real gang, placed by kai-scheduler on simulated GPU nodes, with the run ending terminal-but-active and Stop as its only exit (`test/e2e/prove.sh`). What it does *not* deliver is a workload that computes anything: KWOK completes every pod in the second it binds it, so the container never runs — see the Prove section's own note and `DEMO.md`. |
| 4 | Real hardware: EKS, then GKE. Real finale, real timings, slow-step map calibrated. |
| 5 | AKS, reset, export to GitOps, verification screen polish. |

---

## Risks

1. **SDK surface. Largely closed upstream as of v0.19.0.** This risk was written when
   `pkg/client/v1` covered recipe, bundle, query, evidence and health but neither snapshot nor
   validate, forcing direct imports of `pkg/snapshotter` and `pkg/validator` from outside the
   freeze. **Both are now on the facade** — `Client.CollectSnapshot` and `Client.ValidateState` —
   and this console uses the first through it. `pkg/validator` is no longer imported at all.
   `pkg/snapshotter` still is, but only to decode the `Snapshot` type; the agent runs entirely
   through the facade. The mitigation landed upstream without this project having to ask for it.

   **What remains:** the facade's `AgentConfig` is a hand-written mirror and omits
   `AKSGPUPoolsPath`, which is why AKS is deferred rather than built around it — see
   `docs/aicr-upstream-asks.md`. Pinning the module version and bumping deliberately
   (now automated: `renovate.json`) stays the standing mitigation.

2. **Blast radius.** Cluster-admin, as above. Mitigated by framing, defaults, and disclosure,
   not by technical control.

3. **Apply duration.** 10 to 20 minutes on real hardware, most of it driver compilation. The
   cockpit and slow-step callouts are the mitigation. Image pre-pull is worth investigating in
   phase 4.

4. **Mid-apply failures are normal.** The failure state is not an edge case and should be
   designed in phase 2, not retrofitted.

5. **Two-repo drift.** Recipe data is embedded in the aicr module, so a console release pins a
   recipe catalog snapshot. **Addressed 2026-08-23 by `renovate.json`**, which groups all three
   AICR pins — `go.mod`, `.settings.yaml`, and the snapshot agent image constant — into one
   weekly PR carrying the bump checklist in its body. Still requires the Renovate GitHub App to
   be installed on the repository; see `docs/phase-2-handoff.md`.

6. **Image size.** The console image carries helm, kubectl, and the built SPA, and it is the
   first thing the cluster pulls. Keep it lean; it is on the critical path of the first
   impression.

---

## Open questions

1. **Ownership and budget.** Unresolved. The project lives in `mchmarny/aicrme` for now, which
   is fine for the first demo cycle but leaves three things undecided: who maintains it once it
   is being shown to customers, whether it eventually moves to an NVIDIA org (which would
   trigger the OSRB path), and whether it gets a release and CI budget of its own.

2. **Upstreaming `Snapshot()` and `Validate()` into `pkg/client/v1`.** Risk 1 below is the main
   structural weakness, and the clean fix is a small AICR PR. Since this project is explicitly
   orthogonal to the AICR roadmap, that PR has to be justified on AICR's own terms - which it
   can be, as it serves the integrator persona in ROADMAP section 3. Worth deciding early,
   because the alternative is pinning an aicr version and absorbing breakage on every bump.

3. **Whether the demo needs a scripted narration mode.** Not a v0.1 question, but the Prove
   screen staying live and interactive suggests a presenter might eventually want speaker
   notes, a reset-to-checkpoint, or a "replay the last run" mode driven off the recorded event
   streams already proposed in Testing.
