#!/usr/bin/env bash
# End-to-end: Reset removes what the run created and leaves what it did not.
#
# WHY THIS EXISTS, AND WHY THE UNIT SUITE CANNOT REPLACE IT
# internal/teardown's ownership rule is UNFALSIFIABLE against a fake
# clientset. Nothing in a fake cluster distinguishes a helm release this
# console created from one `helm upgrade --install` adopted -- there is no
# helm at all, so the pre-Apply snapshot every skip decision rests on is
# stubbed, and a test asserting "the bystander survived" would pass equally
# against an implementation that uninstalled nothing and one that uninstalled
# everything.
#
# So this script seeds a real helm release, under a name the recipe also
# uses, in a namespace the recipe also installs into, BEFORE the console
# installs anything. Reset must leave it standing and say so. That single
# assertion is what the whole ownership design exists for; everything else
# here is the machinery that makes it meaningful.
#
# WHAT THIS CANNOT ASSERT, STATED RATHER THAN IMPLIED
# The GPU workload never executes -- KWOK synthesizes pod completion without
# starting a container (see prove.sh's own note). What is asserted is what
# the cluster HOLDS before and after the teardown, which is exactly the
# claim Reset makes.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=lib.sh
source "${SCRIPT_DIR}/lib.sh"
cd "${REPO_ROOT}"

CLUSTER="${CLUSTER:-aicrme-e2e-reset}"
NS="${NS:-aicrme}"
IMAGE="${IMAGE:-aicrme:e2e-reset}"
PORT="${PORT:-18081}"
ADDR="127.0.0.1:${PORT}"
AGENT_NODE_LABEL="aicrme.e2e/agent=true"

# BYSTANDER_RELEASE is deliberately a name the AICR recipe also installs, in
# the namespace it installs it into. A bystander with a name nothing else
# uses would be spared by any implementation, including one that matched on
# nothing at all -- the collision is the entire point.
BYSTANDER_RELEASE="${BYSTANDER_RELEASE:-cert-manager}"
BYSTANDER_NS="${BYSTANDER_NS:-cert-manager}"
# BYSTANDER_KEPT_NS is a namespace the recipe also uses, seeded with a
# ConfigMap this console did not create, so the emptiness half of the rule
# has something to refuse.
BYSTANDER_KEPT_NS="${BYSTANDER_KEPT_NS:-node-feature-discovery}"

JAR="$(mktemp -t aicrme-reset-jar.XXXXXX)"
PF_PID=""
CHART_DIR=""
# Assigned by the EXIT trap; declared here for the same reason every other
# script in this directory declares it (shellcheck SC2154).
ec=0

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

cleanup() {
  local exit_code="$1"
  if [[ "${exit_code}" -ne 0 ]]; then
    e2e_diagnose "${NS}"
    echo "--- helm releases, all namespaces ---" >&2
    helm list -A --all >&2 2>&1 || true
    echo "--- namespaces ---" >&2
    kubectl get ns >&2 2>&1 || true
    echo "--- run record ---" >&2
    curl -fsS --max-time 10 -b "${JAR}" "http://${ADDR}/api/runs/${RUN_ID:-none}" >&2 2>&1 || true
  fi
  [[ -n "${PF_PID}" ]] && kill "${PF_PID}" 2>/dev/null
  rm -f "${JAR}"
  [[ -n "${CHART_DIR}" ]] && rm -rf "${CHART_DIR}"
  kubectl delete validatingwebhookconfiguration aicrme-e2e-block-ns-delete >/dev/null 2>&1 || true
  kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
}
trap 'ec=$?; cleanup "$ec"; exit "$ec"' EXIT

run_json() {
  curl -fsS --max-time 10 -b "${JAR}" "http://${ADDR}/api/runs/$1" 2>/dev/null || true
}

run_state() {
  run_json "$1" | jq -r '.state // "unreachable"'
}

post() {
  curl -fsS -b "${JAR}" -X POST "http://${ADDR}$1" -H 'Content-Type: application/json' "${@:2}"
}

post_status() {
  curl -s -o /dev/null -w '%{http_code}' -b "${JAR}" -X POST "http://${ADDR}$1" \
    -H 'Content-Type: application/json' "${@:2}"
}

delete_status() {
  curl -s -o /dev/null -w '%{http_code}' -b "${JAR}" -X DELETE "http://${ADDR}$1"
}

