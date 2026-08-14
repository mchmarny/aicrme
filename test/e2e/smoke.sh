#!/usr/bin/env bash
# Installs the chart on a Kind cluster and asserts the console serves a login
# page and rejects unauthenticated API access.
set -euo pipefail

CLUSTER="${CLUSTER:-aicrme-e2e}"
NS="${NS:-aicrme}"
IMAGE="${IMAGE:-aicrme:e2e}"
PF_PID=""
ec=0

# On failure, dump what a human needs before the cluster disappears: pod
# status, recent namespace events, and the console's own logs. Task 13
# extends this script, so a failing run that leaves nothing behind costs
# real debugging time later.
diagnose() {
  echo "--- FAILURE: diagnostics before teardown ---" >&2
  kubectl -n "${NS}" get pods -o wide >&2 2>&1 || true
  kubectl -n "${NS}" get events --sort-by=.lastTimestamp >&2 2>&1 || true
  kubectl -n "${NS}" logs deploy/aicrme --all-containers --tail=500 >&2 2>&1 || true
}

cleanup() {
  local exit_code="$1"
  if [[ -n "${PF_PID}" ]]; then
    kill "${PF_PID}" 2>/dev/null || true
  fi
  if [[ "${exit_code}" -ne 0 ]]; then
    diagnose
  fi
  kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
}
# Exit status is captured before cleanup runs anything else, and re-asserted
# with an explicit exit at the end: bash otherwise reports the EXIT trap's
# own last command status (from `kind delete cluster ... || true`, always 0)
# as the script's exit code, which would hide a real failure from CI.
trap 'ec=$?; cleanup "$ec"; exit "$ec"' EXIT

kind create cluster --name "${CLUSTER}" --wait 120s
make image IMAGE="${IMAGE}"
kind load docker-image "${IMAGE}" --name "${CLUSTER}"

helm install aicrme charts/aicrme -n "${NS}" --create-namespace \
  --set image.repository="${IMAGE%:*}" --set image.tag="${IMAGE#*:}" \
  --set image.pullPolicy=Never --wait --timeout 5m

kubectl -n "${NS}" rollout status deploy/aicrme --timeout=120s

kubectl -n "${NS}" port-forward svc/aicrme 18080:8080 >/dev/null 2>&1 &
PF_PID=$!
sleep 3

echo "--- GET / serves the SPA"
curl -fsS http://localhost:18080/ | grep -q "aicrme"

echo "--- GET /healthz is public"
[[ "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:18080/healthz)" == "200" ]]

echo "--- GET /api/events is 401 without a session"
[[ "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:18080/api/events)" == "401" ]]

echo "--- login then POST /api/runs succeeds"
PASSWORD="$(kubectl -n "${NS}" get secret aicrme-auth -o jsonpath='{.data.password}' | base64 -d)"
curl -fsS -c /tmp/aicrme.jar -X POST http://localhost:18080/api/login \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"admin\",\"password\":\"${PASSWORD}\"}"
curl -fsS -b /tmp/aicrme.jar -X POST http://localhost:18080/api/runs | grep -q '"id"'

echo "PASS: smoke test green"
