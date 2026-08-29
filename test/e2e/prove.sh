#!/usr/bin/env bash
# End-to-end: the Prove step against a KWOK-simulated cluster after a REAL
# install -- a gang actually scheduled by kai-scheduler, a console restart
# over a live workload, a Stop that fails and one that succeeds, and a gang
# that never places being cleaned up after.
#
# WHY THIS EXISTS ALONGSIDE THE UNIT TESTS, WHICH IT DOES NOT REPLACE
# internal/prove and internal/steps are covered against a fake clientset,
# which runs no scheduler, no admission, no controllers and no finalizers.
# Everything below is a claim only a real API server can settle, and the four
# defects this script was written against are all of exactly that shape --
# each one green in the unit suite and broken on a cluster:
#
#   1. the workload tolerated neither taint its own GPU nodes carry, so
#      kai-scheduler answered "no nodes with enough resources were found:
#      4 node(s) had untolerated taint(s)" and the gang never placed;
#   2. placement was polled every 20ms for three minutes, which client-go's
#      own rate limiter refused long before the deadline -- and that refusal,
#      not the timeout, became the run's recorded error;
#   3. a read that outlived the budget reported the plumbing rather than the
#      0/2 placement that was the actual diagnosis;
#   4. PlacedNodes excluded Succeeded pods, and KWOK marks a pod Succeeded in
#      the same second it binds it -- so every simulated run timed out over a
#      gang that had in fact been placed immediately.
#
# WHAT THIS CANNOT ASSERT, STATED RATHER THAN IMPLIED
# The gang's pods never execute. KWOK synthesizes their completion without
# starting a container, in the same second it binds them, which is also why
# no assertion below claims two nodes hold 8 GPUs each: the first member's
# resources are released before the second is bound, so co-location on one
# simulated node is normal here and is not evidence of a scheduling fault.
# What IS asserted is the decision -- who bound the gang, and to what.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=lib.sh
source "${SCRIPT_DIR}/lib.sh"
cd "${REPO_ROOT}"

CLUSTER="${CLUSTER:-aicrme-e2e-prove}"
NS="${NS:-aicrme}"
AGENT_NODE_LABEL="aicrme.e2e/agent=true"
PROVE_NS="aicrme-prove"
# Short enough that the gang-timeout assertion does not wait out the
# production default, long enough that a slow runner's Job creation is not
# mistaken for a placement failure. Read by internal/console's proveGangTimeout.
GANG_TIMEOUT="${GANG_TIMEOUT:-45s}"

# Assigned by the EXIT trap below; declared here for the same reason every
# other script in this directory declares it -- the trap's own assignment is
# invisible to shellcheck (SC2154).
ec=0

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

cleanup() {
  local exit_code="$1"
  if [[ "${exit_code}" -ne 0 ]]; then
    e2e_diagnose "${NS}"
    echo "--- prove namespace ---" >&2
    kubectl -n "${PROVE_NS}" get jobs,pods -o wide >&2 2>&1 || true
    kubectl -n "${PROVE_NS}" get events --sort-by=.lastTimestamp >&2 2>&1 || true
    echo "--- kai-scheduler ---" >&2
    kubectl -n kai-scheduler get pods -o wide >&2 2>&1 || true
    kubectl -n kai-scheduler logs -l app=kai-scheduler-default --tail=50 >&2 2>&1 || true
  fi
  e2e_console_cleanup
  kubectl delete validatingwebhookconfiguration aicrme-e2e-block-job-delete >/dev/null 2>&1 || true
  kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
}
trap 'ec=$?; cleanup "$ec"; exit "$ec"' EXIT

# run_json fetches a run, or empty on a transport failure -- callers decide
# whether a missed poll matters.
run_json() {
  e2e_api GET "/api/runs/$1" --max-time 10 2>/dev/null || true
}

run_state() {
  run_json "$1" | jq -r '.state // "unreachable"'
}

# await_state polls until the run reaches want (or any terminal state), then
# echoes the state it settled on. It never fails the script itself: every
# caller has a more specific complaint to make than "the poll ended".
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