await_state() {
  local id="$1" want="$2" tries="$3" state=""
  for _ in $(seq 1 "${tries}"); do
    state="$(run_state "${id}")"
    case "${state}" in
      "${want}"|failed|done|active) break ;;
    esac
    sleep 5
  done
  echo "${state}"
}

# await_terminal waits out a teardown. Separate from await_state because
# `resetting` is a state await_state would break out of only by accident:
# its case arm lists the terminal states, and resetting is not one.
await_terminal() {
  local id="$1" tries="$2" state=""
  for _ in $(seq 1 "${tries}"); do
    state="$(run_state "${id}")"
    case "${state}" in
      failed|done|active|unreachable) break ;;
    esac
    sleep 5
  done
  echo "${state}"
}

drive_to_installed() {
  local created id state pending=""
  created="$(post /api/runs)"
  id="$(echo "${created}" | jq -r '.id')"
  [[ -n "${id}" && "${id}" != "null" ]] || fail "no run id in POST /api/runs response: ${created}"

  state="$(await_state "${id}" awaiting_decision 90)"
  [[ "${state}" == "awaiting_decision" ]] || fail "run ${id} did not reach the recommend gate (state=${state})"
  post "/api/runs/${id}/decide" -d '{"intent":"training","platform":"kubeflow"}' >/dev/null

  for _ in $(seq 1 60); do
    pending="$(run_json "${id}" | jq -c '.pending')"
    [[ "${pending}" == '["apply"]' ]] && break
    sleep 3
  done
  [[ "${pending}" == '["apply"]' ]] || fail "run ${id} did not reach the confirm gate (pending=${pending})"
  post "/api/runs/${id}/decide" -d '{"apply":"yes"}' >/dev/null

  echo "${id}"
}

echo "--- create Kind cluster (control-plane + 3 workers)"
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

# ---------------------------------------------------------------------------
# The bystanders, seeded BEFORE the console installs anything. This ordering
# is load-bearing: the whole ownership rule turns on the pre-Apply snapshot,
# and a release created after it would be indistinguishable from one this run
# installed -- which is the correct behaviour, and would make this script
# assert the opposite of what it means to.
# ---------------------------------------------------------------------------
echo "--- seed a REAL helm release named ${BYSTANDER_RELEASE} in ${BYSTANDER_NS}"
CHART_DIR="$(mktemp -d -t aicrme-bystander.XXXXXX)"
mkdir -p "${CHART_DIR}/templates"
cat >"${CHART_DIR}/Chart.yaml" <<'EOF'
apiVersion: v2
name: bystander
description: A release this console did not create and must not remove.
type: application
version: 0.1.0
appVersion: "1"
EOF
cat >"${CHART_DIR}/templates/configmap.yaml" <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: bystander-marker
data:
  owner: "a human, not this console"
EOF
helm install "${BYSTANDER_RELEASE}" "${CHART_DIR}" \
  --namespace "${BYSTANDER_NS}" --create-namespace --wait --timeout 2m

echo "--- seed a bystander ConfigMap in ${BYSTANDER_KEPT_NS} (a namespace the recipe also uses)"
kubectl create namespace "${BYSTANDER_KEPT_NS}" 2>/dev/null || true
kubectl -n "${BYSTANDER_KEPT_NS}" create configmap somebody-elses-config \
  --from-literal=owner="a human, not this console"

echo "--- build and load PRODUCTION console image"
e2e_build_and_load_image "${CLUSTER}" "${IMAGE}"
echo "--- install chart"
e2e_install_chart "${NS}" "${IMAGE}"

echo "--- pin the snapshot agent to a real worker, dry-run OFF"
kubectl label node "${CLUSTER}-worker" "${AGENT_NODE_LABEL}" --overwrite
kubectl -n "${NS}" set env deploy/aicrme \
  "AICRME_SNAPSHOT_NODE_SELECTOR=${AGENT_NODE_LABEL}" \
  'AICRME_SNAPSHOT_REQUESTS=cpu=200m' \
  'AICRME_APPLY_DRY_RUN=false' \
  'AICRME_PROVE_GANG_TIMEOUT=45s'
kubectl -n "${NS}" rollout status deploy/aicrme --timeout=180s

kubectl -n "${NS}" port-forward "svc/aicrme" "${PORT}:8080" >/dev/null 2>&1 &
PF_PID=$!
sleep 3

echo "--- login"
e2e_login "${ADDR}" "${JAR}" "${NS}"

echo "--- drive the arc: discover, recommend, bundle, apply (real), prove"
RUN_ID="$(drive_to_installed)"
echo "run id: ${RUN_ID}"

