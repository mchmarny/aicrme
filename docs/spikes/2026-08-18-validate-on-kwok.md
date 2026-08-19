# Spike: `aicr.Client.ValidateState` on KWOK — what actually happens

Throwaway spike. Not committed to the repo (this file lives outside
`aicrme` on purpose — `.superpowers/` is not gitignored in this repo, see
"How this was run" below). Program: `/tmp/claude-502/aicr-spike/main.go`,
built against `github.com/NVIDIA/aicr v0.19.0` from the module cache, run
against the pre-existing `aicr-kwok-test` Kind cluster with AICR's
simulated H100 KWOK nodes applied via `test/e2e/lib.sh`'s
`e2e_apply_kwok_nodes`. Raw artifacts (snapshot, recipe, per-phase CTRF
JSON, full log) are in `/tmp/claude-502/aicr-spike/out/`.

## Headline

**Every check that ran (14/14 across deployment + conformance) reported
`passed` — because the checks never actually ran.** The orchestrator Job
Pod for every single validator scheduled onto `system-1`, one of the fake
KWOK nodes carrying the `kwok.x-k8s.io/node=fake:NoSchedule` taint. KWOK's
stage-fast controller marked each container `Terminated{ExitCode: 0,
Reason: Completed, startedAt == finishedAt}` without ever creating a real
container (`imageID: ""`, `started: false`). `ctrf.ExitCodeToCTRFStatus`
maps exit code 0 to `"passed"` unconditionally, so every check's real
verification logic (gpu-operator health, DRA support, gang scheduling,
autoscaling, etc.) simply never executed, and the CTRF report says
"passed" anyway. This is not a hypothetical risk — it is what the
default facade call does today.

## Q1 — What actually passes on simulated hardware?

| Phase | Catalog checks | Applicable to this recipe | Ran | passed | failed | errored | Wall clock |
|---|---|---|---|---|---|---|---|
| deployment | 4 | 4 (operator-health, expected-resources, gpu-operator-version, check-nvidia-smi) | 4 | **4 (false)** | 0 | 0 | 4.20s |
| conformance | 13 | 10 (dra-support, gang-scheduling, accelerator-metrics, ai-service-metrics, pod-autoscaling, cluster-autoscaling, robust-controller, secure-accelerator-access, gpu-operator-health, platform-health) | 10 | **10 (false)** | 0 | 0 | 10.40s |
| performance | 4 | **0** — none of nccl-all-reduce-bw{,-net,-nvls} or inference-perf matched this (service=kind, accelerator=h100, intent=training) recipe | 0 | 0 | 0 | 0 (phase itself reports `status=skipped`) | 0.027s |

Not run (declared inapplicable to `criteria(service=kind, accelerator=h100,
intent=training, platform=kubeflow)` by AICR's own catalog filter, before
any cluster work): `slinky-slurm-health`, `slinky-slurm-imex-channel`,
`inference-gateway` (conformance); `inference-perf`, `nccl-all-reduce-bw`,
`nccl-all-reduce-bw-net`, `nccl-all-reduce-bw-nvls` (performance).

**"Passed", "failed", "errored" — what this run actually distinguishes:**
Zero checks failed or errored. But "passed" here does **not** mean "ran
and verified a true positive" — it means "the container was faked to
exit(0) by KWOK before it ever started." The recipe's 13 components
(cert-manager, gpu-operator, kai-scheduler, kube-prometheus-stack,
kubeflow-trainer, network-operator, nfd, nodewright-operator,
nvidia-dra-driver-gpu, nvsentinel, prometheus-adapter,
prometheus-operator-crds, k8s-ephemeral-storage-metrics) were **not
installed** on this cluster (`helm list -A` showed only pre-existing
argocd/kwok releases) — `gpu-operator-version` and `dra-support` "passing"
against components that don't exist is definitive proof the check logic
never ran. This is worse than the spike's original hypothesis ("a
gpu-operator health-check against KWOK nodes with no real GPUs may simply
fail") — the actual failure mode is a **silent false pass**, not a
disclosed failure.

