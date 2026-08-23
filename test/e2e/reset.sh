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
# ConfigMap this console did not create. Reset deletes no namespaces, so this
# is no longer an emptiness check; the ConfigMap is the inverted input that
# proves the namespaces really are untouched rather than merely still named.
BYSTANDER_KEPT_NS="${BYSTANDER_KEPT_NS:-node-feature-discovery}"

# GANG_TIMEOUT is the production default rather than prove.sh's shortened
# 45s. prove.sh shortens it deliberately, to exercise the timeout path
# cheaply; this script is not testing that path, and a tight budget here
# only couples the teardown assertions to how fast kai-scheduler happens to
# come up. Measured: on the SECOND install of a run (a cluster this script
# has already reset once) the gang did not place inside 45s, where a first
# install places in about two seconds -- kai-scheduler is being reinstalled
# from scratch, re-registering its webhooks and re-electing a leader.
GANG_TIMEOUT="${GANG_TIMEOUT:-3m}"

RUN_ID=""
FAIL_RUN_ID=""
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
    helm list -A >&2 2>&1 || true
    echo "--- namespaces ---" >&2
    kubectl get ns >&2 2>&1 || true
    echo "--- run records ---" >&2
    # Both, and each tolerated as absent: a clean Reset DELETES the record,
    # so the first run is legitimately a 404 by the time the second one is
    # under test. Printing only RUN_ID (this script's first shape) meant the
    # diagnostics for a second-run failure were a bare 404.
    for id in "${RUN_ID:-}" "${FAIL_RUN_ID:-}"; do
      [[ -z "${id}" ]] && continue
      echo "run ${id}:" >&2
      curl -fsS --max-time 10 -b "${JAR}" "http://${ADDR}/api/runs/${id}" >&2 2>&1 || echo "  (no record)" >&2
      echo >&2
    done
  fi
  [[ -n "${PF_PID}" ]] && kill "${PF_PID}" 2>/dev/null
  rm -f "${JAR}"
  [[ -n "${CHART_DIR}" ]] && rm -rf "${CHART_DIR}"
  kubectl delete validatingwebhookconfiguration aicrme-e2e-block-ns-delete >/dev/null 2>&1 || true
  kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
}
trap 'ec=$?; cleanup "$ec"; exit "$ec"' EXIT

# helm_releases lists every release in a namespace, in every status, in a
# way that works under BOTH helm majors. `helm list --all` is the obvious
# spelling and is helm 3 only: helm 4 removed the flag from `list` (it lists
# every status by default) and rejects it with "unknown flag: --all", which
# is how this script silently reported the bystander gone on its first real
# run. The explicit status filters exist in both. internal/steps' own helm
# invocation carries the same set for the same reason.
helm_releases() {
  helm list --namespace "$1" \
    --deployed --failed --pending --superseded --uninstalled --uninstalling \
    --short 2>/dev/null || true
}

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
# One object, so the release has real content and `helm install --wait` has
# something to wait for. Deliberately NOT asserted on after the fact: the
# name collision means AICR's install upgrades this release with the real
# cert-manager chart, and a helm upgrade removes objects absent from the new
# chart. What survives -- and what assertion 1 checks -- is the release
# record itself.
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
  "AICRME_PROVE_GANG_TIMEOUT=${GANG_TIMEOUT}"
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
BYSTANDER_ALIVE="$(helm_releases "${BYSTANDER_NS}" | grep -cx "${BYSTANDER_RELEASE}" || true)"
echo "bystander releases matching ${BYSTANDER_RELEASE} in ${BYSTANDER_NS}: ${BYSTANDER_ALIVE}"
[[ "${BYSTANDER_ALIVE}" -eq 1 ]] \
  || fail "the bystander release ${BYSTANDER_NS}/${BYSTANDER_RELEASE} is gone -- Reset removed something a human installed"
# The release RECORD is what this asserts on, not the objects in it. The
# bystander's own ConfigMap is gone by now and that is correct: the name
# collision this test is built on means AICR's install ran
# `helm upgrade --install cert-manager` over it with the real chart, and an
# upgrade replaces a release's manifest -- an object absent from the new
# chart is removed by helm, during Apply, long before Reset runs. Asserting
# on it would be asserting on what the INSTALL did.
#
# The strong assertion is the run's own account: Reset must say it skipped
# the release, and say why. A release left standing because the teardown
# never ran at all would satisfy the check above but not this one.
SKIP_REASON="$(echo "${RESIDUE}" | jq -r --arg n "${BYSTANDER_RELEASE}" --arg ns "${BYSTANDER_NS}" \
  '[.items[]? | select(.kind == "release" and .name == $n and .namespace == $ns) | .skip // ""] | first // ""')"