STATE=""
for i in $(seq 1 240); do
  STATE="$(run_state "${RUN_ID}")"
  [[ "${STATE}" == "active" || "${STATE}" == "done" || "${STATE}" == "failed" ]] && break
  if [[ $((i % 6)) -eq 0 ]]; then
    echo "[$(date -u +%H:%M:%S)] state=${STATE} phase=$(run_json "${RUN_ID}" | jq -r '.phase')"
  fi
  sleep 10
done
[[ "${STATE}" == "active" ]] || {
  echo "run did not reach state=active: $(run_json "${RUN_ID}" | jq -c '{state,phase,error}')" >&2
  fail "the arc did not end at a running workload"
}
echo "run reached state=active"

INSTALLED_JSON="$(run_json "${RUN_ID}")"
INSTALLED_RELEASES="$(echo "${INSTALLED_JSON}" | jq -r '[.components[].name] | join(",")')"
INSTALLED_COUNT="$(echo "${INSTALLED_JSON}" | jq '.components | length')"
echo "the run installed ${INSTALLED_COUNT} components: ${INSTALLED_RELEASES}"
[[ "${INSTALLED_COUNT}" -gt 0 ]] || fail "the run recorded no components -- nothing below could be meaningful"

# Every component row must carry a namespace, or Reset cannot address its
# release at all and would skip the lot for a reason that looks like
# ownership but is really a dropped field.
NS_MISSING="$(echo "${INSTALLED_JSON}" | jq '[.components[] | select((.namespace // "") == "")] | length')"
[[ "${NS_MISSING}" -eq 0 ]] || fail "${NS_MISSING} component rows carry no namespace"

# The snapshot must have SEEN the bystander. If it did not, the skip below
# would still happen for some other reason and this script would certify a
# rule it never exercised.
SNAPSHOT_SAW_BYSTANDER="$(echo "${INSTALLED_JSON}" \
  | jq --arg n "${BYSTANDER_RELEASE}" --arg ns "${BYSTANDER_NS}" \
    '[.ownership.releases[]? | select(.name == $n and .namespace == $ns)] | length')"
[[ "${SNAPSHOT_SAW_BYSTANDER}" -eq 1 ]] \
  || fail "the pre-Apply snapshot did not record ${BYSTANDER_RELEASE} in ${BYSTANDER_NS} (matched ${SNAPSHOT_SAW_BYSTANDER}) -- the ownership rule below would be vacuous"
echo "pre-Apply snapshot recorded the bystander: 1 match"

echo "--- RESET"
RESET_STATUS="$(post_status "/api/runs/${RUN_ID}/reset" -d '{"confirm":"reset"}')"
[[ "${RESET_STATUS}" == "200" ]] || fail "POST reset returned ${RESET_STATUS}, want 200"
FINAL_STATE="$(await_terminal "${RUN_ID}" 180)"
echo "reset settled at state=${FINAL_STATE}"

# The record is deleted on a clean teardown, so the run may legitimately be
# unreachable here. Capture the residue while it still exists if it does not.
RESET_JSON="$(run_json "${RUN_ID}")"
RESIDUE="$(echo "${RESET_JSON}" | jq -c '.residue // {}')"
echo "residue: ${RESIDUE}"

echo "--- assert 1: the bystander release survived, and the run said it skipped it"
BYSTANDER_ALIVE="$(helm list -n "${BYSTANDER_NS}" --all -q | grep -cx "${BYSTANDER_RELEASE}" || true)"
echo "bystander releases matching ${BYSTANDER_RELEASE} in ${BYSTANDER_NS}: ${BYSTANDER_ALIVE}"
[[ "${BYSTANDER_ALIVE}" -eq 1 ]] \
  || fail "the bystander release ${BYSTANDER_NS}/${BYSTANDER_RELEASE} is gone -- Reset removed something a human installed"
# Its contents too: an uninstall-then-reinstall would leave the name present
# and the ConfigMap's owner rewritten.
BYSTANDER_OWNER="$(kubectl -n "${BYSTANDER_NS}" get configmap bystander-marker -o jsonpath='{.data.owner}' 2>/dev/null || true)"
[[ "${BYSTANDER_OWNER}" == "a human, not this console" ]] \
  || fail "the bystander's own ConfigMap did not survive intact (owner=${BYSTANDER_OWNER:-<gone>})"
