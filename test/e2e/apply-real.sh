#!/usr/bin/env bash
# End-to-end: the FULL Apply chain against a KWOK-simulated cluster with
# --dry-run OFF -- a real `helm upgrade --install` of every component in the
# resolved recipe, asserted through to state=done and workload convergence.
#
# WHY THIS EXISTS ALONGSIDE apply-dryrun.sh, WHICH IT DOES NOT REPLACE
# apply-dryrun.sh pins the marker grammar and the known dry-run ceiling
# (network-operator at 3/14 on nfd's CRD) by name, which is what catches
# upstream drift cheaply. It cannot assert the chain COMPLETES, because
# --dry-run never registers the CRDs later components need: dry-run validates
# each chart against the cluster as it is, not as the previous component left
# it. That ceiling was documented for two phases as the limit of simulated
# verification. It is not -- it is a limit of --dry-run specifically. A real
# install registers those CRDs and the chain runs to completion in about six
# minutes, verified twice locally before this file existed.
#
# So: dryrun is the fast grammar/drift check, real is the full-chain gate.
#
# WHY THE CLUSTER HAS WORKERS, WHICH apply-dryrun.sh's DOES NOT
# A real install actually creates every component's workloads, and they need
# somewhere to run. The KWOK nodes carry kwok.x-k8s.io/node=fake:NoSchedule,
# so everything that does not tolerate that taint lands on real nodes.
#
# The non-obvious consequence: kind leaves a SINGLE-node cluster's
# control-plane untainted (otherwise nothing could schedule), but taints it as
# soon as workers exist. So the control-plane node selector apply-dryrun.sh
# uses to pin the snapshot agent silently stops working here -- the agent
# matches a node it cannot tolerate and Discover times out after ten minutes
# with a scheduling error that does not obviously point back at cluster shape.
# Hence the explicit worker label below. Do not "simplify" it back.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

CLUSTER="${CLUSTER:-aicrme-e2e-real}"
NS="${NS:-aicrme}"
IMAGE="${IMAGE:-aicrme:e2e-real}"
PORT="${PORT:-18083}"
ADDR="localhost:${PORT}"
AGENT_NODE_LABEL="aicrme.e2e/agent=true"
# Workloads are judged only after this settle window. kai-scheduler's
# ReplicaSet transiently reports FailedCreate ("serviceaccount not found")
# while its own chart is still applying, and converges shortly after --
# observed on both verification runs. Judging immediately would pin a race.
SETTLE_SECONDS="${SETTLE_SECONDS:-180}"
JAR="$(mktemp -t aicrme-real-jar.XXXXXX)"
PF_PID=""
ec=0

dump_recent_events() {
  echo "--- last 80 SSE events ---" >&2
  set +e
  curl -fsS -b "${JAR}" --max-time 5 "http://${ADDR}/api/events?since=0" 2>/dev/null \
    | sed -n 's/^data: //p' | tail -n 80 >&2
  set -e
}

fail_run() {
  local run_json="$1"
  echo "run failed: $(echo "${run_json}" | jq -r '.error // "unknown error"')" >&2
  echo "full run: ${run_json}" >&2
  exit 1
}

cleanup() {
  local exit_code="$1"
  if [[ "${exit_code}" -ne 0 ]]; then
    e2e_diagnose "${NS}"
    echo "--- helm releases ---" >&2
    helm list -A >&2 2>&1 || true
    echo "--- workloads short of desired ---" >&2
    kubectl get deploy,ds,sts -A >&2 2>&1 || true
    dump_recent_events
  fi
  if [[ -n "${PF_PID}" ]]; then
    kill "${PF_PID}" 2>/dev/null || true
  fi
  rm -f "${JAR}"
  kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
}
trap 'ec=$?; cleanup "$ec"; exit "$ec"' EXIT

echo "--- create Kind cluster (control-plane + 3 workers; see header)"
KIND_CFG="$(mktemp -t aicrme-kind.XXXXXX.yaml)"
cat >"${KIND_CFG}" <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
  - role: worker
  - role: worker
EOF
kind create cluster --name "${CLUSTER}" --config "${KIND_CFG}" --wait 180s
rm -f "${KIND_CFG}"

e2e_install_kwok
echo "--- apply simulated H100 nodes (2 system + 4x p5.48xlarge)"
e2e_apply_kwok_nodes

echo "--- build and load PRODUCTION console image"
e2e_build_and_load_image "${CLUSTER}" "${IMAGE}"
echo "--- install chart"
e2e_install_chart "${NS}" "${IMAGE}"

echo "--- label a worker for the snapshot agent (see header: workers taint the control-plane)"
kubectl label node "${CLUSTER}-worker" "${AGENT_NODE_LABEL}" --overwrite

echo "--- pin the snapshot agent onto that worker, dry-run OFF"
kubectl -n "${NS}" set env deploy/aicrme \
  "AICRME_SNAPSHOT_NODE_SELECTOR=${AGENT_NODE_LABEL}" \
  'AICRME_SNAPSHOT_REQUESTS=cpu=200m' \
  'AICRME_APPLY_DRY_RUN=false'