echo "reported skip reason: ${SKIP_REASON:-<none>}"
[[ "${SKIP_REASON}" == *"already existed"* ]] \
  || fail "Reset did not report skipping ${BYSTANDER_RELEASE} for ownership (reason=${SKIP_REASON:-<none>})"
# And it must have been reported as skipped, not as removed.
BYSTANDER_REMOVED="$(echo "${RESIDUE}" | jq --arg n "${BYSTANDER_RELEASE}" --arg ns "${BYSTANDER_NS}" \
  '[.items[]? | select(.kind == "release" and .name == $n and .namespace == $ns and .removed == true)] | length')"
[[ "${BYSTANDER_REMOVED}" -eq 0 ]] || fail "Reset reported REMOVING the bystander release"
# Self-check on an inverted input: the same matcher against a release name
# this cluster cannot have must match nothing, or it is not discriminating.
BOGUS="$(helm_releases "${BYSTANDER_NS}" | grep -cx "__no-such-release__" || true)"
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
  if helm_releases "${namespace}" 2>/dev/null | grep -qx "${name}"; then
    REMAINING=$((REMAINING + 1))
    STILL_THERE="${STILL_THERE} ${namespace}/${name}"
  fi
done < <(echo "${INSTALLED_JSON}" | jq -r '.components[] | [.name, .namespace] | @tsv')
echo "releases this run created and still present: ${REMAINING} of ${INSTALLED_COUNT}"
[[ "${REMAINING}" -eq 0 ]] || fail "Reset left these behind:${STILL_THERE}"
# Self-check: the same loop must find the bystander present, or it is not
# actually looking at the cluster.
helm_releases "${BYSTANDER_NS}" | grep -qx "${BYSTANDER_RELEASE}" \
  || fail "assertion 2's matcher cannot see a release that IS present"
echo "assertion 2: PASS"

echo "--- assert 3: NO namespace is deleted, and every one is reported"
# Reset deletes no namespaces at all. Whoever applied the bundle owns the
# cleanup of what it applied, and this console is the bash deployer: a
# namespace left standing is one command for the operator, one deleted out
# from under something is unrecoverable. So the assertion is the inverse of
# what it used to be -- nothing is gone, and everything is named.
NS_DELETED=0
while IFS= read -r ns; do
  [[ -z "${ns}" ]] && continue
  if ! kubectl get namespace "${ns}" -o jsonpath='{.metadata.name}' >/dev/null 2>&1; then
    echo "namespace ${ns} was deleted by Reset" >&2
    NS_DELETED=$((NS_DELETED + 1))
  fi
done < <(echo "${RESIDUE}" | jq -r '.items[]? | select(.kind == "namespace") | .name')
echo "namespaces Reset deleted: ${NS_DELETED} (want 0)"
[[ "${NS_DELETED}" -eq 0 ]] || fail "Reset deleted ${NS_DELETED} namespaces -- it must only report them"

# The bystander ConfigMap is the inverted-input check: it proves the
# namespaces are genuinely untouched rather than merely present.
KEPT_CM="$(kubectl -n "${BYSTANDER_KEPT_NS}" get configmap somebody-elses-config -o jsonpath='{.metadata.name}' 2>/dev/null || true)"
[[ "${KEPT_CM}" == "somebody-elses-config" ]] || fail "the bystander ConfigMap in ${BYSTANDER_KEPT_NS} is gone"

# Every namespace is named with a reason, and the ones this run CREATED are
# flagged so the console can offer the cleanup command. A report the operator
# cannot act on is not a report.
NS_TOTAL="$(echo "${RESIDUE}" | jq '[.items[]? | select(.kind == "namespace")] | length')"
NS_NAMED="$(echo "${RESIDUE}" | jq '[.items[]? | select(.kind == "namespace" and (.skip // "") != "")] | length')"
NS_CREATED="$(echo "${RESIDUE}" | jq '[.items[]? | select(.kind == "namespace" and .created == true)] | length')"
echo "namespaces reported: ${NS_TOTAL}, each with a reason: ${NS_NAMED}, flagged as created by this run: ${NS_CREATED}"
[[ "${NS_TOTAL}" -gt 0 ]] || fail "no namespaces were reported at all -- the inventory is empty"
[[ "${NS_NAMED}" -eq "${NS_TOTAL}" ]] || fail "only ${NS_NAMED} of ${NS_TOTAL} namespaces carry a reason"
[[ "${NS_CREATED}" -gt 0 ]] || fail "no namespace is flagged created -- the console can offer no cleanup command"