**Why every check landed on the fake node, and why the facade can't fix it:**
`pkg/validator/v1/job_plan.go`'s `BuildJobPlan` doc comment states it
directly: *"The orchestrator Job Pod itself always uses tolerate-all
scheduling ({Operator: TolerationOpExists}) ... tolerations and
nodeSelector parameters apply to inner workloads spawned by validators
(e.g., GPU benchmarks, NCCL tests) ... not the orchestrator Job Pod
itself."* `aicr.WithValidationTolerations` / `WithValidationNodeSelector`
are documented the same way ("Does not affect the orchestrator Job").
Confirmed against the running pod:
```
"tolerations": [{"operator": "Exists"}]
"nodeSelector": null
```
So unlike `Client.CollectSnapshot`'s `AgentConfig.NodeSelector` (which
`internal/steps/discover.go` already uses to pin the snapshot agent off
the fake nodes, and which this spike's own snapshot collection relied on
successfully), **there is no facade knob to keep `ValidateState`'s
orchestrator Jobs off KWOK's fake nodes.** The orchestrator only gets a
*soft* affinity preference away from `nvidia.com/gpu.present=true` nodes
(`preferCPUNodeAffinity`); it has no hard exclusion and no override for
the taint. On a cluster with only one real (untainted) node and N fake
(tainted) ones, the scheduler is free to place it on a fake node, and
apparently prefers to (likely resource-scoring: the fake nodes advertise
huge unused capacity vs. the real control-plane, which is running the
cluster's actual system pods).

## Q2 — Can a check result be attributed to a component?

**Refuted.** Real CTRF output (`/tmp/claude-502/aicr-spike/out/phase-deployment-raw-ctrf.json`,
`phase-conformance-raw-ctrf.json`):
```json
{
  "duration": 0,
  "name": "gpu-operator-version",
  "status": "passed",
  "suite": ["deployment"]
}
```
`Suite` is not a component path — it is a **one-element array containing
the phase name**, verbatim (`["deployment"]`, `["conformance"]`), for
every one of the 14 tests, with no exception. `Name` is the **validator
catalog entry's own name** (`operator-health`, `expected-resources`,
`gpu-operator-version`, `check-nvidia-smi`, `dra-support`,
`gang-scheduling`, `accelerator-metrics`, `ai-service-metrics`,
`pod-autoscaling`, `cluster-autoscaling`, `robust-controller`,
`secure-accelerator-access`, `gpu-operator-health`, `platform-health`) —
a check identifier, not a recipe component identifier. Confirmed in
source too: `pkg/validator/ctrf/builder.go`'s `AddResult`/`AddSkipped`
hard-code `Suite: []string{r.Phase}` / `Suite: []string{phase}` — there is
no code path that ever puts a component name into `Suite`.

