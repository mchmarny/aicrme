#!/usr/bin/env bash
# demo-remote.sh — install the console onto a cluster that already exists.
#
# WHY THIS IS NOT scripts/demo.sh
# demo.sh owns its cluster: it runs `kind create cluster`, `kind load
# docker-image`, and installs with --set image.pullPolicy=Never so the chart
# never reaches a registry. None of that works on a managed cluster. This
# script creates nothing, deletes nothing, and pulls the image from ghcr.io
# like any real deployment would.
#
# It is the Phase 4 path: a real GKE/EKS/AKS cluster with real GPU nodes,
# where the point is to find out what the demo does on hardware.
#
# WHAT IT ASSUMES
#   - kubectl is already pointed at the target cluster, and you have admin on it
#   - you can push to IMAGE_REPO (docker login ghcr.io)
#   - the pushed package is PUBLIC, or PULL_SECRET names a Secret you created
#
# USAGE
#   scripts/demo-remote.sh up      # build, push, install, port-forward
#   scripts/demo-remote.sh status  # URL and password
#   scripts/demo-remote.sh down    # uninstall the console; the CLUSTER SURVIVES
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_ROOT}"

NS="${NS:-aicrme}"
PORT="${PORT:-8080}"
IMAGE_REPO="${IMAGE_REPO:-ghcr.io/mchmarny/aicrme/aicrme}"
# Default tag is the current commit, so two runs never race on :latest and a
# console pod's image can always be traced back to a commit.
TAG="${TAG:-$(git rev-parse --short HEAD)}"
IMAGE="${IMAGE_REPO}:${TAG}"
PULL_SECRET="${PULL_SECRET:-}"
SKIP_PUSH="${SKIP_PUSH:-false}"

die() { echo "ERROR: $*" >&2; exit 1; }

# A managed cluster is the whole point, so refuse a kind context outright.
# Running this against Kind would push an image the local cluster then pulls
# from the internet -- slow, confusing, and silently different from what
# demo.sh does.
guard_context() {
  local ctx
  ctx="$(kubectl config current-context 2>/dev/null)" || die "no current kubectl context"
  case "${ctx}" in
    kind-*) die "current context is ${ctx}; use 'make demo' for Kind, this script is for managed clusters" ;;
  esac
  echo "==> target context: ${ctx}"
  kubectl get nodes -o wide 2>/dev/null | head -8
}