# ...and the one that PREDATES the install must not be flagged, or the
# console would tell the operator to delete a namespace it deliberately
# refused to touch.
PRE="$(echo "${RESIDUE}" | jq --arg ns "${BYSTANDER_KEPT_NS}" \
  '[.items[]? | select(.kind == "namespace" and .name == $ns and .created == true)] | length')"
[[ "${PRE}" -eq 0 ]] || fail "${BYSTANDER_KEPT_NS} predates the install but is flagged as created by this run"
echo "assertion 3: PASS"

echo "--- assert 4: a FAILED reset blocks Start, Retry and Discard, and Reset again succeeds"
FAIL_RUN_ID="$(drive_to_installed)"
echo "second run id: ${FAIL_RUN_ID}"
STATE=""
for _ in $(seq 1 240); do
  STATE="$(run_state "${FAIL_RUN_ID}")"
  [[ "${STATE}" == "active" || "${STATE}" == "done" || "${STATE}" == "failed" ]] && break
  sleep 10
done
# active OR failed, deliberately. What this assertion needs is a run that
# INSTALLED something and can therefore be reset; whether its Prove gang
# then placed is a different feature's business (prove.sh's), and requiring
# it here would fail this assertion for a reason unrelated to teardown.
# engine.Reset accepts StateFailed and StateActive alike.
SECOND_INSTALLED="$(run_json "${FAIL_RUN_ID}" | jq '[.components[] | select(.status == "installed")] | length')"
echo "second run: state=${STATE} with ${SECOND_INSTALLED} components installed"
[[ "${STATE}" == "active" || "${STATE}" == "failed" ]] || {
  echo "second run record: $(run_json "${FAIL_RUN_ID}" | jq -c '{state,phase,error}')" >&2
  fail "the second run reached neither active nor failed (state=${STATE})"
}
[[ "${SECOND_INSTALLED}" -gt 0 ]] || {
  echo "second run record: $(run_json "${FAIL_RUN_ID}" | jq -c '{state,phase,error}')" >&2
  fail "the second run installed nothing, so there is no teardown to fail"
}

# The webhook goes up HERE, after the install and before the teardown --
# not before the install, which is where this script first put it. It
# refuses every namespace DELETE cluster-wide with failurePolicy: Fail, and
# deploy.sh's own preflight deletes terminating namespaces, so a webhook
# live during Apply breaks the install rather than the reset. Only a real
# API server can produce this at all: admission is exactly what a fake
# clientset does not run.
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
  echo "SKIP: this recipe left no console-created namespace for the webhook to block; assertion 4 not exercised" >&2
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
  echo "assertion 4: PASS"
fi

echo "--- assert 5: the console accepts a new run afterward"
# LAST, deliberately -- numbered 5 because it runs fifth, not because it
# is the fifth thing the plan listed. It is the only assertion that leaves a run in flight,
# and there is no way to clean one up: a run parked at a decision gate is
# live, and engine.Discard refuses a live run with 409 -- correctly, since
# discarding one would nil e.current out from under its own goroutine.
# Running this last means the run it starts needs no cleanup at all; the
# EXIT trap deletes the cluster underneath it.
#
# The point of a clean Reset: every gate that was refusing new runs is
# cleared -- the record is deleted, recoveredPending is cleared, and no
# residue guard is set. A 409 here would mean the operator has to discard
# something before they can demo again, which is the wedge Reset exists to
# remove.
NEW_RUN="$(post /api/runs)"
NEW_ID="$(echo "${NEW_RUN}" | jq -r '.id')"
[[ -n "${NEW_ID}" && "${NEW_ID}" != "null" ]] || fail "POST /api/runs was refused after a clean reset: ${NEW_RUN}"
NEW_STATE="$(await_state "${NEW_ID}" awaiting_decision 90)"
[[ "${NEW_STATE}" == "awaiting_decision" ]] \
  || fail "the new run reached ${NEW_STATE}, expected a decision gate -- the console is not usable again"
echo "a new run was accepted and reached its first gate: ${NEW_ID}"
echo "assertion 5: PASS"

echo
echo "PASS: Reset removed what the run created and left what it did not"