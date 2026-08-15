#!/usr/bin/env bash
# Drives the full Discover -> Recommend arc against a live Kind cluster
# through the real HTTP API: create the cluster, install the KWOK controller
# and AICR's simulated H100 nodes, install the chart, log in, create a run,
# answer intent=training/platform=kubeflow once the run parks, and assert it
# reaches state=done with a resolved, non-empty recipe. This is Phase 1's
# proof that the whole no-hardware demo arc works on every PR.
#
# Why simulated GPU nodes are mandatory, not optional: a plain KWOK cluster
# with no worker nodes has no derivable accelerator in its snapshot, so
# every intent/platform pair fails AICR's coverage post-condition and
# Recommend fails closed (pinned by internal/steps'
# TestRecommendKWOKGPUlessFixtureMatrix). Of the (intent, platform)
# pairs AICR's catalog can resolve against a simulated-H100 EKS/Ubuntu
# cluster, training/kubeflow is one -- this script drives exactly that pair,
# never a hardcoded assumption that any pair resolves.
#
# The simulated nodes reproduce the topology AICR's own reference tooling
# (github.com/NVIDIA/aicr's kwok/scripts/apply-nodes.sh, run against
# recipes/overlays/h100-eks-ubuntu-training-kubeflow.yaml) generates: 2
# system nodes (m7i.4xlarge) + 4 GPU nodes (p5.48xlarge, 8x H100 each),
# built from kwok/profiles/eks/{system-m7i,p5-h100}.yaml. The values are
# inlined below rather than shelling out to that script: it lives in a
# separate repo not checked out in this repo's CI.
#
# Getting a live run this far surfaced three gaps in aicrme's own Discover
# wiring that would break Discover on ANY cluster, real or simulated, not
# just KWOK -- fixed alongside this script (see cmd/aicrme/main.go and
# internal/steps/discover.go): the snapshot agent's Image, JobName, and
# ServiceAccountName were never defaulted, so aicr.Client.CollectSnapshot
# (the Go entry point this console calls, unlike its CLI) forwarded them to
# the API server as empty strings and Discover failed before any Job could
# even be scheduled. A fourth issue is KWOK-specific: every simulated GPU
# node carries the kwok.x-k8s.io/node=fake:NoSchedule taint, and AICR's
# client auto-targets GPU-labeled nodes when no NodeSelector is set -- see
# the pin-off-the-fake-nodes step below.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "${SCRIPT_DIR}/lib.sh"

CLUSTER="${CLUSTER:-aicrme-e2e-kwok}"
NS="${NS:-aicrme}"
IMAGE="${IMAGE:-aicrme:e2e-kwok}"
PORT="${PORT:-18081}"
ADDR="localhost:${PORT}"
# Must track .settings.yaml's test_tools.kwok. Not read from that file at
# run time: this script has no yq dependency, and the tool list this task
# was scoped against (helm, kubectl, Docker, kind) does not include one.
KWOK_VERSION="${KWOK_VERSION:-0.8.0}"

KWOK_K8S_VERSION="v1.33.5"
KWOK_REGION="us-east-1"
KWOK_ZONES=(us-east-1a us-east-1b)

JAR="$(mktemp -t aicrme-kwok-jar.XXXXXX)"
PF_PID=""
ec=0

# dump_recent_events prints the last 50 SSE events for a failed run -- the
# brief's diagnostic requirement. Best-effort: the SSE connection is
# long-lived, so it is force-closed after a few seconds rather than awaited.
dump_recent_events() {
  echo "--- last 50 SSE events ---" >&2
  set +e
  curl -fsS -b "${JAR}" --max-time 5 "http://${ADDR}/api/events?since=0" 2>/dev/null \
    | sed -n 's/^data: //p' | tail -n 50 >&2
  set -e
}

# fail_run prints the run's error, then exits 1. The SSE dump itself happens
# once, in cleanup, while the port-forward is still alive -- see cleanup's
# ordering below.
fail_run() {
  local run_json="$1"
  echo "run failed: $(echo "${run_json}" | jq -r '.error // "unknown error"')" >&2
  echo "full run: ${run_json}" >&2
  exit 1
}

