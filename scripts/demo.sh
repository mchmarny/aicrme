#!/usr/bin/env bash
# Stands up a local, browser-usable aicrme demo and LEAVES IT RUNNING.
#
# WHY THIS EXISTS
# Everything needed to run this console locally already existed -- but only
# inside test/e2e/*.sh, which tear the cluster down on exit and drive the API
# with curl rather than leaving something to click. So the one thing nobody
# could do was actually use it. This is that path: same cluster shape, same
# chart, same image, no teardown, and it prints the URL and password.
#
# It deliberately reuses test/e2e/lib.sh instead of reimplementing the setup.
# That library encodes traps that cost real time to find, and a second copy
# would drift from it and rediscover them:
#   - a plain KWOK cluster CANNOT resolve a recipe. With no GPU nodes there is
#     no derivable accelerator, so every intent/platform pair fails AICR's
#     coverage post-condition. The simulated H100s are a prerequisite, not
#     decoration.
#   - kind leaves a SINGLE-node cluster's control-plane untainted and taints it
#     the moment any worker exists, so the snapshot agent must be pinned to a
#     labelled worker. Getting this wrong surfaces as Discover hanging for ten
#     minutes with a scheduling error that points nowhere near cluster shape.
#
# NOT a test. Nothing here asserts; it is for a human with a browser.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
# shellcheck source=../test/e2e/lib.sh
source "${REPO_ROOT}/test/e2e/lib.sh"

CLUSTER="${CLUSTER:-aicrme-demo}"
NS="${NS:-aicrme}"
IMAGE="${IMAGE:-aicrme:demo}"
PORT="${PORT:-8080}"
AGENT_NODE_LABEL="aicrme.demo/agent=true"
PF_FILE="${TMPDIR:-/tmp}/aicrme-demo-portforward.pid"

usage() {
  cat <<EOF
usage: $(basename "$0") [up|down|status]

  up      create the cluster, install the console, print the URL and password
  down    delete the cluster and stop the port-forward
  status  show whether the demo is running and how to reach it

env overrides: CLUSTER=${CLUSTER} NS=${NS} IMAGE=${IMAGE} PORT=${PORT}
EOF
}

stop_port_forward() {
  if [[ -f "${PF_FILE}" ]]; then
    local pid
    pid="$(cat "${PF_FILE}" 2>/dev/null || true)"
    if [[ -n "${pid}" ]]; then
      kill "${pid}" 2>/dev/null || true
    fi
    rm -f "${PF_FILE}"
  fi
}

demo_up() {
  if kind get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
    echo "cluster '${CLUSTER}' already exists -- reusing it."
    echo "run '$(basename "$0") down' first for a clean start."
  else
    echo "==> creating cluster '${CLUSTER}' (control-plane + 3 workers)"
    # Three workers, matching test/e2e/apply-real.sh. Worker count is the
    # scheduling budget, not just memory: kind reports each node's allocatable
    # CPU as the HOST's core count, and cutting this to one took a real install
    # from ~5 minutes to over 40 without completing (2026-08-19).
    kind create cluster --name "${CLUSTER}" --config - <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
  - role: worker
  - role: worker
EOF
  fi

  echo "==> installing KWOK and the simulated H100 nodes"
  e2e_install_kwok
  e2e_apply_kwok_nodes

  echo "==> building and loading the console image"
  e2e_build_and_load_image "${CLUSTER}" "${IMAGE}"

  if helm status aicrme -n "${NS}" >/dev/null 2>&1; then
    echo "==> console already installed, upgrading"
    helm upgrade aicrme "${REPO_ROOT}/charts/aicrme" -n "${NS}" \
      --set image.repository="${IMAGE%:*}" --set image.tag="${IMAGE#*:}" \
      --set image.pullPolicy=Never --wait --timeout 5m
  else
    echo "==> installing the console"
    e2e_install_chart "${NS}" "${IMAGE}"
  fi

  echo "==> pinning the snapshot agent to a worker, real install ON"
  kubectl label node "${CLUSTER}-worker" "${AGENT_NODE_LABEL}" --overwrite
  # AICRME_SNAPSHOT_* are deliberately env-only knobs and are NOT chart values:
  # they exist for simulated clusters, and putting them in values.yaml would
  # advertise them as a supported way to run the product.
  kubectl -n "${NS}" set env deploy/aicrme \
    "AICRME_SNAPSHOT_NODE_SELECTOR=${AGENT_NODE_LABEL}" \
    'AICRME_SNAPSHOT_REQUESTS=cpu=200m' \
    'AICRME_APPLY_DRY_RUN=false'
  kubectl -n "${NS}" rollout status deploy/aicrme --timeout=180s

  stop_port_forward
  kubectl -n "${NS}" port-forward "svc/aicrme" "${PORT}:8080" >/dev/null 2>&1 &
  echo $! >"${PF_FILE}"
  sleep 3

  local password
  password="$(e2e_admin_password "${NS}")"

  cat <<EOF

================================================================
  aicrme demo is up

  URL       http://localhost:${PORT}
  username  admin
  password  ${password}

  The console starts at Discover. It will snapshot the cluster,
  then ask you two questions (intent, platform) and install the
  resolved recipe for real -- roughly 5 minutes for 14 actions.

  Validate and Prove are not built yet (Phase 3), so the arc ends
  at a completed Apply.

  stop it:  $(basename "$0") down
================================================================
EOF
}

demo_down() {
  stop_port_forward
  if kind get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
    echo "==> deleting cluster '${CLUSTER}'"
    kind delete cluster --name "${CLUSTER}"
  else
    echo "cluster '${CLUSTER}' is not running"
  fi
}

demo_status() {
  if ! kind get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
    echo "demo is not running (no cluster '${CLUSTER}')"
    return 0
  fi
  echo "cluster:  ${CLUSTER}"
  kubectl -n "${NS}" get deploy aicrme 2>/dev/null || echo "console not installed"
  if [[ -f "${PF_FILE}" ]] && kill -0 "$(cat "${PF_FILE}")" 2>/dev/null; then
    echo "console:  http://localhost:${PORT} (port-forward running)"
    echo "password: $(e2e_admin_password "${NS}" 2>/dev/null || echo '(unavailable)')"
  else
    echo "port-forward is not running; re-run '$(basename "$0") up'"
  fi
}

case "${1:-up}" in
  up) demo_up ;;
  down) demo_down ;;
  status) demo_status ;;
  -h | --help | help) usage ;;
  *)
    usage
    exit 1
    ;;
esac