# post is a mutating call that must succeed; post_status is one whose status
# code is the thing under test.
post() {
  e2e_api POST "$1" -H 'Content-Type: application/json' "${@:2}"
}

post_status() {
  e2e_api_status POST "$1" -H 'Content-Type: application/json' "${@:2}"
}

# drive_to_prove runs the whole arc up to the Prove step and echoes the run
# id. Every gate below is the same one apply-real.sh drives; only the
# terminal state this script waits for differs.
drive_to_prove() {
  local run_json id state
  # Retried while the engine answers 409. Reset is backgrounded server-side,
  # and the engine refuses a new run until that teardown finishes -- so the
  # first attempt after a reset legitimately conflicts, for as long as
  # uninstalling a whole recipe takes. On a cluster with nothing to tear down
  # the first attempt succeeds and this costs one call.
  run_json=""
  id=""
  for _ in $(seq 1 90); do
    run_json="$(post /api/runs 2>/dev/null || true)"
    id="$(echo "${run_json}" | jq -r '.id // empty' 2>/dev/null || true)"
    [[ -n "${id}" ]] && break
    sleep 5
  done
  [[ -n "${id}" && "${id}" != "null" ]] || fail "no run id in POST /api/runs response: ${run_json}"

  state="$(await_state "${id}" awaiting_decision 90)"
  [[ "${state}" == "awaiting_decision" ]] || fail "run ${id} did not reach the recommend gate (state=${state})"
  post "/api/runs/${id}/decide" -d '{"intent":"training","platform":"kubeflow"}' >/dev/null

  local pending=""
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

echo "--- start the console: agent pinned to a real worker, dry-run OFF, short gang budget"
kubectl label node "${CLUSTER}-worker" "${AGENT_NODE_LABEL}" --overwrite
export AICRME_SNAPSHOT_NODE_SELECTOR="${AGENT_NODE_LABEL}"
export AICRME_SNAPSHOT_REQUESTS='cpu=200m'
export AICRME_APPLY_DRY_RUN='false'
export AICRME_PROVE_GANG_TIMEOUT="${GANG_TIMEOUT}"
e2e_start_console
e2e_connect

echo "--- drive the arc: discover, recommend, bundle, apply (real), prove"
RUN_ID="$(drive_to_prove)"
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
RUN_JSON="$(run_json "${RUN_ID}")"
[[ "${STATE}" == "active" ]] || {
  echo "run did not reach state=active: $(echo "${RUN_JSON}" | jq -c '{state,phase,error}')" >&2
  fail "the arc did not end at a running workload"
}
echo "run reached state=active"

# A run that reached StateActive must name what it left running, or a restart
# has only the labels to go on -- which is the design, but the field is what
# the console shows the operator.
WORKLOAD="$(echo "${RUN_JSON}" | jq -c '.workload')"
[[ "$(echo "${WORKLOAD}" | jq -r '.name')" == "prove-${RUN_ID}" ]] \
  || fail "run.workload does not name this run's Job: ${WORKLOAD}"
echo "run.workload: ${WORKLOAD}"

echo "--- assert 1: kai-scheduler ran on a real Kind worker, not a simulated node"
# KAI's own components tolerate broadly. A controller that lands on a KWOK
# node receives a synthesized Ready without its container ever executing, so
# nothing would have scheduled anything and every assertion below it would be
# vacuously true over a gang bound by nobody.
KAI_PLACEMENT="$(kubectl -n kai-scheduler get pods \
  -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.nodeName}{"\n"}{end}')"
[[ -n "${KAI_PLACEMENT}" ]] || fail "no pods in the kai-scheduler namespace at all"
KAI_TOTAL="$(echo "${KAI_PLACEMENT}" | grep -c . || true)"
KAI_ON_WORKERS="$(echo "${KAI_PLACEMENT}" | grep -c " ${CLUSTER}-worker" || true)"
KAI_SCHEDULER_NODE="$(echo "${KAI_PLACEMENT}" | awk '/^kai-scheduler/ {print $2; exit}')"
echo "kai-scheduler namespace: ${KAI_ON_WORKERS} of ${KAI_TOTAL} pods on real Kind workers; scheduler itself on ${KAI_SCHEDULER_NODE:-<none>}"
[[ -n "${KAI_SCHEDULER_NODE}" ]] || fail "no kai-scheduler pod found -- nothing could have placed the gang"
[[ "${KAI_SCHEDULER_NODE}" == "${CLUSTER}-worker"* ]] \
  || fail "kai-scheduler is on ${KAI_SCHEDULER_NODE}, not a real Kind worker -- a KWOK node fakes Ready without executing"
[[ "${KAI_ON_WORKERS}" -eq "${KAI_TOTAL}" ]] \
  || fail "${KAI_TOTAL}-${KAI_ON_WORKERS} kai-scheduler pods are on simulated nodes"
# Self-check: the same matcher against a node prefix this cluster cannot have
# must match nothing, or it is not discriminating.
BOGUS_MATCHES="$(echo "${KAI_PLACEMENT}" | grep -c " __no-such-node__" || true)"
[[ "${BOGUS_MATCHES}" -eq 0 ]] || fail "assertion 1's matcher matched a node that cannot exist"
echo "assertion 1: PASS"

echo "--- assert 2: both gang members were bound, by kai-scheduler, to simulated GPU nodes"
GANG="$(kubectl -n "${PROVE_NS}" get pods -l "aicrme.dev/run-id=${RUN_ID}" -o json)"
GANG_TOTAL="$(echo "${GANG}" | jq '.items | length')"
GANG_BOUND="$(echo "${GANG}" | jq '[.items[] | select(.spec.nodeName != null and .spec.nodeName != "")] | length')"
GANG_NODES="$(echo "${GANG}" | jq -r '[.items[].spec.nodeName] | unique | join(",")')"
GANG_BY_KAI="$(echo "${GANG}" | jq '[.items[] | select(.spec.schedulerName == "kai-scheduler")] | length')"
GANG_GPUS="$(echo "${GANG}" | jq -r '[.items[].spec.containers[0].resources.requests["nvidia.com/gpu"]] | join(",")')"
GANG_ON_GPU_NODES="$(echo "${GANG}" | jq '[.items[] | select(.spec.nodeName | test("^gpu-"))] | length')"
echo "gang: ${GANG_BOUND}/${GANG_TOTAL} bound, scheduler=kai-scheduler on ${GANG_BY_KAI}, nodes=[${GANG_NODES}], gpu requests=[${GANG_GPUS}]"
[[ "${GANG_TOTAL}" -eq 2 ]] || fail "gang has ${GANG_TOTAL} members, want 2 (workload.yaml's completions/parallelism)"
[[ "${GANG_BOUND}" -eq 2 ]] || fail "only ${GANG_BOUND} of 2 gang members were bound to a node"
[[ "${GANG_BY_KAI}" -eq 2 ]] || fail "only ${GANG_BY_KAI} of 2 gang members went through kai-scheduler"
[[ "${GANG_ON_GPU_NODES}" -eq 2 ]] || fail "gang landed off the simulated GPU nodes: [${GANG_NODES}]"
[[ "${GANG_GPUS}" == "8,8" ]] || fail "gang members request [${GANG_GPUS}] GPUs, want 8 each"
# Self-check: the node matcher must reject the real workers it would be
# meaningless to accept.
WORKER_MATCHES="$(echo "${GANG}" | jq --arg p "^${CLUSTER}-worker" '[.items[] | select(.spec.nodeName | test($p))] | length')"
[[ "${WORKER_MATCHES}" -eq 0 ]] || fail "assertion 2 counted ${WORKER_MATCHES} gang members on real workers as GPU-node placements"
echo "assertion 2: PASS"

echo "--- assert 7: validation was SKIPPED on the simulated cluster, not passed"
# The whole reason Validate detects a simulated cluster at all: AICR's
# validator Job carries a blanket toleration, lands on a KWOK fake node, and
# KWOK reports exit 0 without ever starting the container -- a PASS here is
# that false pass, not evidence anything ran. This cluster's fake nodes
# advertise real GPU counts (e2e_apply_kwok_nodes), so a GPU-count check would
# have missed it; the skip has to come from gap.Report.Simulated instead.
VALIDATION="$(run_json "${RUN_ID}" | jq -c '.validation // {}')"
echo "validation record: ${VALIDATION}"
SKIPPED="$(echo "${VALIDATION}" | jq -r '.skipped // empty')"
[[ -n "${SKIPPED}" ]] \
  || fail "validation did not record a skip on a KWOK cluster: ${VALIDATION} -- a pass here is the documented false pass (validator lands on fake nodes, KWOK fakes exit 0)"
[[ "$(echo "${VALIDATION}" | jq '.phases // [] | length')" == "0" ]] \
  || fail "a skipped validation recorded phase results: ${VALIDATION}"
echo "validation skipped as designed: ${SKIPPED}"
echo "assertion 7: PASS"

echo "--- assert 3: the console restarts over a live workload and can still stop it"
# The record is recovered from its file store -- which lives under a directory
# named for this cluster's kube-system UID -- and reconciled against what is
# actually in the cluster (internal/engine/reconcile.go). Both halves matter:
# a run that came back as anything but active would have lost track of a
# workload that is still there, and one that came back active over a workload
# that had been deleted would be claiming something it cannot stop.
#
# A process restart, not a pod delete. That distinction is the point of the
# whole restructure: there is no pod to reschedule, so recovery has nothing to
# trigger it except the next connect. e2e_start_console reuses the same work
# directory precisely so this second launch finds the first one's record.
e2e_stop_console
e2e_start_console
# The session and the cluster connection both died with the old process: the
# cookie was minted by it, and the connection is single-assignment per
# process. e2e_start_console exchanges a new token; e2e_connect is what runs
# recovery.
e2e_connect
RECOVERED_STATE="$(run_state "${RUN_ID}")"
echo "after restart: state=${RECOVERED_STATE}"
[[ "${RECOVERED_STATE}" == "active" ]] || fail "run came back as ${RECOVERED_STATE} after a restart, not active"
kubectl -n "${PROVE_NS}" get job "prove-${RUN_ID}" >/dev/null 2>&1 \
  || fail "the workload is gone after a restart, yet the run came back active"
echo "assertion 3: PASS"

echo "--- assert 6: a Stop that fails leaves the run active and Start blocked"
# An admission webhook with no backing service and failurePolicy: Fail is the
# cheapest way to make one specific API call fail for real. Nothing in a fake
# clientset can produce this: admission is exactly what it does not run.
kubectl apply -f - <<EOF
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: aicrme-e2e-block-job-delete
webhooks:
  - name: block.jobs.aicrme.e2e
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
      - apiGroups: ["batch"]
        apiVersions: ["v1"]
        operations: ["DELETE"]
        resources: ["jobs"]
        scope: Namespaced
    namespaceSelector:
      matchLabels:
        kubernetes.io/metadata.name: ${PROVE_NS}
EOF
STOP_CODE="$(post_status "/api/runs/${RUN_ID}/stop")"
echo "stop with deletion blocked: HTTP ${STOP_CODE}"
[[ "${STOP_CODE}" != "200" ]] || fail "Stop reported success while the workload could not be deleted"
AFTER_FAILED_STOP="$(run_state "${RUN_ID}")"
[[ "${AFTER_FAILED_STOP}" == "active" ]] \
  || fail "a failed Stop moved the run to ${AFTER_FAILED_STOP} -- it must stay active"
kubectl -n "${PROVE_NS}" get job "prove-${RUN_ID}" >/dev/null 2>&1 \
  || fail "the workload is gone after a Stop that reported failure"
START_CODE="$(post_status /api/runs)"
echo "start while a workload is still running: HTTP ${START_CODE}"
[[ "${START_CODE}" == "409" ]] || fail "POST /api/runs answered ${START_CODE}, want 409 while a workload is running"
kubectl delete validatingwebhookconfiguration aicrme-e2e-block-job-delete
echo "assertion 6: PASS"

echo "--- assert 4: Stop removes every object the run created"
STOP_CODE="$(post_status "/api/runs/${RUN_ID}/stop")"
[[ "${STOP_CODE}" == "200" ]] || fail "Stop answered ${STOP_CODE} once deletion was possible again"
STOPPED_STATE="$(run_state "${RUN_ID}")"
[[ "${STOPPED_STATE}" == "done" ]] || fail "run is ${STOPPED_STATE} after a successful Stop, want done"
LEFTOVER_JOBS="$(kubectl -n "${PROVE_NS}" get jobs -l app.kubernetes.io/managed-by=aicrme -o name | wc -l | tr -d ' ')"
LEFTOVER_PODS="$(kubectl -n "${PROVE_NS}" get pods -l app.kubernetes.io/managed-by=aicrme -o name | wc -l | tr -d ' ')"
echo "after Stop: ${LEFTOVER_JOBS} owned jobs, ${LEFTOVER_PODS} owned pods"
[[ "${LEFTOVER_JOBS}" -eq 0 ]] || fail "${LEFTOVER_JOBS} owned Job(s) survived Stop"
[[ "${LEFTOVER_PODS}" -eq 0 ]] || fail "${LEFTOVER_PODS} owned pod(s) survived Stop -- they can still hold GPUs"
# Self-check: the same selector found something a moment ago, so a zero here
# has to mean deletion and not a typo in the label.
[[ "$(kubectl -n "${PROVE_NS}" get pods -o name | wc -l | tr -d ' ')" -eq 0 ]] \
  || fail "unowned pods remain in ${PROVE_NS} -- the owned-label filter may be reading the wrong key"
echo "assertion 4: PASS"

echo "--- assert 5: a gang that never places is cleaned up, and says why"
# RESET FIRST, because a second run over an existing install is now refused.
# internal/steps' alreadyInstalled stops Apply when the recipe's own releases
# are already on the cluster: kai-scheduler's SchedulingShard survives a
# reinstall by design and owns the scheduler Deployment, so installing twice
# leaves the cluster running the FIRST install's scheduler against a control
# plane replaced underneath it -- observed on real GKE H100s on 2026-08-28,
# where Apply reported 16/16 and the gang then placed 0/2. This assertion
# therefore goes through the supported path (reset, then install) rather than
# around it.
echo "--- reset the first run so the second has a clean cluster to install into"
post "/api/runs/${RUN_ID}/reset" -d '{"confirm":"reset"}' >/dev/null
# The wait for it is in drive_to_prove, which retries POST /api/runs while the
# engine answers 409. Two earlier attempts to wait here were both wrong, in
# opposite directions:
#
#   .residue.summary never arrives -- reset.sh says why in as many words, "the
#   record is deleted on a clean teardown, so the run may legitimately be
#   unreachable here".
#
#   `helm list -n kai-scheduler` answered "gone" five seconds in, because it
#   carried no --kube-context. Every kubectl call in this suite is pinned to
#   kind-${CLUSTER} for exactly that reason (c720207); an unpinned helm read
#   the runner's ambient kubeconfig, found no such namespace, and reported
#   success for a teardown that had barely started.
#
# "The console will start a new run" is the real precondition, it is what the
# next line needs, and no context can fake it.

# NOTHING REVERTS A NODE TAINT, which is why placement is blocked this way.
#
# This used to scale kai-scheduler-default to zero, and that worked only
# because of the defect this suite now guards against: the Deployment is owned
# by the SchedulingShard rather than by helm, so it survived a reinstall at
# zero replicas. With a Reset in between, the shard is purged, the reinstall
# creates a fresh Deployment, and the kai-operator reconciles any manual scale
# straight back to one -- the gang then places and the assertion fails having
# tested nothing.
#
# Tainting the simulated GPU nodes is also closer to what this assertion says
# it is about. "A full or unschedulable cluster" is literally an unschedulable
# node, the gang stays genuinely Pending, and the cleanup still has something
# real to race. It is applied BEFORE the run because a taint persists: there
# is no window to lose, unlike a scale that must land after an install and
# before a gang that binds in about two seconds.
#
# Safe for Apply: the KWOK nodes already carry kwok.x-k8s.io/node=fake, so
# every real component is on a Kind worker. The only thing that wanted a
# simulated GPU node is the workload this assertion needs to keep off one.
echo "--- making the simulated GPU nodes unschedulable"
# Bare names, not `-o name`'s node/<name> form: kubectl taint takes a resource
# type and a name ("taint nodes gpu-0 ..."), and rejects the slash form with
# `the server doesn't have a resource type "node/gpu-0"`.
for node in $(e2e_kubectl get nodes -o name | sed 's|^node/||' | grep '^gpu-'); do
  e2e_kubectl taint nodes "${node}" aicrme-e2e-block=true:NoSchedule --overwrite
done

TIMEOUT_RUN_ID="$(drive_to_prove)"
echo "second run id: ${TIMEOUT_RUN_ID}"
STATE=""
for _ in $(seq 1 240); do
  STATE="$(run_state "${TIMEOUT_RUN_ID}")"
  [[ "${STATE}" == "failed" || "${STATE}" == "active" || "${STATE}" == "done" ]] && break
  sleep 10
done
TIMEOUT_RUN_JSON="$(run_json "${TIMEOUT_RUN_ID}")"
[[ "${STATE}" == "failed" ]] || fail "a run whose gang cannot place ended ${STATE}: $(echo "${TIMEOUT_RUN_JSON}" | jq -c '{state,phase,error}')"
TIMEOUT_ERR="$(echo "${TIMEOUT_RUN_JSON}" | jq -r '.error')"
echo "recorded error: ${TIMEOUT_ERR}"
grep -q "did not place within" <<<"${TIMEOUT_ERR}" \
  || fail "the run's error names something other than the placement timeout: ${TIMEOUT_ERR}"
grep -q "0/2 members placed" <<<"${TIMEOUT_ERR}" \
  || fail "the run's error does not report how much of the gang made it: ${TIMEOUT_ERR}"
grep -qv "cleanup failed" <<<"${TIMEOUT_ERR}" \
  || fail "cleanup itself failed: ${TIMEOUT_ERR}"
ORPHANS="$(kubectl -n "${PROVE_NS}" get jobs,pods -o name | wc -l | tr -d ' ')"
echo "objects left in ${PROVE_NS} after the failed run: ${ORPHANS}"
[[ "${ORPHANS}" -eq 0 ]] || {
  kubectl -n "${PROVE_NS}" get jobs,pods -o wide >&2
  fail "${ORPHANS} object(s) survived a failed Prove -- a gang that places later still holds GPUs"
}
# The cleanup confirmed absence, so nothing may still be blocking Start:
# a run whose cleanup could not be confirmed keeps the console shut
# (internal/engine's Ruling 12 guard), and that is not this case.
[[ "$(echo "${TIMEOUT_RUN_JSON}" | jq -r '.cleanupUnconfirmed // false')" == "false" ]] \
  || fail "a confirmed cleanup still set cleanupUnconfirmed"
kubectl -n kai-scheduler scale deploy/kai-scheduler-default --replicas=1
sleep 20
LATE_ARRIVALS="$(kubectl -n "${PROVE_NS}" get pods -o name | wc -l | tr -d ' ')"
echo "objects in ${PROVE_NS} 20s after the scheduler came back: ${LATE_ARRIVALS}"
[[ "${LATE_ARRIVALS}" -eq 0 ]] || fail "a late-placing gang came back after cleanup declared it gone"
echo "assertion 5: PASS"

echo "PASS: prove e2e green (run ${RUN_ID} placed a 2-member gang on [${GANG_NODES}] via kai-scheduler on ${KAI_SCHEDULER_NODE}, survived a restart, refused to lie about a failed Stop, stopped cleanly, and run ${TIMEOUT_RUN_ID} cleaned up after a gang that never placed)"