# node_yaml emits one fake KWOK Node object to stdout. Field values
# reproduce AICR's kwok/profiles/eks/{system-m7i,p5-h100}.yaml node
# profiles and kwok/templates/nodes/node.yaml.tmpl's structure -- see the
# header comment for why they are inlined rather than sourced from there.
node_yaml() {
  local name="$1" node_type="$2" instance_type="$3" zone="$4" \
    cpu="$5" memory="$6" storage="$7" max_pods="$8"
  local extra_label="" gpu_label_block="" gpu_annotation_block=""
  local gpu_capacity_block="" gpu_allocatable_block=""

  if [[ "${node_type}" == "system" ]]; then
    extra_label='    node-role.kubernetes.io/control-plane: ""'
  fi
  if [[ "${node_type}" == "accelerated" ]]; then
    gpu_label_block='    nvidia.com/gpu.present: "true"
    nvidia.com/gpu.product: NVIDIA-H100-80GB-HBM3
    nvidia.com/gpu.count: "8"
    nvidia.com/gpu.memory: "81920"
    nvidia.com/cuda.driver.major: "570"
    nvidia.com/cuda.driver.minor: "86"'
    gpu_annotation_block='    nvidia.com/gpu.driver.version: "570.86.16"'
    gpu_capacity_block='    nvidia.com/gpu: "8"'
    gpu_allocatable_block='    nvidia.com/gpu: "8"'
  fi

  cat <<EOF
apiVersion: v1
kind: Node
metadata:
  name: ${name}
  labels:
    type: kwok
    kubernetes.io/hostname: ${name}
    kubernetes.io/os: linux
    kubernetes.io/arch: amd64
    node.kubernetes.io/instance-type: ${instance_type}
    topology.kubernetes.io/region: ${KWOK_REGION}
    topology.kubernetes.io/zone: ${zone}
    aicr.run/node-type: ${node_type}
${extra_label}
${gpu_label_block}
  annotations:
    kwok.x-k8s.io/node: "fake"
${gpu_annotation_block}
spec:
  taints:
    - key: kwok.x-k8s.io/node
      value: fake
      effect: NoSchedule
status:
  nodeInfo:
    architecture: amd64
    containerRuntimeVersion: containerd://1.7.0
    kernelVersion: 6.1.0-fake
    kubeletVersion: ${KWOK_K8S_VERSION}
    operatingSystem: linux
    osImage: Amazon Linux 2023
  capacity:
    cpu: "${cpu}"
    memory: ${memory}
    ephemeral-storage: ${storage}
    pods: "${max_pods}"
${gpu_capacity_block}
  allocatable:
    cpu: "${cpu}"
    memory: ${memory}
    ephemeral-storage: ${storage}
    pods: "${max_pods}"
${gpu_allocatable_block}
  conditions:
    - type: Ready
      status: "True"
      reason: KubeletReady
      message: kubelet is posting ready status
    - type: MemoryPressure
      status: "False"
      reason: KubeletHasSufficientMemory
    - type: DiskPressure
      status: "False"
      reason: KubeletHasNoDiskPressure
    - type: PIDPressure
      status: "False"
      reason: KubeletHasSufficientPID
  addresses:
    - type: Hostname
      address: ${name}
EOF
}

# apply_kwok_nodes creates 2 system + 4 GPU (H100) fake nodes and waits for
# the KWOK controller (installed by the caller) to bring them Ready.
apply_kwok_nodes() {
  local tmp i zone
  tmp="$(mktemp -d)"
  node_yaml system-0 system m7i.4xlarge "${KWOK_ZONES[0]}" 16 64Gi 100Gi 110 >"${tmp}/system-0.yaml"
  node_yaml system-1 system m7i.4xlarge "${KWOK_ZONES[1]}" 16 64Gi 100Gi 110 >"${tmp}/system-1.yaml"
  for i in 0 1 2 3; do
    zone="${KWOK_ZONES[$((i % 2))]}"
    node_yaml "gpu-${i}" accelerated p5.48xlarge "${zone}" 192 2048Gi 3800Gi 250 >"${tmp}/gpu-${i}.yaml"
  done
  kubectl apply -f "${tmp}/"
  kubectl wait --for=condition=Ready nodes -l type=kwok --timeout=60s
  rm -rf "${tmp}"
}