# report_gpu_nodes exists because Discover's behaviour depends entirely on it,
# and the failure it prevents is a ten-minute timeout. The agent Job tolerates
# nvidia.com/gpu (internal/steps/discover.go) but nothing else, so a GPU pool
# carrying some OTHER taint strands it.
report_gpu_nodes() {
  echo "==> GPU nodes and their taints"
  local found
  found="$(kubectl get nodes -o json 2>/dev/null | jq -r '
    .items[]
    | select((.status.allocatable // {})["nvidia.com/gpu"] != null)
    | "  \(.metadata.name)  gpus=\(.status.allocatable["nvidia.com/gpu"])  taints=\([(.spec.taints // [])[] | "\(.key)=\(.value // "")\(":" + .effect)"] | join(","))"
  ')"
  if [[ -z "${found}" ]]; then
    echo "  none advertise nvidia.com/gpu yet."
    echo "  That is EXPECTED on a fresh cluster: the device plugin ships with the"
    echo "  GPU stack this console is about to install. Discover reports it as a gap."
  else
    echo "${found}"
    if echo "${found}" | grep -q "taints=.*[a-z]" && ! echo "${found}" | grep -q "nvidia.com/gpu"; then
      echo "  ⚠ a GPU node carries a taint that is NOT nvidia.com/gpu. The snapshot"
      echo "    agent tolerates only nvidia.com/gpu, so Discover may sit Pending"
      echo "    until it times out. Set AICRME_SNAPSHOT_NODE_SELECTOR, or add the"
      echo "    toleration in internal/steps/discover.go."
    fi
  fi
}

up() {
  guard_context
  report_gpu_nodes

  if [[ "${SKIP_PUSH}" != "true" ]]; then
    # Build for the CLUSTER's architecture, not the laptop's. A managed
    # cluster is almost always amd64 and the machine building the image
    # increasingly is not, and the mismatch surfaces as a CrashLoopBackOff
    # with "exec format error" -- which says nothing about its own cause.
    local arch
    arch="$(kubectl get nodes -o json 2>/dev/null \
      | jq -r '[.items[].metadata.labels["kubernetes.io/arch"]] | unique | join(",")')"
    case "${arch}" in
      amd64|arm64) ;;
      *) die "cluster reports architecture '${arch}'; set PLATFORM= by hand and re-run with SKIP_PUSH=false" ;;
    esac
    echo "==> building ${IMAGE} for linux/${arch} (cluster arch)"
    make image IMAGE="${IMAGE}" PLATFORM="linux/${arch}"
    echo "==> pushing ${IMAGE}"
    docker push "${IMAGE}" \
      || die "push failed — run 'docker login ghcr.io' first, or set SKIP_PUSH=true if it is already pushed"
  else
    echo "==> SKIP_PUSH=true, using ${IMAGE} as already pushed"
  fi

  local extra=()
  [[ -n "${PULL_SECRET}" ]] && extra+=(--set "imagePullSecrets[0].name=${PULL_SECRET}")

  # Pin the console to a GPU-FREE node pool when one exists.
  #
  # The recipe installs nodewright-customizations, which on EKS and AKS
  # restarts the GPU nodes. A console sitting on one is killed part-way
  # through the install that rebooted it, and the run it was driving has to be
  # recovered and retried. GKE's COS image does not restart, so this is
  # portability insurance -- but the cost of being wrong is a broken demo on
  # the two clouds we have not tried yet.
  #
  # Written to a values file rather than --set: the pool label keys are
  # cloud.google.com/gke-nodepool and eks.amazonaws.com/nodegroup, and helm's
  # --set requires every dot in a key to be backslash-escaped, which is a
  # quoting trap nobody should have to debug at demo time.
  local pool_key pool_val values_file=""
  read -r pool_key pool_val <<<"$(kubectl get nodes -o json 2>/dev/null | jq -r '
    [ .items[]
      | select(((.status.allocatable // {})["nvidia.com/gpu"]) == null)
      | .metadata.labels
      | to_entries[]
      | select(.key == "cloud.google.com/gke-nodepool"
            or .key == "eks.amazonaws.com/nodegroup"
            or .key == "agentpool")
    ] | if length > 0 then "\(.[0].key) \(.[0].value)" else "" end')"
  if [[ -n "${pool_key}" && -n "${pool_val}" ]]; then
    values_file="$(mktemp -t aicrme-values.XXXXXX.yaml)"
    printf 'nodeSelector:\n  %s: "%s"\n' "${pool_key}" "${pool_val}" >"${values_file}"
    extra+=(-f "${values_file}")
    echo "==> pinning the console to GPU-free pool ${pool_val} (${pool_key})"
  else
    echo "==> no GPU-free node pool found; the console will schedule anywhere."
    echo "    On EKS/AKS that risks it landing on a GPU node that"
    echo "    nodewright-customizations later restarts, mid-install."
  fi

  echo "==> installing the chart into ${NS}"
  helm upgrade --install aicrme charts/aicrme -n "${NS}" --create-namespace \
    --set image.repository="${IMAGE_REPO}" \
    --set image.tag="${TAG}" \
    --set image.pullPolicy=Always \
    "${extra[@]}" \
    --wait --timeout 5m
  kubectl -n "${NS}" rollout status deploy/aicrme --timeout=180s

  status
}

status() {
  kubectl -n "${NS}" get deploy aicrme >/dev/null 2>&1 || die "the console is not installed in ${NS}"
  echo
  echo "==> console is up"
  echo "    kubectl -n ${NS} port-forward svc/aicrme ${PORT}:8080"
  echo "    then open http://127.0.0.1:${PORT}"
  echo "    user:     admin"
  echo -n "    password: "
  kubectl -n "${NS}" get secret aicrme-auth -o jsonpath='{.data.password}' | base64 -d
  echo
  echo
  echo "    This console runs with cluster-admin and installs privileged"
  echo "    DaemonSets and CRDs. It is a demo and eval tool."
}

down() {
  echo "==> uninstalling the console from ${NS} — the CLUSTER IS NOT TOUCHED"
  helm uninstall aicrme -n "${NS}" 2>/dev/null || true
  echo "Anything a RUN installed is still there. Use the console's own Reset for that,"
  echo "or AICR's tools/cleanup for a clean slate — see DEMO.md."
}

case "${1:-up}" in
  up) up ;;
  status) status ;;
  down) down ;;
  *) die "usage: $0 [up|status|down]" ;;
esac