# Self-check on an inverted input: the same matcher against a release name
# this cluster cannot have must match nothing, or it is not discriminating.
BOGUS="$(helm list -n "${BYSTANDER_NS}" --all -q | grep -cx "__no-such-release__" || true)"
[[ "${BOGUS}" -eq 0 ]] || fail "assertion 1's matcher matched a release that cannot exist"
echo "assertion 1: PASS"

echo "--- assert 2: every release this run created is gone"
REMAINING=0
STILL_THERE=""
while IFS=$'\t' read -r name namespace; do
  [[ -z "${name}" ]] && continue
  # The bystander is the one name that legitimately survives; assertion 1
  # already covered it.
  if [[ "${name}" == "${BYSTANDER_RELEASE}" && "${namespace}" == "${BYSTANDER_NS}" ]]; then
    continue
  fi
  if helm list -n "${namespace}" --all -q 2>/dev/null | grep -qx "${name}"; then
    REMAINING=$((REMAINING + 1))
    STILL_THERE="${STILL_THERE} ${namespace}/${name}"
  fi
done < <(echo "${INSTALLED_JSON}" | jq -r '.components[] | [.name, .namespace] | @tsv')
echo "releases this run created and still present: ${REMAINING} of ${INSTALLED_COUNT}"
[[ "${REMAINING}" -eq 0 ]] || fail "Reset left these behind:${STILL_THERE}"
# Self-check: the same loop must find the bystander present, or it is not
# actually looking at the cluster.
helm list -n "${BYSTANDER_NS}" --all -q | grep -qx "${BYSTANDER_RELEASE}" \
  || fail "assertion 2's matcher cannot see a release that IS present"
echo "assertion 2: PASS"

echo "--- assert 3: namespaces this run created are gone; one holding a bystander is kept"
KEPT="$(kubectl get namespace "${BYSTANDER_KEPT_NS}" -o jsonpath='{.metadata.name}' 2>/dev/null || true)"
[[ "${KEPT}" == "${BYSTANDER_KEPT_NS}" ]] \
  || fail "${BYSTANDER_KEPT_NS} was deleted -- it held a ConfigMap this console did not create"
KEPT_CM="$(kubectl -n "${BYSTANDER_KEPT_NS}" get configmap somebody-elses-config -o jsonpath='{.metadata.name}' 2>/dev/null || true)"
[[ "${KEPT_CM}" == "somebody-elses-config" ]] || fail "the bystander ConfigMap in ${BYSTANDER_KEPT_NS} is gone"
# And it is NAMED, not silently spared. A skip nobody can read is a skip the
# operator cannot act on.
NAMED="$(echo "${RESIDUE}" | jq --arg ns "${BYSTANDER_KEPT_NS}" \
  '[.items[]? | select(.kind == "namespace" and .name == $ns and (.skip // "") != "")] | length')"
echo "namespaces reported as skipped with a stated reason: ${NAMED}"
[[ "${NAMED}" -ge 1 || "${FINAL_STATE}" == "done" ]] \
  || fail "${BYSTANDER_KEPT_NS} was kept but not named in the residue"
echo "assertion 3: PASS"

echo "--- assert 4: the console accepts a new run afterward"
# The point of a clean Reset: every gate that was refusing new runs is
# cleared -- the record is deleted, recoveredPending is cleared, and no
# residue guard is set. A 409 here would mean the operator has to discard
# something before they can demo again, which is the wedge Reset exists to
# remove.
NEW_RUN="$(post /api/runs)"
NEW_ID="$(echo "${NEW_RUN}" | jq -r '.id')"
[[ -n "${NEW_ID}" && "${NEW_ID}" != "null" ]] || fail "POST /api/runs was refused after a clean reset: ${NEW_RUN}"
echo "a new run was accepted: ${NEW_ID}"
# Cancelled and discarded immediately: assertion 5 drives its own run, and a
# live one would make its POST /api/runs 409 for a reason unrelated to what
# it is testing.
NEW_STATE="$(await_state "${NEW_ID}" awaiting_decision 90)"
[[ "${NEW_STATE}" == "awaiting_decision" || "${NEW_STATE}" == "failed" ]] \
  || fail "the new run reached ${NEW_STATE}, expected a decision gate"
DISCARDED="$(delete_status "/api/runs/${NEW_ID}")"
[[ "${DISCARDED}" == "204" || "${DISCARDED}" == "200" ]] \
  || fail "could not discard the probe run (status ${DISCARDED})"
echo "assertion 4: PASS"