kubectl -n "${NS}" rollout status deploy/aicrme --timeout=180s

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

echo "--- decide {intent:training, platform:kubeflow}"
curl -fsS -b "${JAR}" -X POST "http://${ADDR}/api/runs/${RUN_ID}/decide" \
  -H 'Content-Type: application/json' \
  -d '{"intent":"training","platform":"kubeflow"}' >/dev/null

echo "--- poll until the confirm gate (Recommend + Bundle complete)"
STATE=""
for _ in $(seq 1 60); do
  RUN_JSON="$(curl -fsS -b "${JAR}" "http://${ADDR}/api/runs/${RUN_ID}")"
  STATE="$(echo "${RUN_JSON}" | jq -r '.state')"
  [[ "${STATE}" == "awaiting_decision" || "${STATE}" == "failed" ]] && break
  sleep 3
done
[[ "${STATE}" == "failed" ]] && fail_run "${RUN_JSON}"
PENDING="$(echo "${RUN_JSON}" | jq -cS '.pending')"
[[ "${PENDING}" == '["apply"]' ]] || {
  echo "confirm gate did not fire as expected: pending=${PENDING}" >&2
  exit 1
}

echo "--- decide {apply:yes} -- REAL install begins"
curl -fsS -b "${JAR}" -X POST "http://${ADDR}/api/runs/${RUN_ID}/decide" \
  -H 'Content-Type: application/json' \
  -d '{"apply":"yes"}' >/dev/null

# Both verification runs installed all 14 components in 6m02s. 40 minutes
# covers a full retry storm (deploy.sh backs off 5s/20s/45s/80s/120s over five
# attempts, WITHOUT --best-effort) plus a slow runner's image pulls, which are
# the variable this script adds over the dry-run job -- a real install pulls
# every component's images, which --dry-run never does.
echo "--- poll until done or failed (Apply)"
STATE=""
for _ in $(seq 1 240); do
  RUN_JSON="$(curl -fsS -b "${JAR}" "http://${ADDR}/api/runs/${RUN_ID}")"
  STATE="$(echo "${RUN_JSON}" | jq -r '.state')"
  [[ "${STATE}" == "done" || "${STATE}" == "failed" ]] && break
  sleep 10
done

[[ "${STATE}" == "failed" ]] && fail_run "${RUN_JSON}"
[[ "${STATE}" == "done" ]] || {
  echo "Apply did not reach a terminal state within the deadline (state=${STATE})" >&2
  fail_run "${RUN_JSON}"
}
echo "run reached state=done"

# Derived from the run, never hardcoded: the component count moves with every
# AICR release, and the handoff is explicit that assertions must follow the
# pinned version rather than a number someone wrote down once.
echo "--- assert every component in the projection installed"
TOTAL="$(echo "${RUN_JSON}" | jq '.components // [] | length')"
[[ "${TOTAL}" -gt 0 ]] || {
  echo "run carries no component projection at all" >&2
  fail_run "${RUN_JSON}"
}
NOT_INSTALLED="$(echo "${RUN_JSON}" | jq -r '[.components[] | select(.status != "installed")] | length')"
[[ "${NOT_INSTALLED}" -eq 0 ]] || {
  echo "${NOT_INSTALLED} of ${TOTAL} components did not reach status=installed:" >&2
  echo "${RUN_JSON}" | jq -r '.components[] | select(.status != "installed") | "  \(.index)/\(.total) \(.name) \(.status)"' >&2
  fail_run "${RUN_JSON}"
}
echo "all ${TOTAL} components report installed"

# helm reporting success is not the same claim as the workloads running. A
# Deployment whose pods were never CREATED has no pods, so asserting on pod
# phase cannot see it -- assert desired-vs-ready instead.
echo "--- settle ${SETTLE_SECONDS}s, then assert workload convergence"
sleep "${SETTLE_SECONDS}"
UNCONVERGED="$(
  {
    kubectl get deploy -A -o json | jq -r '
      .items[] | select((.status.readyReplicas // 0) != (.spec.replicas // 0))
      | "deployment \(.metadata.namespace)/\(.metadata.name) ready=\(.status.readyReplicas // 0)/\(.spec.replicas // 0)"'
    kubectl get ds -A -o json | jq -r '
      .items[] | select((.status.numberReady // 0) != (.status.desiredNumberScheduled // 0))
      | "daemonset \(.metadata.namespace)/\(.metadata.name) ready=\(.status.numberReady // 0)/\(.status.desiredNumberScheduled // 0)"'
    kubectl get sts -A -o json | jq -r '
      .items[] | select((.status.readyReplicas // 0) != (.spec.replicas // 0))
      | "statefulset \(.metadata.namespace)/\(.metadata.name) ready=\(.status.readyReplicas // 0)/\(.spec.replicas // 0)"'
  } | grep -v '^$' || true
)"
[[ -z "${UNCONVERGED}" ]] || {
  echo "workloads short of desired after ${SETTLE_SECONDS}s:" >&2
  echo "${UNCONVERGED}" >&2
  exit 1
}

echo "PASS: apply-real e2e green (run ${RUN_ID}; ${TOTAL} components installed for real, state=done, every workload at desired replicas)"
