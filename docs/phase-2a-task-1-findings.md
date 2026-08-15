# Phase 2a Task 1 Findings: dry-run e2e viability probe

**Date:** 2026-08-15
**Branch:** `phase-2a`
**Gate outcome:** **1 — exit 0, markers present.** Proceed.

This probe answers the two questions later Phase 2a tasks assume without
proof: whether `deploy.sh --dry-run` survives against a Kind+KWOK cluster,
and whether its stdout marker grammar matches what the plan transcribed
from the aicr template. Both hold. The captured transcript is committed at
`internal/applier/testdata/deploy-transcript-kwok.txt` for Task 4's golden
parser test.

## Bundle generation (Step 2)

Ran the throwaway `cmd/probe` (deleted in this task's final commit) against
the committed simulated-H100 fixture
(`internal/steps/testdata/snapshot-kwok-h100.yaml`), intent=training,
platform=kubeflow — the same pair `test/e2e/discover-recommend.sh` proves
resolvable on KWOK.

```
components=13 files=61 bytes=159731 hasErrors=false dir=/tmp/aicrme-probe-bundle
```

- **Component count:** 13 (`result.Components`)
- **Files on disk:** 61
- **Bytes reported by MakeBundle:** 159731 (~156 KiB)
- **`du -sh` on disk:** **352K**
- `hasErrors=false`

`du -sh` is the number that matters for Task 3's chart `workDir.sizeLimit`:
**352K**. Note the bundle root contains **14** numbered component
directories (`001-cert-manager` … `014-prometheus-adapter`), not 13 — see
"Component count vs. deploy step count" below.

## Cluster setup (Step 3)

Reused `node_yaml`/`apply_kwok_nodes` from `test/e2e/discover-recommend.sh`
(lines ~85-190), copied verbatim into a scratch file outside the repo
(`$TMPDIR/kwok-setup.sh`, sourced) per the brief. `test/e2e/discover-recommend.sh`
itself was not modified.

- `kind create cluster --name aicrme-probe --wait 120s` — ready in 16s.
- KWOK controller v0.8.0 (`kwok.yaml` + `stage-fast.yaml`) installed and rolled out.
- Applied 2 system nodes (`m7i.4xlarge`) + 4 GPU nodes (`p5.48xlarge`, 8x
  simulated H100 each), matching the topology
  `test/e2e/discover-recommend.sh` uses.

`kubectl get nodes` showed 7 Ready nodes: the real `kind` control-plane
node (`aicrme-probe-control-plane`) plus the 2 simulated system nodes and 4
simulated GPU nodes — all `Ready`, matching the brief's expectation of "2
system + 4 GPU nodes, all Ready" (in addition to Kind's own control-plane
node, which is not part of the simulated topology).

## `deploy.sh --dry-run` result (Step 4 — the go/no-go gate)

```
cd /tmp/aicrme-probe-bundle
NO_COLOR=1 DRY_RUN_FLAG=--dry-run KUBECONFIG_FLAG= HELM_DEBUG_FLAG= \
  bash deploy.sh --retries 0 2>&1 | tee /tmp/aicrme-probe-transcript.txt
```

- **Exit code: 0**
- All 14 numbered deploy steps report `└─ ✓ <name> installed`.
- 0 lines matching `level=ERROR`.
- 14 lines matching `level=WARN` — all the same benign Helm client warning,
  once per `helm upgrade --install --dry-run` invocation:
  `level=WARN msg="--dry-run is deprecated and should be replaced with '--dry-run=client'"`.
  This is Helm v4's own deprecation notice for the deploy.sh-generated
  `--dry-run` flag, not an application-level failure.
- Final script line: `✓ All components installed successfully.`

**This is gate outcome 1 (exit 0, markers present).** No component failed
to render under `--dry-run`; no CRD-not-installed template failure was hit.
Proceeding per the brief's instructions for outcome 1.

Transcript size: 5,882,375 bytes (~6.0 MiB), 101,765 lines — the full
`helm template`-rendered manifest output for all 14 components, which is
why the fixture is large. This is the real, unedited stdout captured via
`tee`; nothing was trimmed.

## Component count vs. deploy step count

The probe's `result.Components` count is **13**, but `deploy.sh` numbers
**14** steps:

```
┌─ [1/14]  cert-manager               → cert-manager
┌─ [2/14]  nfd                        → node-feature-discovery
┌─ [3/14]  network-operator           → nvidia-network-operator
┌─ [4/14]  nodewright-operator        → skyhook
┌─ [5/14]  prometheus-operator-crds   → monitoring
┌─ [6/14]  kube-prometheus-stack      → monitoring
┌─ [7/14]  gpu-operator               → gpu-operator
┌─ [8/14]  k8s-ephemeral-storage-metrics → monitoring
┌─ [9/14]  kai-scheduler              → kai-scheduler
┌─ [10/14] kubeflow-trainer           → kubeflow
┌─ [11/14] kubeflow-trainer-post      → kubeflow
┌─ [12/14] nvidia-dra-driver-gpu      → nvidia-dra-driver
┌─ [13/14] nvsentinel                 → nvsentinel
┌─ [14/14] prometheus-adapter         → monitoring
```

Step 11, `kubeflow-trainer-post`, is a `local-helm` post-install step
parented to component 10 (`kubeflow-trainer`) — the bundle's
`011-kubeflow-trainer-post/` directory exists alongside
`010-kubeflow-trainer/`, both driven under a single logical component. This
is not a marker-grammar deviation (the `[N/M]` fields are plain integers
however many steps there are); it just means anything downstream that
assumes "13 components" == "13 deploy steps" is off by one. The plan's
Task 4 illustrative example (`┌─ [1/13] gpu-operator  →  gpu-operator`) used
`13` as a placeholder digit count, not a literal assertion that `M` is
always 13 — no code change is implied, but flagging it since a hardcoded
`13` anywhere downstream would be wrong on this fixture.

## Marker grammar vs. the plan's Step 5 regexes

Verified via `repr()` on the raw captured lines (Python, to rule out
terminal/locale rendering artifacts):

```
header:    '┌─ [1/14] cert-manager  →  cert-manager\n'
success:   '└─ ✓ cert-manager installed\n'
preflight: '✓ Pre-flight checks passed\n'
```

All three match the brief's Step 5 grammar **exactly, byte for byte**:

- Header: `┌─ [N/M] <name>  →  <namespace>` — confirmed two spaces on
  either side of `→`, no space between `┌` and `─`.
- Success: `└─ ✓ <name> installed` — confirmed no space between `└` and
  `─`, single space before/after `✓`.
- Preflight: exactly one line matching `^✓ Pre-flight checks passed$`
  (`grep -c` returned `1`).

No retry-line deviation to report: `--retries 0` combined with every
component succeeding on its first attempt means the retry-line grammar
(`  retrying ...`, two leading spaces per the brief) never fired in this
transcript. It was not exercised, not contradicted — no lines matching
`^  ` + retry text appear in the fixture. If Task 4's golden test wants
retry-line coverage, it will need a second, separately captured fixture (a
component would have to actually fail-then-succeed), which is out of this
task's scope.

**No regex changes needed for Task 4** — the plan's quoted patterns match
observed reality exactly.

## Tool versions (version-sensitive; record verbatim)

```
$ helm version
version.BuildInfo{Version:"v4.2.4", GitCommit:"3900f434fd3ef2b84065dc04508df48f288dba00", GitTreeState:"clean", GoVersion:"go1.26.5", KubeClientVersion:"v1.36"}

$ kubectl version
Client Version: v1.36.3
Kustomize Version: v5.8.1
Server Version: v1.36.1
```

Also relevant:

- `kind version`: `kind v0.32.0 go1.26.3 darwin/arm64`
- `kind` node image: `kindest/node:v1.36.1`
- KWOK version: `v0.8.0` (matches `test/e2e/discover-recommend.sh`'s
  `KWOK_VERSION` default)
- KWOK-simulated kubelet version on fake nodes: `v1.33.5`
- Docker server version: `29.3.1`

## Network note

`deploy.sh --dry-run` fetches charts from `helm.ngc.nvidia.com`, `ghcr.io`,
and similar hosts not on this machine's sandbox network allowlist. The
probe run (Step 4) and the bundle-generation run (Step 2, container image
resolution aside) both required `dangerouslyDisableSandbox: true` to reach
those hosts; this is expected per the task's environment notes, not a
finding about `deploy.sh` itself.

## Summary

| Question | Answer |
|---|---|
| Gate outcome | 1 (exit 0, markers present) |
| `deploy.sh` exit code | 0 |
| Components resolved | 13 |
| Deploy steps (bundle dirs) | 14 (see note above) |
| Bundle files | 61 |
| Bundle bytes (MakeBundle) | 159,731 |
| Bundle size on disk (`du -sh`) | **352K** |
| Failed components | none |
| Marker grammar deviation from plan | none |
| Transcript size | 5,882,375 bytes / 101,765 lines |
