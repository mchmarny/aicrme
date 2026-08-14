#!/usr/bin/env bash
# Installs the chart on a Kind cluster and asserts the console serves a login
# page and rejects unauthenticated API access.
set -euo pipefail

CLUSTER="${CLUSTER:-aicrme-e2e}"
NS="${NS:-aicrme}"
IMAGE="${IMAGE:-aicrme:e2e}"

cleanup() { kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true; }
trap cleanup EXIT

kind create cluster --name "${CLUSTER}" --wait 120s
docker build -t "${IMAGE}" .
kind load docker-image "${IMAGE}" --name "${CLUSTER}"

helm install aicrme charts/aicrme -n "${NS}" --create-namespace \
  --set image.repository="${IMAGE%:*}" --set image.tag="${IMAGE#*:}" \
  --set image.pullPolicy=Never --wait --timeout 5m

kubectl -n "${NS}" rollout status deploy/aicrme --timeout=120s

kubectl -n "${NS}" port-forward svc/aicrme 18080:8080 >/dev/null 2>&1 &
PF_PID=$!
trap 'kill "${PF_PID}" 2>/dev/null || true; cleanup' EXIT
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
