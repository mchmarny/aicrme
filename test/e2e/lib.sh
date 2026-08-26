#!/usr/bin/env bash
# Shared helpers for the Kind-based e2e scripts. Sourced, not executed:
# callers already have `set -euo pipefail`, their own CLUSTER/NS, and
# REPO_ROOT in scope.

# REPO_ROOT is derived from this file's own location so every caller has it
# without repeating the ../.. walk. A caller that already set it wins.
REPO_ROOT="${REPO_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"

# Must track .settings.yaml's test_tools.kwok. Not read from that file at
# run time: these scripts have no yq dependency, and the tool list this task
# was scoped against (helm, kubectl, Docker, kind) does not include one.
KWOK_VERSION="${KWOK_VERSION:-0.8.0}"

KWOK_K8S_VERSION="v1.33.5"
KWOK_REGION="us-east-1"
KWOK_ZONES=(us-east-1a us-east-1b)

# e2e_diagnose dumps what a human needs before a failing cluster disappears:
# pod status, recent namespace events, and the console's own log.
#
# The console's log is a local file now rather than `kubectl logs
# deploy/aicrme`, and it is the half most likely to explain a failure that the
# cluster itself cannot.
#
# Both cluster calls are pinned to the Kind context and bounded. The console
# can now fail before the cluster exists -- a daemon that is not running, a
# work directory it cannot lock -- and at that point `kubectl` with no context
# of its own means the operator's current-context, which on a laptop is a real
# cluster behind a VPN. Unpinned and unbounded, these two calls spent five
# minutes in i/o timeouts against it before saying anything.
e2e_diagnose() {
  local ns="$1"
  local kc=(kubectl --request-timeout=10s)
  if [[ -n "${CLUSTER:-}" ]]; then
    kc+=(--context "kind-${CLUSTER}")
  fi
  echo "--- FAILURE: diagnostics before teardown ---" >&2
  "${kc[@]}" -n "${ns}" get pods -o wide >&2 2>&1 || true
  "${kc[@]}" -n "${ns}" get events --sort-by=.lastTimestamp >&2 2>&1 || true
  if [[ -n "${CONSOLE_LOG:-}" && -f "${CONSOLE_LOG}" ]]; then
    echo "--- console log (tail) ---" >&2
    tail -500 "${CONSOLE_LOG}" >&2 || true
  fi
}

# e2e_start_console builds the binary, starts it against the current kubectl
# context, and exports everything later calls need: CONSOLE_URL, a curl cookie
# jar, and the PID to kill on the way out.
#
# The binary prints its tokenized URL to stdout unconditionally -- whether or
# not the browser open was requested and whether or not it succeeded -- which
# is what makes it drivable from CI at all. --addr 127.0.0.1:0 lets the OS pick
# the port, so two scripts can never collide on one.
#
# Every AICRME_* knob a caller sets is inherited: this exports nothing of its
# own, so a caller that wants a dry-run Apply or a pinned snapshot node
# selector sets the variable before calling.
e2e_start_console() {
  local url token
  # Reused across a restart within one script, never recreated: the run
  # records live under it, keyed by the cluster's kube-system UID, and
  # recovery-on-connect is exactly what a restart test is checking. A fresh
  # directory each time would make every restart look like a first launch.
  CONSOLE_WORK_DIR="${CONSOLE_WORK_DIR:-$(mktemp -d)}"
  CONSOLE_LOG="$(mktemp -t aicrme-console-log.XXXXXX)"
  # A fresh jar every start, unlike the work directory: the token is one-shot
  # and the cookie dies with the process that minted it, so a restart is a new
  # session by construction.
  CONSOLE_JAR="$(mktemp -t aicrme-console-jar.XXXXXX)"
  export CONSOLE_WORK_DIR CONSOLE_LOG CONSOLE_JAR

  make -C "${REPO_ROOT}" build

  # --open=false because there is no browser on a runner, and an attempted
  # open there is a spawned process that never exits.
  AICRME_WORK_DIR="${CONSOLE_WORK_DIR}" "${REPO_ROOT}/bin/aicrme" \
    --addr 127.0.0.1:0 --open=false >"${CONSOLE_LOG}" 2>&1 &
  CONSOLE_PID=$!
  export CONSOLE_PID

  url=""
  for _ in $(seq 1 50); do
    url="$(grep -oE 'http://127\.0\.0\.1:[0-9]+/\?t=[A-Za-z0-9_-]+' "${CONSOLE_LOG}" | head -1 || true)"
    [[ -n "${url}" ]] && break
    # A console that died on startup (a work directory it cannot lock, a
    # missing helm) will never print a URL, and waiting the full ten seconds
    # to say so buries the reason it gives in its log.
    kill -0 "${CONSOLE_PID}" 2>/dev/null || break
    sleep 0.2
  done
  if [[ -z "${url}" ]]; then
    echo "console did not print a tokenized URL; log:" >&2
    cat "${CONSOLE_LOG}" >&2
    return 1
  fi

  CONSOLE_URL="${url%%/\?t=*}"
  export CONSOLE_URL
  token="${url##*t=}"

  # The one-shot exchange: everything afterwards authenticates by the cookie
  # this stores. Content-Type is mandatory -- see e2e_api on why.
  curl -fsS -c "${CONSOLE_JAR}" -X POST "${CONSOLE_URL}/api/session" \
    -H 'Content-Type: application/json' \
    -d "{\"token\":\"${token}\"}" >/dev/null
}

