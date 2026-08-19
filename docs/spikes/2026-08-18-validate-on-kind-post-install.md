# Probe 2: `aicr.Client.ValidateState` on a real Kind cluster with all 14 deployment actions installed

Throwaway probe. Not committed — lives outside `aicrme` on purpose (repo `git status --short`
stayed empty for the whole run; nothing under `/Users/mchmarny/dev/aicrme` was created,
edited, staged, or committed). Program: `/tmp/claude-502/probe2/validate/main.go`, a
minimally-adapted copy of probe 1's `/tmp/claude-502/aicr-spike/main.go`, built against
`github.com/NVIDIA/aicr v0.19.0` (same pin as `aicrme`'s `go.mod` and as probe 1). Cluster
built by `/tmp/claude-502/probe2/apply-real.sh`, a copy of `test/e2e/apply-real.sh` (not
edited in place; the repo's own copy was never touched) with exactly one change: `cleanup()`
no longer runs `kind delete cluster`, so the cluster survives for this probe's use.
Raw artifacts in `/tmp/claude-502/probe2/out/`.

## Headline

**Yes, `ValidateState` produces meaningful results on a real cluster — but only for the one
check (`ai-service-metrics`) whose catalog entry happens to declare a `dependencyAffinity`
that lands its orchestrator pod on a real node.** All 13 other applicable checks reproduced
probe 1's exact false-pass mechanism, on this same "real, everything-installed" cluster:
their orchestrator pods landed on KWOK fake nodes (`system-0`/`system-1`) — which
`apply-real.sh` deliberately keeps in the cluster, alongside the real Kind nodes, purely so
recipe resolution sees H100 topology — and were faked `Terminated{ExitCode:0}` in ~1 second
without ever starting a container. **Real components installed does not fix the false-green
bug; it only gives one check in fourteen a side-channel escape from it.** The one check that
escaped genuinely ran (real `imageID`, `started:true` while live, 2 minutes of real retry
logs) and genuinely failed for a GPU-shaped reason it stated in plain language.

## Q1 — Do checks genuinely execute? (imageID / started evidence)

**13 of 14 did not. 1 of 14 did — the difference is directly visible in pod status.**

Captured live, mid-execution (`kubectl get pod -o json` while `status.phase=Running`):

Fake-node check (`check-nvidia-smi`, landed on `system-0`) — this is the **terminal** state,
captured seconds after the orchestrator reported "passed":
```json
{
  "name": "validator",
  "imageID": "",
  "started": false,
  "state": {"terminated": {"exitCode": 0, "reason": "Completed",
    "startedAt": "2026-08-18T23:52:37Z", "finishedAt": "2026-08-18T23:52:37Z"}}
}
```
`startedAt == finishedAt`, `imageID` empty, `nodeName: system-0` (a KWOK fake node),
`tolerations: [{"operator": "Exists"}]`, `nodeSelector: null` — identical signature to
probe 1.

Real-node check (`ai-service-metrics`, landed on `aicrme-e2e-real-worker2`), captured while
still `Running`:
```json
{
  "name": "validator",
  "imageID": "ghcr.io/nvidia/aicr-validators/conformance@sha256:6e8d7b8507023639f6c396b0bf920d45b9e5ef2156659d83b42f31882eda6c4e",
  "started": true,
  "state": {"running": {"startedAt": "2026-08-18T23:52:47Z"}}
}
```
Real, resolved `imageID` (a real image was pulled from `ghcr.io`), `started: true` while the
container was actually executing — that is the fact probe 1 could never produce.

**One caveat worth stating precisely, since it complicates a naive "check `started`" rule for
a UI**: after this same pod *terminated* (2 minutes later, genuinely failing), `started` reset
to `false` again —
```json
{"name": "validator", "imageID": "ghcr.io/nvidia/aicr-validators/conformance@sha256:6e8d...",
 "started": false, "restartCount": 0,
 "state": {"terminated": {"exitCode": 1, "reason": "Error",
   "startedAt": "2026-08-18T23:52:47Z", "finishedAt": "2026-08-18T23:54:47Z",
   "message": "[NOT_FOUND] no DCGM_FI_DEV_GPU_UTIL time series in Prometheus after 2m0s — verify DCGM exporter is running and scraping GPU metrics"}}}
```
`started` is a liveness-style flag, not a "did this ever start" flag — it goes false on any
terminal state, real or faked. **The reliable post-hoc signal is `imageID` non-empty (a real
image was resolved and a container created) combined with `startedAt != finishedAt` (real
elapsed wall time) — not `started` in isolation once the pod has already finished.** A
console built on this facade would need to check imageID/duration, or inspect the pod while
still running, not just read `started` off a completed pod.

Full per-check node placement (`kubectl -n aicrme-validate-probe2 get pods -o
custom-columns=...`), 14/14 checks:

| Check | Node | started (final) | imageID |
|---|---|---|---|
| operator-health | system-0 | false | *(empty)* |
| expected-resources | system-0 | false | *(empty)* |
| gpu-operator-version | system-0 | false | *(empty)* |
| check-nvidia-smi | system-0 | false | *(empty)* |
| dra-support | system-0 | false | *(empty)* |
| gang-scheduling | system-1 | false | *(empty)* |
| accelerator-metrics | system-0 | false | *(empty)* |
| **ai-service-metrics** | **aicrme-e2e-real-worker2** | **false (true while Running)** | **`ghcr.io/nvidia/aicr-validators/conformance@sha256:6e8d...`** |
| pod-autoscaling | system-0 | false | *(empty)* |
| cluster-autoscaling | system-0 | false | *(empty)* |
| robust-controller | system-0 | false | *(empty)* |
| secure-accelerator-access | system-0 | false | *(empty)* |
| gpu-operator-health | system-0 | false | *(empty)* |
| platform-health | system-0 | false | *(empty)* |

Why `ai-service-metrics` alone escapes: its catalog entry
(`recipes/validators/catalog.yaml`) carries a `dependencyAffinity` steering it toward the
`kube-prometheus-stack` component's Prometheus pod (`requirement: preferred`) "so the dial is
loopback/same-node" — an accident of one check's own connectivity requirement, not a general
fix. The orchestrator's own scheduling (`pkg/validator/v1/job_plan.go`) is still tolerate-all
with only a soft anti-GPU-node preference for every check, this one included; it happened to
land on a real node because a *different* mechanism (dependency co-location) outweighed the
default preference this time. **This is not something the design can be relied on for the
other 13 checks — none of them carry a `dependencyAffinity` to any installed, un-tainted
node.**

## Q2 — Passed / failed / errored, per phase, with all 14 actions installed

| Phase | Catalog | Selected | Ran for real | passed | failed | errored/other | Phase status | Duration |
|---|---|---|---|---|---|---|---|---|
| deployment | 4 | 4 | 0/4 | 4 (all false — fake-node) | 0 | 0 | passed | 4.20s |
| conformance | 13 | 10 | 1/10 | 9 (8 false + ai-service-metrics is real but that one FAILED, not passed) | 1 (ai-service-metrics, real) | 0 | **failed** | 2m17.76s |
| performance | 4 | 0 | — | 0 | 0 | 0 (phase itself `skipped`) | skipped | 5ms |

Correcting the counts: conformance summary reports `passed: 9, failed: 1, tests: 10` — read
literally that's "9 passed", but only 8 of those 9 are the fake-node false-passes; the 9th
slot in the 10-test suite is `ai-service-metrics` itself, which is the one `failed` entry, not
a `passed` one. So: **8 checks false-passed (fake node, no execution), 1 check genuinely
ran and genuinely failed, 1 check genuinely ran and would be the only one to genuinely pass
if it could reach that state** (it can't in this environment — see Q3).

**Zero checks errored ("other"/StatusOther) in this run** — every check that executed at all
(for real or faked) reached a terminal passed/failed CTRF status, not an inconclusive one.
`ctrf.IsFailingStatus` treats `failed` and `other` identically for gating purposes, so this
run never exercised the "errored" path — a genuinely useful finding on its own: this facade's
default configuration on a real cluster does not spontaneously produce "could not determine"
outcomes; it produces clean-looking failures or clean-looking (fake) passes, both equally
readable by a naive caller as green/red with no third state to alert on.

**Failed ≠ errored in the design, confirmed by source, not just this run**:
`pkg/validator/ctrf/types.go`'s `StatusOther` doc comment: *"the check could not be executed
to a verdict (crash, OOM, timeout, or a Job that reached a terminal Failed state with no
inspectable pod)"* — a distinct code path from `StatusFailed` ("ran, verdict is negative").
`ai-service-metrics`'s outcome in this run is unambiguously the `failed` kind: it ran to
completion, produced a verdict, and that verdict was negative. This run did not exercise
`errored`.

## Q3 — Is a GPU-dependent failure distinguishable from a component-missing failure?

**For the one check that actually ran: yes, unambiguously, in `Message` and `Stdout` — but
not in `Extra` (empty).** Real CTRF entry (`/tmp/claude-502/probe2/out/phase-conformance-raw-ctrf.json`):
```json
{
  "duration": 120000,
  "message": "[NOT_FOUND] no DCGM_FI_DEV_GPU_UTIL time series in Prometheus after 2m0s — verify DCGM exporter is running and scraping GPU metrics",
  "name": "ai-service-metrics",
  "status": "failed",
  "stdout": [
    "time=2026-08-18T23:52:47.701Z level=INFO msg=\"starting check\" name=ai-service-metrics",
    "time=2026-08-18T23:52:47.713Z level=INFO msg=\"DCGM_FI_DEV_GPU_UTIL not yet available, retrying\" poll_interval=10s",
    "... (12 identical retry lines at 10s intervals) ...",
    "time=2026-08-18T23:54:47.718Z level=ERROR msg=FAIL message=\"[NOT_FOUND] no DCGM_FI_DEV_GPU_UTIL time series in Prometheus after 2m0s — verify DCGM exporter is running and scraping GPU metrics\""
  ],
  "suite": ["conformance"]
}
```
That `Message` text is exactly what a Validate screen needs to honestly render "not
applicable / no GPU here" rather than "broken": it names the missing signal
(`DCGM_FI_DEV_GPU_UTIL`), the wait budget (`2m0s`), and points at the actual remediation
(DCGM exporter). It is free text, not an enum code — `Extra` is empty for this check (its
source, `validators/conformance/ai_service_metrics_check.go`, never calls `EmitExtra`), so
the CTRF `extra` object's promised low-cardinality-enum contract is not exercised here. A UI
reading only `Extra` would see nothing for this check; a UI reading `Message` gets the full,
correct story.

**For the 13 checks that never executed: the source code shows the same honest-distinction
design exists, but this run could not observe it firing, because the orchestrator faked exit
0 before the in-pod logic ever ran.** `validators/deployment/nvidia_smi.go` (`check-nvidia-smi`,
one of the 13 false-passed checks in this very run) defines exactly the `Extra` contract the
task brief describes — real enum codes, confirmed present in source:
```go
skipReasonNoGPUNodes            = "no-gpu-nodes"             // cluster has no GPU nodes at all
skipReasonNoSchedulableGPUNodes = "no-schedulable-gpu-nodes" // GPU nodes exist but all cordoned/unschedulable
skipReasonNodesBusy             = "nodes-busy"               // schedulable GPU nodes exist but are busy with workloads
```
and — the specific detail that matters for this recipe — a fail-closed rule keyed on whether
the resolved recipe declares `gpu-operator` (confirmed present in `recipe.json` from this
run's own resolution: `{"Name": "gpu-operator", ...}` is in `Components`):
```go
if validators.RecipeDeclares(ctx, gpuOperatorComponent) {
    return errors.New(errors.ErrCodeNotFound,
        "recipe declares gpu-operator but the cluster has no GPU nodes to verify — "+
            "check node provisioning, the GPU Operator rollout, or validator RBAC")
}
```
i.e., the design's own intent is that a GPU-operator-bearing recipe with zero real GPU nodes
must **fail with a stated reason**, not silently skip or silently pass. **This is source
inspection, explicitly not a measurement** — `check-nvidia-smi`'s orchestrator landed on
`system-0` and returned faked `exitCode:0` in under a second in this run (see Q1 table), so
this code path never actually executed and its real output was never observed. The honest
answer to Q3 is split: **one real data point says yes, cleanly, via free-text `Message`; the
rest of the recipe's GPU-aware checks have the same design intent by inspection but are
*unverified by this probe* because the scheduling bug (Q1) pre-empted them before they could
run.**

## Q4 — Wall clock

**Full three-phase validation program: 2m25.2s** (`/tmp/claude-502/probe2/out/timings.json`):

| Step | Duration |
|---|---|
| CollectSnapshot | 3.11s |
| ResolveRecipeFromSnapshot | 15ms |
| ValidateState: deployment | 4.20s |
| ValidateState: conformance | 2m17.79s |
| ValidateState: performance | 39ms |
| **TOTAL** | **2m25.21s** |

Unlike probe 1's 17.8s (meaningless — nothing executed), this number is **meaningful but
still not representative of a fully-real run**: 2m17.6s of the 2m17.79s conformance-phase
time is the one real check (`ai-service-metrics`) burning its full `2m` DCGM-wait budget
before failing; every other check in every phase still completed in ~1 second because it
never left the fake-node false-pass path. A validation pass where all 14 catalog-applicable
checks actually executed against real infrastructure would run far longer — this probe still
cannot report that number, for the same structural reason probe 1 couldn't: the orchestrator
routes almost all checks around real execution entirely.

No image-pull blocker was hit: `ai-service-metrics`'s real pull of
`ghcr.io/nvidia/aicr-validators/conformance@sha256:...` from `ghcr.io` succeeded with no
delay or error in this environment.

## Cluster and install context (for reproducibility)

`test/e2e/apply-real.sh`'s copy ran to its own internal PASS: Kind cluster
`aicrme-e2e-real` (control-plane + 3 workers, `kindest/node:v1.36.1`), KWOK 0.8.0 installed
with 2 system + 4 GPU fake nodes (same as probe 1, added here *only* so recipe resolution
sees H100 topology — apply-real.sh's own header explains this), all 14 deployment actions
(`cert-manager, gpu-operator, k8s-ephemeral-storage-metrics, kai-scheduler,
kube-prometheus-stack, kubeflow-trainer, kubeflow-trainer-post, network-operator, nfd,
nodewright-operator, nvidia-dra-driver-gpu, nvsentinel, prometheus-adapter,
prometheus-operator-crds`) reached `helm ... deployed`, run reached `state=done`, every
Deployment/DaemonSet/StatefulSet converged to `readyReplicas == desired` after the 180s
settle window, and the script's own bus-event assertions (attribution, no duplicate
transitions, contiguous IDs) passed. This is the same 14-action, all-installed baseline the
task specified — not a partial or degraded install.

The one deviation from the stock script: `cleanup()` no longer runs `kind delete cluster`, so
the cluster (and all 14 components, and the 14 leftover validator Jobs/pods from this probe,
left behind via `WithValidationCleanup(false)`) is still running at the time of this report,
under an isolated `KUBECONFIG=/tmp/claude-502/probe2/kubeconfig` (the user's own
`~/.kube/config` was never touched). Mirrors probe 1's own precedent of leaving
`aicr-kwok-test` running for inspection.

## Files

- `/tmp/claude-502/probe2/apply-real.sh` — adapted copy of `test/e2e/apply-real.sh` (only
  change: cluster survives).
- `/tmp/claude-502/probe2/apply-real.log` — full install log.
- `/tmp/claude-502/probe2/validate/main.go` — the throwaway validate program (adapted from
  probe 1's).
- `/tmp/claude-502/probe2/out/spike.log` — full run log (timestamps, per-check status, the
  `dial tcp: lookup system-0/1 ... no such host` warnings that reveal fake-node placement,
  same signature as probe 1).
- `/tmp/claude-502/probe2/out/snapshot.yaml`, `recipe.json`, `criteria.json` — Discover/
  Recommend artifacts.
- `/tmp/claude-502/probe2/out/phase-{deployment,conformance,performance}-raw-ctrf.json` —
  real CTRF reports (Q1–Q3 source of truth).
- `/tmp/claude-502/probe2/out/all-phase-results.json`, `timings.json` — typed results and
  wall-clock breakdown.
- Cluster evidence still live: `KUBECONFIG=/tmp/claude-502/probe2/kubeconfig kubectl -n
  aicrme-validate-probe2 get pods -o wide` shows all 14 validator pods, 13 on
  `system-0`/`system-1` (`Succeeded`, ~1s), 1 (`ai-service-metrics`) on
  `aicrme-e2e-real-worker2` (`Failed`, 2m real duration).