cleanup() {
  local exit_code="$1"
  # Diagnostics run BEFORE killing the port-forward, and exactly once here
  # (fail_run no longer dumps its own copy): dump_recent_events curls the
  # console through PF_PID, so dumping after the kill -- or twice, once from
  # fail_run and again here -- was the previous, empty-on-failure bug.
  if [[ "${exit_code}" -ne 0 ]]; then
    e2e_diagnose "${NS}"
    dump_recent_events
  fi
  if [[ -n "${PF_PID}" ]]; then
    kill "${PF_PID}" 2>/dev/null || true
  fi
  rm -f "${JAR}"
  kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
}
# Exit status is captured before cleanup runs anything else, and re-asserted
# with an explicit exit at the end: bash otherwise reports the EXIT trap's
# own last command status (from `kind delete cluster ... || true`, always 0)
# as the script's exit code, which would hide a real failure from CI.
trap 'ec=$?; cleanup "$ec"; exit "$ec"' EXIT

echo "--- create Kind cluster"
kind create cluster --name "${CLUSTER}" --wait 120s

echo "--- install KWOK controller (v${KWOK_VERSION})"
curl -fsSL --connect-timeout 10 --max-time 60 \
  "https://github.com/kubernetes-sigs/kwok/releases/download/v${KWOK_VERSION}/kwok.yaml" \
  | kubectl apply --request-timeout=30s -f -
curl -fsSL --connect-timeout 10 --max-time 60 \
  "https://github.com/kubernetes-sigs/kwok/releases/download/v${KWOK_VERSION}/stage-fast.yaml" \
  | kubectl apply --request-timeout=30s -f -
kubectl -n kube-system rollout status deploy/kwok-controller --timeout=120s

echo "--- apply simulated H100 nodes (2 system + 4x p5.48xlarge)"
apply_kwok_nodes

echo "--- build and load console image"
e2e_build_and_load_image "${CLUSTER}" "${IMAGE}"

echo "--- install chart"
e2e_install_chart "${NS}" "${IMAGE}"

# Every simulated GPU node carries the kwok.x-k8s.io/node=fake:NoSchedule
# taint (see node_yaml above) and no real kubelet. AICR's Go client
# auto-targets a node advertising nvidia.com/gpu.present=true when no
# NodeSelector is set -- exactly right on real hardware, but on this cluster
# that selector matches only the tainted fake GPU nodes. Left unpinned, the
# agent pod carries no toleration for that taint (deliberately -- tolerating
# it would let the pod land there, and kwok-controller fakes Running/
# Succeeded status for anything scheduled onto a kwok node with no real
# execution ever happening, confirmed against kwok-controller v0.8.0's
# stage-fast.yaml pod-complete Stage), so it would stay Pending on every
# fake GPU node and Discover would time out loudly rather than ever
# completing. Pin the agent Job onto the real node instead.
echo "--- pin the snapshot agent off the simulated GPU nodes onto the real one"
kubectl -n "${NS}" set env deploy/aicrme 'AICRME_SNAPSHOT_NODE_SELECTOR=node-role.kubernetes.io/control-plane='
kubectl -n "${NS}" rollout status deploy/aicrme --timeout=120s

kubectl -n "${NS}" port-forward "svc/aicrme" "${PORT}:8080" >/dev/null 2>&1 &
PF_PID=$!
sleep 3

echo "--- login"
e2e_login "${ADDR}" "${JAR}" "${NS}"

echo "--- POST /api/runs"
RUN_JSON="$(curl -fsS -b "${JAR}" -X POST "http://${ADDR}/api/runs" -H 'Content-Type: application/json')"
RUN_ID="$(echo "${RUN_JSON}" | jq -r '.id')"
[[ -n "${RUN_ID}" && "${RUN_ID}" != "null" ]] || {
  echo "no run id in response: ${RUN_JSON}" >&2
  exit 1
}
echo "run id: ${RUN_ID}"

