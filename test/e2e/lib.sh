#!/usr/bin/env bash
# Shared helpers for the Kind-based e2e scripts (smoke.sh,
# discover-recommend.sh, apply-dryrun.sh). Sourced, not executed: callers
# already have `set -euo pipefail` and their own CLUSTER/NS/IMAGE in scope.

# Must track .settings.yaml's test_tools.kwok. Not read from that file at
# run time: these scripts have no yq dependency, and the tool list this task
# was scoped against (helm, kubectl, Docker, kind) does not include one.
KWOK_VERSION="${KWOK_VERSION:-0.8.0}"

KWOK_K8S_VERSION="v1.33.5"
KWOK_REGION="us-east-1"
KWOK_ZONES=(us-east-1a us-east-1b)

# e2e_diagnose dumps what a human needs before a failing cluster disappears:
# pod status, recent namespace events, and the console's own logs.
e2e_diagnose() {
  local ns="$1"
  echo "--- FAILURE: diagnostics before teardown ---" >&2
  kubectl -n "${ns}" get pods -o wide >&2 2>&1 || true
  kubectl -n "${ns}" get events --sort-by=.lastTimestamp >&2 2>&1 || true
  kubectl -n "${ns}" logs deploy/aicrme --all-containers --tail=500 >&2 2>&1 || true
}

# e2e_build_and_load_image builds the console image and loads it into the
# named Kind cluster so the chart install never reaches out to a registry.
e2e_build_and_load_image() {
  local cluster="$1" image="$2"
  make image IMAGE="${image}"
  kind load docker-image "${image}" --name "${cluster}"
}

# e2e_install_chart installs the aicrme chart from a locally-loaded image
# and waits for the Deployment to roll out.
e2e_install_chart() {
  local ns="$1" image="$2"
  helm install aicrme charts/aicrme -n "${ns}" --create-namespace \
    --set image.repository="${image%:*}" --set image.tag="${image#*:}" \
    --set image.pullPolicy=Never --wait --timeout 5m
  kubectl -n "${ns}" rollout status deploy/aicrme --timeout=120s
}

# e2e_admin_password reads the generated console password from the
# aicrme-auth Secret.
e2e_admin_password() {
  local ns="$1"
  kubectl -n "${ns}" get secret aicrme-auth -o jsonpath='{.data.password}' | base64 -d
}

# e2e_login authenticates against the console at addr ($1, host:port),
# storing the session cookie in the jar at $2, using the password read from
# the aicrme-auth Secret in namespace $3. Content-Type is mandatory: the
# same-origin CSRF middleware (internal/api/server.go) treats a mutating
# request carrying neither Origin nor Sec-Fetch-Site as same-origin only
# when its Content-Type is not one a plain HTML <form> can produce -- curl
# sends neither header, so omitting this would get the login rejected.
e2e_login() {
  local addr="$1" jar="$2" ns="$3"
  local password
  password="$(e2e_admin_password "${ns}")"
  curl -fsS -c "${jar}" -X POST "http://${addr}/api/login" \
    -H 'Content-Type: application/json' \
    -d "{\"username\":\"admin\",\"password\":\"${password}\"}"
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