# e2e_connect points the console at the current kubectl context. Every
# cluster-touching route answers 409 until this succeeds.
e2e_connect() {
  local context
  context="$(kubectl config current-context)"
  echo "--- connecting the console to ${context}"
  e2e_api POST /api/connect -H 'Content-Type: application/json' \
    -d "{\"context\":\"${context}\"}" >/dev/null
}

# e2e_api issues an authenticated request and writes the body to stdout.
#
# Sec-Fetch-Site is set on every call because the server's same-origin wrapper
# (internal/api/server.go) rejects a mutating request that carries neither it
# nor Origin unless the Content-Type is one a plain HTML form cannot produce --
# and curl sends neither header. Setting it once here is what keeps every
# caller from having to remember.
e2e_api() {
  local method="$1" path="$2"
  shift 2
  curl -fsS -b "${CONSOLE_JAR}" -X "${method}" \
    -H 'Sec-Fetch-Site: same-origin' \
    "${CONSOLE_URL}${path}" "$@"
}

# e2e_api_status issues an authenticated request and writes only its HTTP
# status to stdout, for the assertions that are about the status rather than
# the body.
e2e_api_status() {
  local method="$1" path="$2"
  shift 2
  curl -s -o /dev/null -w '%{http_code}' -b "${CONSOLE_JAR}" -X "${method}" \
    -H 'Sec-Fetch-Site: same-origin' \
    "${CONSOLE_URL}${path}" "$@"
}

# e2e_stop_console ends the console and removes the credentials it was holding.
#
# SIGTERM rather than SIGKILL, and then a wait: the console reaps the deploy.sh
# process tree before returning, and killing it outright leaves a helm mid
# release-install, which strands that release in pending-install and blocks the
# next upgrade until someone runs `helm rollback` by hand.
e2e_stop_console() {
  [[ -n "${CONSOLE_PID:-}" ]] || return 0
  kill "${CONSOLE_PID}" 2>/dev/null || true
  wait "${CONSOLE_PID}" 2>/dev/null || true
  CONSOLE_PID=""
}

# e2e_console_cleanup stops the console and removes everything it left behind,
# including the work directory e2e_start_console deliberately preserves across
# a restart. For an EXIT trap, not between the two halves of a restart test.
e2e_console_cleanup() {
  e2e_stop_console
  rm -f "${CONSOLE_JAR:-}" "${CONSOLE_LOG:-}"
  [[ -n "${CONSOLE_WORK_DIR:-}" ]] && rm -rf "${CONSOLE_WORK_DIR}"
  CONSOLE_WORK_DIR=""
  return 0
}

# e2e_install_kwok installs the KWOK controller (CRDs plus the stage-fast
# rules that fake Ready/Running status with no real kubelet) at KWOK_VERSION
# and waits for its Deployment to roll out.
e2e_install_kwok() {
  echo "--- install KWOK controller (v${KWOK_VERSION})"
  curl -fsSL --connect-timeout 10 --max-time 60 \
    "https://github.com/kubernetes-sigs/kwok/releases/download/v${KWOK_VERSION}/kwok.yaml" \
    | kubectl apply --request-timeout=30s -f -
  curl -fsSL --connect-timeout 10 --max-time 60 \
    "https://github.com/kubernetes-sigs/kwok/releases/download/v${KWOK_VERSION}/stage-fast.yaml" \
    | kubectl apply --request-timeout=30s -f -
  kubectl -n kube-system rollout status deploy/kwok-controller --timeout=120s
}

# e2e_node_yaml emits one fake KWOK Node object to stdout. Field values
# reproduce AICR's kwok/profiles/eks/{system-m7i,p5-h100}.yaml node
# profiles and kwok/templates/nodes/node.yaml.tmpl's structure -- see
# e2e_apply_kwok_nodes's header comment for why they are inlined rather than
# sourced from there.
e2e_node_yaml() {
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

# e2e_apply_kwok_nodes creates 2 system + 4 GPU (H100) fake nodes and waits
# for the KWOK controller (installed by e2e_install_kwok, run by the caller)
# to bring them Ready.
e2e_apply_kwok_nodes() {
  local tmp i zone
  tmp="$(mktemp -d)"
  e2e_node_yaml system-0 system m7i.4xlarge "${KWOK_ZONES[0]}" 16 64Gi 100Gi 110 >"${tmp}/system-0.yaml"
  e2e_node_yaml system-1 system m7i.4xlarge "${KWOK_ZONES[1]}" 16 64Gi 100Gi 110 >"${tmp}/system-1.yaml"
  for i in 0 1 2 3; do
    zone="${KWOK_ZONES[$((i % 2))]}"
    e2e_node_yaml "gpu-${i}" accelerated p5.48xlarge "${zone}" 192 2048Gi 3800Gi 250 >"${tmp}/gpu-${i}.yaml"
  done
  kubectl apply -f "${tmp}/"
  kubectl wait --for=condition=Ready nodes -l type=kwok --timeout=60s
  rm -rf "${tmp}"
}