There is exactly one place in the whole validator catalog where a
check declares a structured link to a component:
`ai-service-metrics`'s `dependencyAffinity: [{componentRef:
kube-prometheus-stack, ...}]` in `recipes/validators/catalog.yaml`. But
that field only steers **pod scheduling** (co-locate with Prometheus) —
it is never copied into the `TestResult` the caller gets back, and
`RawReport`/`Report` carry no `componentRef`, `extra["component"]`, or
similar field. (`TestResult.Extra` exists and *could* carry this, but is
populated only from each container's own `EmitExtra` sentinel-line
output — arbitrary check-defined counts/enums, not a component
identifier, per its own doc comment "Values are counts/enum codes only —
never node names or IPs.")

**Practical consequence for "green checks land next to their components":**
the best a UI could do is a hand-maintained, best-effort string mapping
from check `Name` to component `Name` (e.g. `gpu-operator-version` /
`gpu-operator-health` → `gpu-operator`; `dra-support` →
`nvidia-dra-driver-gpu`; `gang-scheduling` → `kai-scheduler`;
`robust-controller` → `kubeflow-trainer`). Several checks have **no
1:1 component to map to at all**: `operator-health` and
`expected-resources` are generic/multi-component; `platform-health` is
platform-wide; `cluster-autoscaling`'s description literally says
"Verify cluster autoscaling with Karpenter" — **Karpenter is not a
component in this recipe's 13-component list at all**, so that check
cannot be attributed to any component this console would ever show. This
is not a UI implementation gap; it's a genuine data-shape mismatch
between AICR's per-check catalog and AICR's per-component recipe.

## How the snapshot was obtained

Followed `internal/steps/discover.go` / `internal/aicrclient/`'s own
pattern exactly: `client.CollectSnapshot(ctx, &aicr.AgentConfig{...})`
with `NodeSelector: {"node-role.kubernetes.io/control-plane": ""}` to pin
the agent Job off the fake KWOK nodes (the taint excludes them from
scheduling regardless of the label match, since the fake `system-0/1`
nodes also carry the `control-plane` label). Took **3.1s**. The real
in-cluster collector genuinely ran (produced real
`/proc/sys/...`, `linuxkit`-flavored grub data, a real "D-Bus not
available" error, a real "failed to read `/lib/modules/...`" error) — not
a KWOK-faked completion, confirming the NodeSelector worked as
`discover.go`'s comment says it should.

**A fixture *can* be used** — this is a real, useful finding for the e2e
story that contradicts the spike brief's caution. `internal/aicrclient/options.go`'s
own `decodeSnapshot` (and `internal/steps/criteria.go`'s identical
`decodeSnapshot`) already do exactly this: `yaml.Unmarshal` the raw
snapshot bytes into `snapshotter.Snapshot`, then `aicr.WrapSnapshot(&inner)`
to populate the unexported `internal` field — no live agent required. This
repo ships committed fixtures that round-trip through exactly this path:
`internal/steps/testdata/snapshot-kwok-h100.yaml` (and the `internal/gap/`
copy). **I did not end up needing the fixture path** — CollectSnapshot
against the live cluster was fast (3.1s) and more representative, so I
used the real agent per the brief's primary instruction — but a future e2e
that wants a snapshot without deploying the agent Job can use
`aicr.WrapSnapshot` over a committed fixture with no caveats; it is the
same code path this console's own `/api/options` handler already relies
on for a stored `snapshot.yaml` artifact.

One nuance worth flagging: the fingerprint used for **recipe resolution**
(`service=kind, accelerator=h100, nodes=7`) comes from **cluster-wide node
label topology** (`nodeTopology.label.nvidia.com/gpu.product`), not from
the collecting pod's own hardware probe — which correctly reported
`gpu-present: false, gpu-count: 0, driver-loaded: false` because it ran on
the real, GPU-less control-plane node. Recipe resolution trusts node
labels (which KWOK fakes perfectly); the hardware collector distrusts
them (correctly). Both readings are simultaneously present in the same
snapshot and disagree — a caller that only reads the collector's local
GPU measurement would (correctly) conclude "no GPU here," while the
criteria/fingerprint layer (correctly, given the demo's premise) resolves
an h100 recipe anyway.

## Did checks schedule pods? What did they need?

Yes — every deployment/conformance check is one Kubernetes `Job` (`aicr-<name>-<hex>`,
`BackoffLimit: 0`) with a single `validator` container running
`ghcr.io/nvidia/aicr-validators/{deployment,conformance,performance}:v0.19.0`
(the catalog's `:latest` tag is rewritten to the client's pinned version,
`v0.19.0`, per `catalog.yaml`'s documented resolution order — confirmed:
images pulled were tagged `:v0.19.0`, not `:latest`). No `ImagePullSecrets`
were needed — `ghcr.io/nvidia/*` pulled anonymously in ~0.5s in a smoke
test. Every orchestrator pod:
- got `tolerations: [{operator: Exists}]` — hardcoded, not configurable via the facade (see Q1).
- got no `nodeSelector` and a **soft** `preferCPUNodeAffinity` (prefer nodes without `nvidia.com/gpu.present`) — not a hard constraint, and not sufficient to keep it off a fake node once that fake node isn't GPU-labeled (`system-0`/`system-1` are the KWOK "system" node type, GPU-label-free, so the soft affinity doesn't disqualify them).
- landed on `system-1` (a fake node) in all 14/14 cases observed.

`WithValidationTolerations`/`WithValidationNodeSelector` exist but (per
source doc and this run) only affect **inner** workloads a check might
spawn (e.g., NCCL benchmark pods) — not the orchestrator itself. Since
none of the applicable checks in this run spawn inner workloads (no NCCL
checks were applicable — see Q1), this run does not exercise that inner
path at all; it is documented but unverified by this spike.

## Wall clock

**17.78 seconds total** (CollectSnapshot 3.14s + ResolveRecipeFromSnapshot
16ms + deployment 4.20s + conformance 10.40s + performance 0.03s). Trivially
fits a demo's attention span — but for the wrong reason: nothing real ran.
A validation pass against a cluster where the checks actually execute
(real components installed, checks scheduled on real or at least
non-KWOK-faked infrastructure) would take however long
`operator-health`/`expected-resources` (up to 2m/8m) or the conformance
checks (up to 10m) actually need — this spike cannot speak to that number
because nothing here ran for real. Given how fast and clean the false-pass
path is, there's a real risk a demo operator would not notice anything is
wrong without inspecting `kubectl get pods -o wide` themselves.

## What surprised me

1. **The false-pass, not a false-fail.** The spike brief's own hypothesis was that a health check would "simply fail" against fake hardware. The actual failure mode is worse and quieter: a 100% pass rate, sub-20-second wall clock, with the underlying components never installed and the check containers never started. A demo audience or a screenshot would see nothing but green.
2. **`Client.CollectSnapshot`'s `AgentConfig.NodeSelector` and `Client.ValidateState`'s equivalent-sounding `WithValidationNodeSelector` solve two different problems.** The former controls where the one agent Job itself schedules. The latter, despite the name symmetry, only forwards to *inner* workloads a check spawns — never the check's own orchestrator Job. This asymmetry is undocumented at the point of use (you have to read `job_plan.go`'s doc comment to find it) and is exactly the kind of trap the console's Phase 3 design needs to know about before assuming "we'll just pass tolerations like Discover does."
3. **The recipe-resolution fingerprint and the hardware collector read the same snapshot and disagree, silently, on whether a GPU is present** — one trusts node labels, the other trusts local hardware probing, both are "correct" for what they measure, and nothing in the `Snapshot` type flags the disagreement.
4. **The performance phase wasn't slow — it never ran at all.** Zero of its 4 catalog checks applied to a `kind`/`h100`/`training` recipe (the NCCL checks are matrixed to specific service+accelerator combos, none of which include `kind`). This resolves the spike's biggest a-priori wall-clock risk (a 65-minute `inference-perf` or three 30-minute NCCL checks) for free — but only for *this* criteria combination; a different service (eks/gke) would very likely select real, GPU-needing performance checks that would run head-first into the same fake-node trap.

## Files

- `/tmp/claude-502/aicr-spike/main.go` — the throwaway program.
- `/tmp/claude-502/aicr-spike/out/spike.log` — full run log (timestamps, per-check status, the `dial tcp: lookup system-1 ... no such host` warnings that first revealed the fake-node placement).
- `/tmp/claude-502/aicr-spike/out/snapshot.yaml` — the real collected snapshot (23.8KB).
- `/tmp/claude-502/aicr-spike/out/recipe.json` — resolved 13-component recipe.
- `/tmp/claude-502/aicr-spike/out/phase-{deployment,conformance,performance}-raw-ctrf.json` — real CTRF reports (Q1/Q2 source of truth).
- `/tmp/claude-502/aicr-spike/out/all-phase-results.json` — all three typed `PhaseResult`s (Phase/Status/Duration/Summary/Report) in one file.
- `/tmp/claude-502/aicr-spike/out/timings.json` — wall-clock breakdown.
- Cluster evidence still live on `aicr-kwok-test` (namespace `aicrme-spike`, `WithValidationCleanup(false)`): `kubectl --kubeconfig /tmp/claude-502/aicr-kwok-test.kubeconfig -n aicrme-spike get pods -o wide` shows all 14 Jobs' pods on node `system-1`, `Completed`, 0-1s duration.
