#!/usr/bin/env bash
# Starts the console against a Kind cluster and asserts it serves the SPA,
# refuses unauthenticated API access, and gates every run route until a
# cluster has been chosen.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "${SCRIPT_DIR}/lib.sh"

CLUSTER="${CLUSTER:-aicrme-e2e}"
NS="${NS:-aicrme}"
ec=0

cleanup() {
  local exit_code="$1"
  if [[ "${exit_code}" -ne 0 ]]; then
    e2e_diagnose "${NS}"
  fi
  e2e_console_cleanup
  kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
}
# Exit status is captured before cleanup runs anything else, and re-asserted
# with an explicit exit at the end: bash otherwise reports the EXIT trap's
# own last command status (from `kind delete cluster ... || true`, always 0)
# as the script's exit code, which would hide a real failure from CI.
trap 'ec=$?; cleanup "$ec"; exit "$ec"' EXIT

kind create cluster --name "${CLUSTER}" --wait 120s
e2e_start_console

# Body captured before matching, not piped into `grep -q`: under pipefail,
# grep exiting early on a match closes the pipe and curl fails with EPIPE
# (exit 23), turning a passing assertion into a failing script. Small bodies
# fit the pipe buffer and hide it; a larger SPA would not.
echo "--- GET / serves the SPA"
SPA_BODY="$(curl -fsS "${CONSOLE_URL}/")"
grep -q "aicrme" <<<"${SPA_BODY}"

echo "--- GET /healthz is public"
[[ "$(curl -s -o /dev/null -w '%{http_code}' "${CONSOLE_URL}/healthz")" == "200" ]]

# No cookie at all, deliberately: the launch token is the only way in, and a
# request that never exchanged one must not reach any API route.
echo "--- GET /api/events is 401 without a session"
[[ "$(curl -s -o /dev/null -w '%{http_code}' "${CONSOLE_URL}/api/events")" == "401" ]]

# The connect gate, with a live session: authenticated is not the same as
# connected, and a run route that answered here would be acting on no cluster
# in particular.
echo "--- POST /api/runs is 409 before a cluster is chosen"
[[ "$(e2e_api_status POST /api/runs)" == "409" ]]

echo "--- GET /api/contexts answers before any connection"
CONTEXTS_BODY="$(e2e_api GET /api/contexts)"
grep -q "kind-${CLUSTER}" <<<"${CONTEXTS_BODY}"

e2e_connect

echo "--- POST /api/runs succeeds once connected"
RUN_BODY="$(e2e_api POST /api/runs)"
grep -q '"id"' <<<"${RUN_BODY}"

# The reload path: a restored tab has the cookie and no memory of the cluster,
# and the connection is single-assignment, so this is what stops it being sent
# back to a Connect screen that would refuse it.
echo "--- GET /api/cluster reports the established connection"
CLUSTER_BODY="$(e2e_api GET /api/cluster)"
grep -q "kind-${CLUSTER}" <<<"${CLUSTER_BODY}"

echo "PASS: smoke test green"
