# Phase 2 handoff

**Status:** Phases 0 and 1 are complete and merged to `main`. Phase 2 has not started.
**Spec:** `approach.md`. **Phase 0-1 plan:** `docs/superpowers/plans/2026-08-13-aicrme-phase-0-1.md` — its "Roadmap for Phases 2-5" section carries the `deploy.sh` marker grammar and the applier decision, which are the main technical inputs to Phase 2.

This document records what the Phase 0-1 build learned that the code and the plan do not already state. It exists because the working ledger those findings lived in was scratch and has been deleted.

---

## What works today

`helm install` on Kind/KWOK gives a console that runs the full **Discover → Recommend** arc: login, live SSE timeline, real cluster snapshot via the AICR agent Job, capability statement and gap list, two questions, resolved and version-pinned recipe. Verified live end to end by `test/e2e/discover-recommend.sh`.

Apply, Validate, Prove, Reset, GitOps export, AKS, and real hardware are Phases 2-5 and not built.

### Demoing it — the non-obvious prerequisite

**A plain KWOK cluster cannot resolve a recipe.** With no worker nodes the snapshot has no derivable `accelerator`, so every intent/platform pair fails AICR's coverage post-condition. This cost a full fix round to diagnose and an implementer proposed three scope reductions before the real cause was found.

The cluster must carry AICR's simulated GPU nodes. Use the tooling in the AICR repo: `kwok/profiles/eks/p5-h100.yaml` presents fake nodes as EKS `p5.48xlarge` H100s, applied via `kwok/scripts/apply-nodes.sh`; see `kwok/README.md`. `test/e2e/discover-recommend.sh` does this correctly — copy its approach.

Against a simulated-H100 KWOK cluster, **5 of 12 pairs resolve**: `training/kubeflow` (13 components), `training/slurm` (16), `training/any` (12), `inference/dynamo` (16), `inference/any` (14). The other 7 fail because AICR's catalog has no `service=kind` overlay for them — not an accelerator problem. `internal/steps/recommend_test.go` pins both matrices against real fixtures.

