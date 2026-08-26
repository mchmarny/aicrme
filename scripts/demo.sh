#!/usr/bin/env bash
# Stands up a local, browser-usable aicrme demo.
#
# WHY THIS EXISTS
# Everything needed to run this console locally already existed -- but only
# inside test/e2e/*.sh, which tear the cluster down on exit and drive the API
# with curl rather than leaving something to click. So the one thing nobody
# could do was actually use it. This is that path: same cluster shape, no
# teardown, and it ends by running the console in the foreground.
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
PORT="${PORT:-8080}"
AGENT_NODE_LABEL="aicrme.demo/agent=true"
# Its own work directory, not the operator's ~/.aicrme: the demo's run records
# are about a throwaway KWOK cluster, and `make demo-down` deletes that cluster
# while the records keyed on it would otherwise sit in the real one forever.
WORK_DIR="${WORK_DIR:-${REPO_ROOT}/.demo-work}"

usage() {
  cat <<EOF
usage: ./scripts/demo.sh [up|down|status]
       (or: make demo | make demo-down | make demo-status)

  up      create the cluster, build the binary, and run the console
  down    delete the cluster and its demo work directory
  status  show whether the demo cluster is running

env overrides: CLUSTER=${CLUSTER} PORT=${PORT} WORK_DIR=${WORK_DIR}
EOF
}

demo_up() {
  if kind get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
    echo "cluster '${CLUSTER}' already exists -- reusing it."
    echo "run 'make demo-down' first for a clean start."
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

  echo "==> pinning the snapshot agent to a worker"
  kubectl label node "${CLUSTER}-worker" "${AGENT_NODE_LABEL}" --overwrite

  echo "==> building the console"
  make -C "${REPO_ROOT}" build

  cat <<EOF

================================================================
  starting the aicrme console

  It opens your browser at a tokenized loopback URL and prints
  the same URL below. Pick the '${CLUSTER}' context on the
  Connect screen -- it arrives preselected.

  The console then starts at Discover. It snapshots the cluster,
  asks you two questions (intent, platform) and installs the
  resolved recipe for real -- roughly 5 minutes for 14 actions.

  It then runs the reference workload: a 2-pod gang at 8 GPUs each,
  placed by kai-scheduler. The run ends ACTIVE, with the workload
  left running and "Stop workload" as the only way out.

  Validate is deferred on measurement (docs/phase-2-handoff.md).
  Nothing here executes a container -- see DEMO.md's "What this
  demo cannot show you" for what the placement does and does not
  prove.

  Ctrl-C stops the console. The cluster stays up until
  'make demo-down'.
================================================================

EOF

  # In the foreground, and exec'd: the console holds the operator's
  # credentials for as long as it runs and reaps the deploy.sh process tree on
  # the way out, so the Ctrl-C that ends the demo has to reach it directly
  # rather than killing an intermediate shell. AICRME_SNAPSHOT_* are
  # simulated-cluster knobs -- see internal/steps/discover.go on why a KWOK
  # cluster cannot satisfy AICR's own GPU auto-targeting.
  AICRME_WORK_DIR="${WORK_DIR}" \
    AICRME_SNAPSHOT_NODE_SELECTOR="${AGENT_NODE_LABEL}" \
    AICRME_SNAPSHOT_REQUESTS='cpu=200m' \
    AICRME_APPLY_DRY_RUN=false \
    exec "${REPO_ROOT}/bin/aicrme" \
    --addr "127.0.0.1:${PORT}" \
    --context "kind-${CLUSTER}"
}

demo_down() {
  if kind get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
    echo "==> deleting cluster '${CLUSTER}'"
    kind delete cluster --name "${CLUSTER}"
  else
    echo "cluster '${CLUSTER}' is not running"
  fi
  # The run records are keyed on that cluster's kube-system UID, so once it is
  # gone nothing can ever recover them -- leaving them behind would only
  # accumulate directories no console will look in again.
  if [[ -d "${WORK_DIR}" ]]; then
    echo "==> removing the demo work directory ${WORK_DIR}"
    rm -rf "${WORK_DIR}"
  fi
}

demo_status() {
  if ! kind get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
    echo "demo is not running (no cluster '${CLUSTER}')"
    return 0
  fi
  echo "cluster:  ${CLUSTER}"
  # The work-directory lock is the console's own "one process per work
  # directory" guard (internal/console/lock.go), so its presence is the same
  # answer the binary itself would give.
  if [[ -f "${WORK_DIR}/lock" ]]; then
    echo "console:  running, at http://127.0.0.1:${PORT} (see its terminal for the tokenized URL)"
  else
    echo "console:  not running -- re-run 'make demo'"
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