echo "--- assert 5: a FAILED reset blocks Start, Retry and Discard, and Reset again succeeds"
# A webhook that refuses every namespace DELETE. Only a real API server can
# produce this: admission is exactly what a fake clientset does not run. The
# service does not exist and failurePolicy is Fail, so every delete is
# rejected without anything having to be listening.
kubectl apply -f - <<EOF
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: aicrme-e2e-block-ns-delete
webhooks:
  - name: block.namespaces.aicrme.e2e
    admissionReviewVersions: ["v1"]
    sideEffects: None
    failurePolicy: Fail
    timeoutSeconds: 5
    clientConfig:
      service:
        name: no-such-service
        namespace: ${NS}
        path: /reject
    rules:
      - apiGroups: [""]
        apiVersions: ["v1"]
        operations: ["DELETE"]
        resources: ["namespaces"]
        scope: Cluster
EOF

FAIL_RUN_ID="$(drive_to_installed)"
echo "second run id: ${FAIL_RUN_ID}"
STATE=""
for _ in $(seq 1 240); do
  STATE="$(run_state "${FAIL_RUN_ID}")"
  [[ "${STATE}" == "active" || "${STATE}" == "done" || "${STATE}" == "failed" ]] && break
  sleep 10
done
[[ "${STATE}" == "active" ]] || fail "the second run did not reach state=active (state=${STATE})"

post_status "/api/runs/${FAIL_RUN_ID}/reset" -d '{"confirm":"reset"}' >/dev/null
BLOCKED_STATE="$(await_terminal "${FAIL_RUN_ID}" 180)"
BLOCKED_JSON="$(run_json "${FAIL_RUN_ID}")"
INCOMPLETE="$(echo "${BLOCKED_JSON}" | jq -r '.residue.incomplete // false')"
echo "blocked reset settled at state=${BLOCKED_STATE} incomplete=${INCOMPLETE}"

if [[ "${INCOMPLETE}" != "true" ]]; then
  # The webhook blocks namespace deletion only. A recipe whose charts all
  # ship their own Namespace manifests would have nothing left for the
  # namespace step to delete, so there would be no failure to provoke. Say
  # so rather than asserting on a condition that never arose.
  echo "SKIP: this recipe left no console-created namespace for the webhook to block; assertion 5 not exercised" >&2
else
  START_STATUS="$(post_status /api/runs)"
  RETRY_STATUS="$(post_status "/api/runs/${FAIL_RUN_ID}/retry")"
  DISCARD_STATUS="$(delete_status "/api/runs/${FAIL_RUN_ID}")"
  echo "with an incomplete teardown: start=${START_STATUS} retry=${RETRY_STATUS} discard=${DISCARD_STATUS}"
  [[ "${START_STATUS}" == "409" ]] || fail "Start returned ${START_STATUS} over a half-torn-down cluster, want 409"
  [[ "${RETRY_STATUS}" == "409" ]] || fail "Retry returned ${RETRY_STATUS} over a half-torn-down cluster, want 409"
  [[ "${DISCARD_STATUS}" == "409" ]] || fail "Discard returned ${DISCARD_STATUS}, want 409 -- it would delete the only residue inventory"

  # The remedy stays reachable: blocking every operation that could resolve
  # the residue is the operator dead end the guard exists to prevent.
  kubectl delete validatingwebhookconfiguration aicrme-e2e-block-ns-delete
  AGAIN_STATUS="$(post_status "/api/runs/${FAIL_RUN_ID}/reset" -d '{"confirm":"reset"}')"
  [[ "${AGAIN_STATUS}" == "200" ]] || fail "a second Reset returned ${AGAIN_STATUS}, want 200 -- the remedy must stay reachable"
  AGAIN_STATE="$(await_terminal "${FAIL_RUN_ID}" 180)"
  AGAIN_INCOMPLETE="$(run_json "${FAIL_RUN_ID}" | jq -r '.residue.incomplete // false')"
  echo "second reset settled at state=${AGAIN_STATE} incomplete=${AGAIN_INCOMPLETE}"
  [[ "${AGAIN_INCOMPLETE}" != "true" ]] || fail "the second Reset did not clear the guard"
  FINAL_START="$(post_status /api/runs)"
  [[ "${FINAL_START}" == "200" ]] || fail "Start returned ${FINAL_START} after the residue was cleared, want 200"
  echo "assertion 5: PASS"
fi

echo
echo "PASS: Reset removed what the run created and left what it did not"