Two snapshot fixtures are committed and both matter: `snapshot-kwok.yaml` (no GPUs, pins the degenerate case and the gap package's degraded-collector guard) and `snapshot-kwok-h100.yaml` (simulated GPUs, pins the demo path). Do not delete either.

---

## Constraints Phase 2 inherits

**The applier drives `deploy.sh`, not per-component `install.sh`.** The plan's Decisions table originally said the opposite. `deploy.sh` is ~480 generated lines carrying correctness logic a per-component loop silently drops: preflight for terminating namespaces / stale webhooks / orphaned CRD groups, per-component wait derivation (`kai-scheduler` 20m/1 retry, `*-readiness` gates 1h35m, `ASYNC_COMPONENTS` skipping `--wait`), quadratic-backoff retry with helm hook-Job cleanup, and a post-install block that waits for `nvidia.com/gpu-driver-upgrade-state=upgrade-done` on every managed node before restarting the DRA kubelet plugin. Skipping that last one strands DRA pods in `ContainerCreating` (AICR issue #973). The marker-to-event grammar is in the plan's roadmap.

Guard it: pin the sha256 of `pkg/bundler/deployer/helm/templates/deploy.sh.tmpl` from the pinned aicr module so an upstream edit forces a parser review. The freed upstream-PR budget (Risk 1 turned out already resolved) should go to adding an opt-in machine-readable event stream to that template, retiring the parser.

**`StateActive` is declared but unreachable.** `engine.execute()` unconditionally finishes at `StateDone`, and no `Step`/engine hook lets a Prove step park in the terminal-but-active state the spec's §6 requires. Nothing in Phase 0-1 revisits `engine.go`, so Phase 3 must add the hook. Related: nothing in `execute`/`runStep`/`awaitDecisions` re-checks that `e.current` is still the run its goroutine started for — `Start`'s `isLive` guard is the only protection and it does not cover `StateActive`. Both bite when Reset lands.

**The ConfigMap-backed `engine.Store` is still unimplemented.** The interface exists (`internal/engine/store.go`) with only `NewMemoryStore`. Apply takes 10-20 minutes on real hardware, which is when restart survival starts to matter. Two latent issues activate with it: `handleCreateRun` passes the cancellable request context to `engine.Start`, so a `store.Save` failure would leave `e.current` live and permanently 409 new runs; and a server restart resets `bus.nextID` to 1 while the browser keeps a high `lastId`, so `?since=` filters out the entire new run. The bus needs an epoch or a run-scoped cursor, designed together with the store.

**The training workload does not exist.** `demos/workloads/training/` in the AICR repo holds only `gke-nccl-test-tcpxo.yaml`. The spec's "Training + Kubeflow → TrainJob NCCL all-reduce" finale has no source material and must be authored in Phase 3. Inference is well covered (`vllm-agg.yaml`, `nimservice-llama-3-2-1b.yaml`, `chat-server.sh`, `chat.html`).

**`aicrclient` has headroom.** `MakeBundle`, `BundleComponents`, and `ValidateState` all exist on `*aicr.Client` and are deliberately not exposed yet. The single-method interface decomposition accommodates a `Bundler`/`Validator` additively with no restructuring.

---

## Deferred findings

Every per-task review's minors, triaged by the final whole-branch review as acceptable to defer. Grouped by where they bite.

### Activated by Phase 2 work
- ConfigMap store: the `engine.Start` context / 409-latch and the `bus.nextID` epoch reset, above.
- `readOnlyRootFilesystem` is omitted from the pod security context — correctly deferred until the `deploy.sh` wiring shows which helm/kubectl cache dirs need to be writable.
- The image is 55 MB compressed (was 38 MB before `cmd/aicrme` imported AICR, which pulls its full transitive graph). It is the first thing a cluster pulls. Watch it.

### Raised to the top of the Phase 2 list by the final review
- **Nothing resets `authed` on a 401.** After the 8-hour session expiry the console sticks on "reconnecting…" forever with no path back to the login screen.
- `Discover.tsx` shows the green "already capable" copy for `gap.Analyze`'s degraded early return too — "No cluster snapshot available" also yields nil gaps. Should distinguish capable from no-snapshot.

### Cosmetic / low risk
- `.golangci.yaml` carries orphaned `gocognit`/`gocyclo` settings for linters not enabled.
- `/` answers all methods, so `POST /` and `POST /healthz` return index.html 200.
- `isAssetPath` heuristic: false-positive on a client route containing a dot, false-negative on an extensionless asset.
- CSP lacks `base-uri`, `form-action`, `object-src`. `index.html` is not content-hashed and gets no `Cache-Control`.
- `useEvents.ts` comment claims an O(1) fast path but the append is an O(n) copy; it closes the closure `source` rather than `msg.target`.
- `gap` rules: `gpu-scheduler` covers KAI images only (`kueue`/`grove` are also gang-scheduling adjacent); a comment cites the chart name `aws-efa-k8s-device-plugin` where the component is `aws-efa`; `usableGPUs` is all-or-nothing.
- `ServiceFromSnapshot` is production-dead (test-only callers). `pairResolves` sits at 81.8% — the gap is two registry parse-failure returns unreachable from real catalog candidates, left honestly uncovered.
- No dedicated failure-state screen; errors render as a text line.
- Chart: helm/kubectl downloads are TLS-only with no checksum; `alpine` pinned by tag not digest; the ClusterRoleBinding uses the cluster-scoped name `aicrme`, so a second install in another namespace fails closed; an operator-supplied `--set auth.password=` is persisted in helm release values and shell history.
- `e2e.yaml` runs on `pull_request` only, with no `concurrency` or `timeout-minutes`.

### Known gaps in the gap list
Only 4 of the spec's 5 gap rules ship: `gpu-driver`, `device-plugin`, `gpu-metrics`, `gpu-scheduler`. EFA is deferred because `TypeNetworkTopology` is gated behind the agent's `--discover-network` flag (now plumbed through `DiscoverConfig` but off by default) and no fixture exercises it. Two residual false-positive paths exist on `gpuOperatorAbsent`: a policy-specific collector failure defeats the degraded guard, and a standalone helm install of `nvidia-device-plugin`/`dcgm-exporter` with no ClusterPolicy makes both gaps fire falsely. Both are outside the "vanilla cluster" premise.

The demo's headline number — "N of M GPUs usable" — is unit-tested but no fixture exercises it, because even the simulated-H100 KWOK snapshot reports `gpu-present: false` (the agent finds no real device). It first runs for real on EKS in Phase 4.

---

## Explicit non-goals — do not "fix" these

Air-gapped operation, private registries, and registry mirroring are spec non-goals; direct internet access to ghcr.io, nvcr.io, and the upstream Helm repos is an assumed precondition. `AICRME_SNAPSHOT_IMAGE` is deliberately not exposed in `values.yaml` for this reason. Day-2 operations, multi-user auth/OIDC/scoped RBAC, multi-cluster, and in-UI editing of component sets or versions are all out of scope. The `cluster-admin` grant is deliberate and disclosed; do not attempt to narrow it.

---

## How to start Phase 2

Write a plan first — `superpowers:writing-plans`, informed by this document and the roadmap section of the Phase 0-1 plan. Then execute it; `superpowers:subagent-driven-development` was used for Phase 0-1 and worked well.

Two process notes from the Phase 0-1 run worth carrying forward. Verify the plan's own code before trusting it — four real defects shipped in the plan text and were caught only by implementers (a send-on-closed-channel race, a `ServeMux` routing panic, a Dockerfile that would have embedded an empty SPA, and a false premise about AICR deriving criteria from the snapshot). And demand bite-proofs: every fix round that mattered was verified by reintroducing the bug and watching the test fail.