# Discover deploys the AICR snapshot agent as a Kubernetes Job (image pull +
# schedule + run + report), so this loop allows several minutes.
echo "--- poll until awaiting_decision (Discover complete)"
STATE=""
for _ in $(seq 1 90); do
  RUN_JSON="$(curl -fsS -b "${JAR}" "http://${ADDR}/api/runs/${RUN_ID}")"
  STATE="$(echo "${RUN_JSON}" | jq -r '.state')"
  [[ "${STATE}" == "awaiting_decision" || "${STATE}" == "failed" ]] && break
  sleep 5
done
[[ "${STATE}" == "failed" ]] && fail_run "${RUN_JSON}"
[[ "${STATE}" == "awaiting_decision" ]] || {
  echo "run did not reach awaiting_decision within the deadline (state=${STATE})" >&2
  fail_run "${RUN_JSON}"
}

PENDING="$(echo "${RUN_JSON}" | jq -cS '.pending')"
[[ "${PENDING}" == '["intent","platform"]' ]] || {
  echo "unexpected pending decisions: ${PENDING}" >&2
  exit 1
}

echo "--- GET /api/options: training must offer kubeflow on this cluster"
OPTIONS_JSON="$(curl -fsS -b "${JAR}" "http://${ADDR}/api/options")"
echo "${OPTIONS_JSON}" | jq -e '(.platformsByIntent.training // []) | index("kubeflow") != null' >/dev/null || {
  echo "options does not offer training/kubeflow -- the catalog change that would break the demo: ${OPTIONS_JSON}" >&2
  exit 1
}
PROVISIONAL="$(echo "${OPTIONS_JSON}" | jq -r '.provisional')"
echo "options verified (provisional=${PROVISIONAL})"

echo "--- POST /api/runs/${RUN_ID}/decide {intent:training, platform:kubeflow}"
curl -fsS -b "${JAR}" -X POST "http://${ADDR}/api/runs/${RUN_ID}/decide" \
  -H 'Content-Type: application/json' \
  -d '{"intent":"training","platform":"kubeflow"}' >/dev/null

echo "--- poll until done (Recommend complete)"
STATE=""
for _ in $(seq 1 60); do
  RUN_JSON="$(curl -fsS -b "${JAR}" "http://${ADDR}/api/runs/${RUN_ID}")"
  STATE="$(echo "${RUN_JSON}" | jq -r '.state')"
  [[ "${STATE}" == "done" || "${STATE}" == "failed" ]] && break
  sleep 3
done
[[ "${STATE}" == "done" ]] || fail_run "${RUN_JSON}"

echo "--- recipe resolved; extracting component count from the SSE stream"
# A parse failure here is a hard failure, not a warning-and-pass: this is the
# assertion that proves Recommend actually pinned components, not just that
# the run reached done with no error. Silently falling back to the weaker
# state=done floor would let an SSE event-shape change (e.g. a Data field
# rename) quietly downgrade this test to a much weaker one while it kept
# reporting green -- exactly the failure mode this whole task exists to
# catch. Empirically reliable in practice (verified against a real run
# resolving 13 components), so a genuine miss here means something is wrong
# and the test should say so loudly.
COMPONENT_COUNT=""
set +e
COMPONENT_COUNT="$(curl -fsS -b "${JAR}" --max-time 5 "http://${ADDR}/api/events?since=0" 2>/dev/null \
  | sed -n 's/^data: //p' \
  | jq -r 'select(.data.componentCount != null) | .data.componentCount' 2>/dev/null \
  | tail -n 1)"
set -e
[[ -n "${COMPONENT_COUNT}" ]] || {
  echo "could not parse a component count from the SSE stream -- the KindLog/Data event shape probably changed" >&2
  exit 1
}
echo "resolved recipe: ${COMPONENT_COUNT} components"
[[ "${COMPONENT_COUNT}" -gt 0 ]] || {
  echo "resolved recipe reports zero components" >&2
  exit 1
}

echo "PASS: discover-recommend e2e green (run ${RUN_ID}, training/kubeflow resolved)"
