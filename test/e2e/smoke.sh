#!/usr/bin/env bash
# Installs the chart on a Kind cluster and asserts the console serves a login
# page and rejects unauthenticated API access.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "${SCRIPT_DIR}/lib.sh"

CLUSTER="${CLUSTER:-aicrme-e2e}"
NS="${NS:-aicrme}"
IMAGE="${IMAGE:-aicrme:e2e}"
JAR="$(mktemp -t aicrme-smoke-jar.XXXXXX)"
PF_PID=""
ec=0

cleanup() {
  local exit_code="$1"
  if [[ -n "${PF_PID}" ]]; then
    kill "${PF_PID}" 2>/dev/null || true
  fi
  if [[ "${exit_code}" -ne 0 ]]; then
    e2e_diagnose "${NS}"
  fi
  rm -f "${JAR}"
  kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
}
# Exit status is captured before cleanup runs anything else, and re-asserted
# with an explicit exit at the end: bash otherwise reports the EXIT trap's
# own last command status (from `kind delete cluster ... || true`, always 0)
# as the script's exit code, which would hide a real failure from CI.
trap 'ec=$?; cleanup "$ec"; exit "$ec"' EXIT

kind create cluster --name "${CLUSTER}" --wait 120s
e2e_build_and_load_image "${CLUSTER}" "${IMAGE}"
e2e_install_chart "${NS}" "${IMAGE}"

kubectl -n "${NS}" port-forward svc/aicrme 18080:8080 >/dev/null 2>&1 &
PF_PID=$!
sleep 3

# Body captured before matching, not piped into `grep -q`: under pipefail,
# grep exiting early on a match closes the pipe and curl fails with EPIPE
# (exit 23), turning a passing assertion into a failing script. Small bodies
# fit the pipe buffer and hide it; a larger SPA would not.
echo "--- GET / serves the SPA"
SPA_BODY="$(curl -fsS http://localhost:18080/)"
grep -q "aicrme" <<<"${SPA_BODY}"

echo "--- GET /healthz is public"
[[ "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:18080/healthz)" == "200" ]]

echo "--- GET /api/events is 401 without a session"
[[ "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:18080/api/events)" == "401" ]]

echo "--- login then POST /api/runs succeeds"
e2e_login "localhost:18080" "${JAR}" "${NS}"
RUN_BODY="$(curl -fsS -b "${JAR}" -X POST http://localhost:18080/api/runs)"
grep -q '"id"' <<<"${RUN_BODY}"

echo "PASS: smoke test green"
