#!/usr/bin/env bash
# End-to-end: the FULL Apply chain against a KWOK-simulated cluster with
# --dry-run OFF -- a real `helm upgrade --install` of every component in the
# resolved recipe, asserted through to state=active and workload convergence.
#
# state=active, not done: Prove is the run's final step and deliberately
# leaves its reference workload running (internal/engine's ActiveStep), so a
# successful run now ends terminal-but-active. Waiting for `done` here, as
# this script did before that step existed, would have run this job's whole
# poll budget out over a run that had already succeeded. What the Prove step
# itself does with that workload -- placement, restart, Stop, cleanup after a
# gang that never places -- is test/e2e/prove.sh's subject, not this one's.
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
AGENT_NODE_LABEL="aicrme.e2e/agent=true"
# Workloads are judged only after this settle window. kai-scheduler's
# ReplicaSet transiently reports FailedCreate ("serviceaccount not found")
# while its own chart is still applying, and converges shortly after --
# observed on both verification runs. Judging immediately would pin a race.
SETTLE_SECONDS="${SETTLE_SECONDS:-180}"
EVENTS_FILE=""
ec=0

# --- resource instrumentation -------------------------------------------
#
# Added 2026-08-19 after a regression in which
# apply-real failed twice on main with the Kind API server going unreachable
# mid-Apply, and the script could explain nothing, because every diagnostic it
# runs fires from the EXIT trap -- by which time the cluster is already gone.
#
# So this samples continuously during the run and writes to the runner's disk,
# which survives the cluster dying. Each capture answers a question no other one
# can: docker events says whether a CONTAINER was OOM-killed and which; the
# sampler says how memory and apiserver load were trending before that; the
# streamed console log says what this console itself was doing; and the
# at-the-moment probe in the Apply poll loop says whether the console or the
# API server underneath it went first.
#
# THE GAP THE SECOND ROUND CLOSED: kube-apiserver and etcd run as STATIC PODS INSIDE
# the Kind control-plane container. If one of those processes is OOM-killed the
# container survives, so `docker events oom` never fires -- and reading that
# silence as "no OOM happened" would be a false negative from a capture that
# structurally could not see it. An in-container process death also matches the
# observed symptom better than a container death does: `connection reset by
# peer` then `EOF` is what a client sees when the API server process dies and
# restarts underneath it, not when its container disappears.
#
# So the captures below deliberately overlap at three levels -- host kernel,
# container, and in-cluster process -- because each is blind to a failure the
# others can see.
# Deliberately NOT mktemp: this must land somewhere a workflow artifact step
# can still reach after the script is gone. The 2026-08-19 11:23 run proved why
# -- the shell was killed in a way that skipped its EXIT trap entirely (no
# output, no cleanup, exit 1 after 7m32s of silence), so every capture that only
# surfaced via cleanup died with it. Dumping at the end is not durable; writing
# where something else can collect it is.
DIAG_DIR="${DIAG_DIR:-${SCRIPT_DIR}/../../.e2e-diag}"
mkdir -p "${DIAG_DIR}"
DOCKER_EVENTS_PID=""
SAMPLER_PID=""
CONSOLE_LOG_PID=""
SAMPLE_SECONDS="${SAMPLE_SECONDS:-5}"

start_instrumentation() {
  docker events \
    --filter 'event=oom' --filter 'event=die' --filter 'event=kill' \
    --format '{{.Time}} {{.Action}} {{.Actor.Attributes.name}}' \
    >"${DIAG_DIR}/docker-events.log" 2>&1 &
  DOCKER_EVENTS_PID=$!

  # The console's own account of what it did. Streamed continuously because the
  # failure takes the cluster with it -- `kubectl logs` after the fact returns
  # nothing, which is why the two failing runs on main have no console output at
  # all. The loop re-attaches across restarts and grabs --previous, so an
  # OOMKilled console leaves its dying words rather than a gap.
  (
    while :; do
      kubectl -n "${NS}" logs -l app.kubernetes.io/name=aicrme \
        --tail=-1 --timestamps --follow >>"${DIAG_DIR}/console.log" 2>&1 || true
      echo "=== console log stream ended $(date -u +%H:%M:%S); previous container: ===" \
        >>"${DIAG_DIR}/console.log" 2>&1
      kubectl -n "${NS}" logs -l app.kubernetes.io/name=aicrme \
        --tail=200 --timestamps --previous >>"${DIAG_DIR}/console.log" 2>&1 || true
      sleep 2
    done
  ) &
  CONSOLE_LOG_PID=$!

  # Every command here is failure-tolerant on purpose: this subshell must never
  # be able to fail the run it is observing.
  (
    i=0
    while :; do
      i=$((i + 1))
      {
        echo "=== $(date -u +%H:%M:%S) ==="
        # Runner headroom. Without this a container memory number has nothing to
        # mean anything against.
        free -m 2>/dev/null | sed -n '2p' | awk '{print "runner mem total="$2" used="$3" avail="$7}'
        docker stats --no-stream \
          --format '{{.Name}} mem={{.MemUsage}} ({{.MemPerc}}) cpu={{.CPUPerc}}' 2>/dev/null
        # The static pods. restartCount rising here IS the smoking gun for an
        # in-container OOM that docker events cannot see.
        kubectl -n kube-system get pods \
          -l tier=control-plane \
          -o jsonpath='{range .items[*]}{.metadata.name} restarts={.status.containerStatuses[0].restartCount} last={.status.containerStatuses[0].lastState.terminated.reason}/{.status.containerStatuses[0].lastState.terminated.exitCode}{"\n"}{end}' 2>/dev/null
        kubectl -n "${NS}" get pod -l app.kubernetes.io/name=aicrme \
          -o jsonpath='console restarts={.items[0].status.containerStatuses[0].restartCount} last={.items[0].status.containerStatuses[0].lastState.terminated.reason}{"\n"}' 2>/dev/null
        # Host-kernel OOM verdicts, sampled rather than only dumped at the end:
        # names the process the kernel actually killed, which is the one fact
        # that separates "apiserver died" from "our console died".
        sudo dmesg 2>/dev/null | grep -iE 'killed process|out of memory' | tail -3
        # Every other sample only: one extra API request, and it directly tests
        # whether ~20 new watches loaded the apiserver.
        if [[ $((i % 2)) -eq 0 ]]; then
          kubectl get --raw /metrics 2>/dev/null \
            | grep -E '^apiserver_current_inflight_requests|^apiserver_longrunning_requests\{.*watch' \
            | head -6
        fi
      } >>"${DIAG_DIR}/samples.log" 2>&1
      sleep "${SAMPLE_SECONDS}"
    done
  ) &
  SAMPLER_PID=$!
}

stop_instrumentation() {
  # if-then rather than `[[ ... ]] && kill ... || true`: in that form the `|| true`
  # also fires when the test itself is false, which reads as an else-branch and
  # is not one (SC2015).
  if [[ -n "${SAMPLER_PID}" ]]; then
    kill "${SAMPLER_PID}" 2>/dev/null || true
  fi
  if [[ -n "${DOCKER_EVENTS_PID}" ]]; then
    kill "${DOCKER_EVENTS_PID}" 2>/dev/null || true
  fi
  if [[ -n "${CONSOLE_LOG_PID}" ]]; then
    kill "${CONSOLE_LOG_PID}" 2>/dev/null || true
  fi
}

# Fires the moment a poll fails, while the cluster may still be answering.
# This is the discriminator the failing runs lacked: it asks "is the API server
# alive?" and "was the console container OOM-killed?" at the instant the
# symptom appears, not minutes later once everything is unreachable.
probe_at_failure() {
  local n="$1"
  {
    echo "=== console unreachable (consecutive failure ${n}) at $(date -u +%H:%M:%S) ==="
    echo "-- api server healthz --"
    kubectl get --raw /healthz 2>&1 | head -3
    echo "-- console pod --"
    kubectl -n "${NS}" get pod -l app.kubernetes.io/name=aicrme -o wide 2>&1 | head -5
    kubectl -n "${NS}" get pod -l app.kubernetes.io/name=aicrme \
      -o jsonpath='restarts={.items[0].status.containerStatuses[0].restartCount} lastTerminated={.items[0].status.containerStatuses[0].lastState.terminated.reason} exit={.items[0].status.containerStatuses[0].lastState.terminated.exitCode}{"\n"}' 2>&1
    echo "-- kind containers --"
    docker ps -a --filter "name=${CLUSTER}" --format '{{.Names}} {{.Status}}' 2>&1
    # The static pods, asked from inside the control-plane container via crictl
    # rather than through kubectl: if the API server is the thing that died,
    # kubectl cannot answer questions about it, and this path still can.
    echo "-- static pods via crictl (survives a dead apiserver) --"
    docker exec "${CLUSTER}-control-plane" crictl ps -a \
      --name 'kube-apiserver|etcd' -o table 2>&1 | head -10
    echo "-- apiserver container log tail (in-container) --"
    docker exec "${CLUSTER}-control-plane" sh -c \
      'crictl logs --tail 25 $(crictl ps -a --name kube-apiserver -q | head -1) 2>&1' 2>&1 | tail -25
    echo "-- kernel OOM verdicts right now --"
    sudo dmesg 2>&1 | grep -iE 'killed process|out of memory' | tail -5
  } >>"${DIAG_DIR}/at-failure.log" 2>&1
  echo "console unreachable (${n}) -- probe captured" >&2
}

dump_instrumentation() {
  echo "--- instrumentation: at-failure probes ---" >&2
  cat "${DIAG_DIR}/at-failure.log" >&2 2>/dev/null || echo "(console never became unreachable)" >&2
  echo "--- instrumentation: docker oom/die/kill events ---" >&2
  cat "${DIAG_DIR}/docker-events.log" >&2 2>/dev/null || echo "(none captured)" >&2
  echo "--- instrumentation: kernel OOM ---" >&2
  sudo dmesg 2>/dev/null | grep -iE 'out of memory|killed process' | tail -20 >&2 \
    || echo "(no kernel OOM lines, or dmesg unavailable)" >&2
  # The WHOLE sample series, not a tail. The failures happen minutes in, and the
  # trend leading up to one is the evidence -- a truncated window would show the
  # aftermath and hide the ramp.
  echo "--- instrumentation: full resource sample series ---" >&2
  cat "${DIAG_DIR}/samples.log" >&2 2>/dev/null || echo "(none captured)" >&2
  echo "--- instrumentation: console log (streamed, survives cluster loss) ---" >&2
  tail -n 400 "${DIAG_DIR}/console.log" >&2 2>/dev/null || echo "(none captured)" >&2
}

# Printed on SUCCESS. A passing run is the baseline a failing run's numbers have
# to mean something against -- without it, "control-plane at 1004MiB" is a number
# with nothing to compare to. Each line below is the counterpart of a signal the
# failure path dumps in full.
dump_run_baseline() {
  echo "--- instrumentation: baseline for this passing run ---"
  echo "peak container memory:"
  grep -o '[a-z0-9-]*\(control-plane\|worker[0-9]*\) mem=[0-9.]*MiB' \
    "${DIAG_DIR}/samples.log" 2>/dev/null | sort -u -t= -k2 -rn | head -5 \
    || echo "  (no samples)"
  echo "runner memory low-water mark (lowest available seen):"
  grep -o 'runner mem total=[0-9]* used=[0-9]* avail=[0-9]*' \
    "${DIAG_DIR}/samples.log" 2>/dev/null | sort -t= -k4 -n | head -1 \
    || echo "  (not captured)"
  # If these are non-zero on a PASSING run, the static pods are already
  # restarting under load and the failures are the same thing, further along.
  echo "control-plane static pod restarts (final):"
  kubectl -n kube-system get pods -l tier=control-plane \
    -o jsonpath='{range .items[*]}  {.metadata.name} restarts={.status.containerStatuses[0].restartCount}{"\n"}{end}' 2>/dev/null \
    || echo "  (unavailable)"
  echo "peak apiserver inflight requests:"
  grep -h '^apiserver_current_inflight_requests' "${DIAG_DIR}/samples.log" 2>/dev/null \
    | sort -k2 -rn | head -2 || echo "  (not captured)"
  echo "kernel OOM verdicts this run:"
  sudo dmesg 2>/dev/null | grep -icE 'killed process' | sed 's/^/  count=/' \
    || echo "  (dmesg unavailable)"
}

dump_recent_events() {
  [[ -n "${CONSOLE_URL:-}" ]] || return 0
  echo "--- last 80 SSE events ---" >&2
  set +e
  e2e_api GET '/api/events?since=0' --max-time 5 2>/dev/null \
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
  stop_instrumentation
  if [[ "${exit_code}" -ne 0 ]]; then
    # Runner-disk evidence first: it survives the cluster being gone, and the
    # kubectl-based diagnostics below routinely do not.
    dump_instrumentation
    e2e_diagnose "${NS}"
    echo "--- helm releases ---" >&2
    helm list -A >&2 2>&1 || true
    echo "--- workloads short of desired ---" >&2
    kubectl get deploy,ds,sts -A >&2 2>&1 || true
    dump_recent_events
  fi
  e2e_console_cleanup
  rm -f "${EVENTS_FILE}"
  # DIAG_DIR is deliberately left in place for the workflow's artifact upload.
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

echo "--- label a worker for the snapshot agent (see header: workers taint the control-plane)"
kubectl label node "${CLUSTER}-worker" "${AGENT_NODE_LABEL}" --overwrite

echo "--- start the console: agent pinned onto that worker, dry-run OFF"
export AICRME_SNAPSHOT_NODE_SELECTOR="${AGENT_NODE_LABEL}"
export AICRME_SNAPSHOT_REQUESTS='cpu=200m'
export AICRME_APPLY_DRY_RUN='false'
e2e_start_console
e2e_connect

# Started before the run, not before Apply: the memory trend leading INTO the
# install is what makes a spike during it legible.
start_instrumentation

echo "--- POST /api/runs"
RUN_JSON="$(e2e_api POST /api/runs -H 'Content-Type: application/json')"
RUN_ID="$(echo "${RUN_JSON}" | jq -r '.id')"
[[ -n "${RUN_ID}" && "${RUN_ID}" != "null" ]] || {
  echo "no run id in response: ${RUN_JSON}" >&2
  exit 1
}
echo "run id: ${RUN_ID}"

echo "--- poll until awaiting_decision (Discover complete)"
STATE=""
for _ in $(seq 1 90); do
  RUN_JSON="$(e2e_api GET "/api/runs/${RUN_ID}")"
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
e2e_api POST "/api/runs/${RUN_ID}/decide" \
  -H 'Content-Type: application/json' \
  -d '{"intent":"training","platform":"kubeflow"}' >/dev/null

echo "--- poll until the confirm gate (Recommend + Bundle complete)"
STATE=""
for _ in $(seq 1 60); do
  RUN_JSON="$(e2e_api GET "/api/runs/${RUN_ID}")"
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
e2e_api POST "/api/runs/${RUN_ID}/decide" \
  -H 'Content-Type: application/json' \
  -d '{"apply":"yes"}' >/dev/null

# Both verification runs installed all 14 components in 6m02s. 40 minutes
# covers a full retry storm (deploy.sh backs off 5s/20s/45s/80s/120s over five
# attempts, WITHOUT --best-effort) plus a slow runner's image pulls, which are
# the variable this script adds over the dry-run job -- a real install pulls
# every component's images, which --dry-run never does.
echo "--- poll until active or failed (Apply, then Prove)"
STATE=""
CURL_FAILS=0
POLLS=0
for _ in $(seq 1 240); do
  # Deliberately NOT `curl -fsS ...` bare: under `set -e` a single failed poll
  # aborted the script instantly, and every diagnostic then ran from the EXIT
  # trap against a cluster that had already gone. That is how the two failing
  # runs on main managed to explain nothing. Tolerating a few polls costs
  # nothing when healthy and is the difference between "it broke" and knowing
  # WHAT broke first -- the console or the API server underneath it.
  if ! RUN_JSON="$(e2e_api GET "/api/runs/${RUN_ID}" --max-time 10 2>/dev/null)"; then
    CURL_FAILS=$((CURL_FAILS + 1))
    probe_at_failure "${CURL_FAILS}"
    if [[ "${CURL_FAILS}" -ge 3 ]]; then
      echo "console unreachable for ${CURL_FAILS} consecutive polls (state=${STATE:-unknown})" >&2
      exit 1
    fi
    sleep 10
    continue
  fi
  CURL_FAILS=0
  STATE="$(echo "${RUN_JSON}" | jq -r '.state')"
  [[ "${STATE}" == "active" || "${STATE}" == "done" || "${STATE}" == "failed" ]] && break
  POLLS=$((POLLS + 1))
  # A heartbeat straight to stdout every ~60s. Everything else this script
  # captures is written to disk and surfaced later, which is worthless when the
  # shell is SIGKILLed -- the 11:23 run died with 7m32s of silence and took its
  # whole capture set with it. A line already flushed into the CI log cannot be
  # killed with the process, so this is the one signal guaranteed to survive.
  if [[ $((POLLS % 6)) -eq 0 ]]; then
    HB_MEM="$(free -m 2>/dev/null | sed -n '2p' | awk '{print "avail="$7"MiB"}')"
    HB_CP="$(docker stats --no-stream --format '{{.Name}}={{.MemUsage}}' 2>/dev/null \
      | grep control-plane | head -1)"
    HB_RS="$(kubectl -n kube-system get pods -l tier=control-plane \
      -o jsonpath='{range .items[*]}{.metadata.name}:{.status.containerStatuses[0].restartCount} {end}' 2>/dev/null)"
    echo "[hb $(date -u +%H:%M:%S)] runner ${HB_MEM:-?} | ${HB_CP:-cp=?} | restarts: ${HB_RS:-?}"
  fi
  sleep 10
done

[[ "${STATE}" == "failed" ]] && fail_run "${RUN_JSON}"
[[ "${STATE}" == "active" ]] || {
  echo "the run did not reach state=active within the deadline (state=${STATE})" >&2
  fail_run "${RUN_JSON}"
}
echo "run reached state=active"

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

# The observer narrates cluster conditions (Pod/Event/DaemonSet/Deployment/
# Node telemetry) as kind=cluster bus events for the whole life of the run,
# including the settle window just finished above, so the fetch happens here
# -- after convergence, not right after state=done -- to capture that tail
# rather than clip it. This is the only check in the suite that exercises
# that telemetry against a REAL cluster: internal/bus and internal/observer
# are unit-tested, but a jq filter with a wrong field name returns empty, a
# `-z`/`-eq 0` test reads that as pass, and the assertion is green forever
# having matched nothing. Each assertion below prints what it actually
# matched, not just pass/fail, and carries its own inverted-input check so a
# silently-broken filter fails loudly instead of vacuously agreeing with
# whatever it's given.
echo "--- fetch the full bus event stream for the cluster-telemetry assertions"
EVENTS_FILE="$(mktemp -t aicrme-real-events.XXXXXX.json)"
set +e
e2e_api GET '/api/events?since=0' --max-time 15 \
  | sed -n 's/^data: //p' \
  | jq -s '[.[] | select(has("kind"))]' >"${EVENTS_FILE}"
set -e
EVENT_COUNT="$(jq 'length' "${EVENTS_FILE}")"
[[ "${EVENT_COUNT}" -gt 0 ]] || {
  echo "no events at all came back from /api/events -- cannot assert on cluster telemetry" >&2
  exit 1
}
echo "fetched ${EVENT_COUNT} bus events (all kinds) for assertion"

echo "--- assert 1: cluster events appeared and were attributed to a real deployment action"
# component is a moving cursor (Attribution.ActiveAction): it names whichever
# deployment action deploy.sh was installing when the condition was observed,
# and is legitimately empty between actions and outside Apply. run.components
# IS that action list (14 rows for this recipe -- see the TOTAL assertion
# above: 13 components, 14 deployment actions),
# so matching a cluster event's component against it is matching against
# actions, per the task brief.
ACTIONS_JSON="$(echo "${RUN_JSON}" | jq -cS '[.components[].name] | unique')"
echo "run's deployment-action set (${TOTAL} actions): ${ACTIONS_JSON}"
CLUSTER_TOTAL="$(jq '[.[] | select(.kind=="cluster")] | length' "${EVENTS_FILE}")"
CLUSTER_WITH_COMPONENT="$(jq '[.[] | select(.kind=="cluster" and .component != null and .component != "")] | length' "${EVENTS_FILE}")"
CLUSTER_ATTRIBUTED="$(jq --argjson actions "${ACTIONS_JSON}" '
  [.[] | select(.kind=="cluster" and .component != null and .component != "")
        | select(.component as $c | $actions | index($c) != null)] | length
' "${EVENTS_FILE}")"
echo "cluster events: ${CLUSTER_TOTAL} total, ${CLUSTER_WITH_COMPONENT} carry a non-empty component, ${CLUSTER_ATTRIBUTED} match a real deployment action"
[[ "${CLUSTER_TOTAL}" -gt 0 ]] || {
  echo "no kind=cluster events at all -- the observer produced no telemetry for this run" >&2
  exit 1
}
[[ "${CLUSTER_ATTRIBUTED}" -gt 0 ]] || {
  echo "no cluster event's component matched any of this run's ${TOTAL} deployment actions" >&2
  echo "components actually seen on cluster events: $(jq -cr '[.[] | select(.kind=="cluster") | .component] | unique' "${EVENTS_FILE}")" >&2
  exit 1
}
echo "assertion 1: PASS"
# Prove the matcher discriminates rather than passing vacuously: the same
# query against an action set that cannot exist in this run must match
# nothing. If it ever does, the filter above is broken, not permissive.
BOGUS_ATTRIBUTED="$(jq --argjson actions '["__no-such-action__"]' '
  [.[] | select(.kind=="cluster" and .component != null and .component != "")
        | select(.component as $c | $actions | index($c) != null)] | length
' "${EVENTS_FILE}")"
[[ "${BOGUS_ATTRIBUTED}" -eq 0 ]] || {
  echo "assertion 1's matcher matched ${BOGUS_ATTRIBUTED} event(s) against a bogus action set -- the filter is not discriminating" >&2
  exit 1
}

echo "--- assert 2: at most one cluster event per normalized state transition"
# A property, not a count (see the task brief and design doc Section 5): an
# events-per-run ceiling encodes today's action count and breaks on an AICR
# bump.
#
# A first version of this assertion grouped strictly by (uid, reason,
# resolved) and asserting no group exceeds one event. Run for real, it
# failed against genuine, non-buggy telemetry, for two reasons visible in
# this very run's data:
#
#   1. kind=cluster is not exclusively observer.publish's ClusterData.
#      internal/steps/discover.go's Discover step ALSO emits
#      bus.Event{Kind: bus.KindCluster} for each capability gap finding, with
#      no Data payload at all (no uid/reason/resolved -- just a message).
#      Grouping their absent uid/reason together produced a spurious
#      uid=null/reason=null bucket. Excluded below by requiring .data to be
#      an object carrying a uid -- the actual ClusterData shape the brief
#      describes.
#   2. RolloutProgress and GPUAllocatable are CONTINUOUS conditions, not
#      binary ones: onDeployment/onDaemonSet/onNode (internal/observer/
#      handlers.go) publish on every distinct ready/desired or GPU-quantity
#      change, so a DaemonSet scaling 0/10 -> 6/10 -> 7/10 -> 8/10 -> 9/10 ->
#      10/10 genuinely, correctly publishes five separate resolved=false
#      events before the sixth resolves it -- five distinct transitions, not
#      one transition published five times. This run's own
#      nvsentinel/platform-connectors DaemonSet did exactly that. A flat
#      (uid,reason,resolved) grouping cannot tell "five real transitions"
#      from "one transition republished five times", which is the actual
#      defect this assertion exists to catch.
#   3. Event-sourced Warnings (onEventChange, internal/observer/events.go)
#      can legitimately arise and resolve more than once in one run: Ruling
#      26 resolves a Warning the instant the pod it names is no longer
#      troubled, and resolveEventsLocked then unconditionally evicts the
#      dedupe entry -- so kubelet posting a SECOND readiness-probe-failure
#      Event later in the same run is, correctly, a new arising, not a
#      duplicate of the first. This run's own kube-prometheus-stack pods
#      showed exactly this: the identical (uid, Unhealthy) pair arising and
#      resolving three separate times as node-exporter's readiness probe
#      settled.
#
# What actually distinguishes a genuine transition from a bug-caused
# duplicate, matching the change-detection guard every handler in
# internal/observer already applies before it calls publish (e.g.
# "if had && prev == summary { return }"), is CONTENT: two events for the
# SAME (uid, reason), ADJACENT in publish order, with IDENTICAL
# resolved/ready/desired/container are a state that did not actually change
# between two publishes -- the exact thing change-detection exists to
# prevent. Non-adjacent repeats (arise ... resolve ... arise again) and
# adjacent-but-different repeats (6/10 then 7/10) are both genuine,
# distinct transitions and must not be flagged.
TYPED_CLUSTER_TOTAL="$(jq '[.[] | select(.kind=="cluster" and ((.data? // null) | type == "object" and has("uid")))] | length' "${EVENTS_FILE}")"
echo "${CLUSTER_TOTAL} kind=cluster events total, ${TYPED_CLUSTER_TOTAL} carry a real ClusterData payload (the rest are Discover's gap-finding markers, no uid/reason/resolved of their own)"
# shellcheck disable=SC2016 # this is a jq program: $seq/$cur/$prev are jq's
# own variables, reused verbatim below against both EVENTS_FILE and a
# synthetic fixture, and must not be shell-expanded.
DUPLICATE_JQ='
  [.[] | select(.kind=="cluster" and ((.data? // null) | type == "object" and has("uid")))
        | .data + {id}]
  | group_by([.uid, .reason])
  | map(sort_by(.id))
  | map(. as $seq
      | [range(0; ($seq | length) - 1)]
      | map($seq[.] as $prev | $seq[.+1] as $cur
          | select(
              (($cur.resolved // false) == ($prev.resolved // false)) and
              (($cur.ready // 0) == ($prev.ready // 0)) and
              (($cur.desired // 0) == ($prev.desired // 0)) and
              (($cur.container // "") == ($prev.container // ""))
            )
          | {uid: $cur.uid, reason: $cur.reason, resolved: ($cur.resolved // false), prevId: $prev.id, id: $cur.id}
        )
    )
  | add // []
'
SEQUENCE_COUNT="$(jq '
  [.[] | select(.kind=="cluster" and ((.data? // null) | type == "object" and has("uid")))]
  | group_by([.data.uid, .data.reason]) | length
' "${EVENTS_FILE}")"
DUPLICATES="$(jq -c "${DUPLICATE_JQ}" "${EVENTS_FILE}")"
DUPLICATE_COUNT="$(echo "${DUPLICATES}" | jq 'length')"
echo "${TYPED_CLUSTER_TOTAL} typed cluster events fall into ${SEQUENCE_COUNT} distinct (uid,reason) sequences; ${DUPLICATE_COUNT} adjacent same-content repeat(s) found"
[[ "${DUPLICATE_COUNT}" -eq 0 ]] || {
  echo "${DUPLICATE_COUNT} event(s) republished the same resolved/ready/desired/container as the immediately preceding event for their (uid,reason):" >&2
  echo "${DUPLICATES}" | jq -r '.[] | "  uid=\(.uid) reason=\(.reason) resolved=\(.resolved) id=\(.prevId)->\(.id)"' >&2
  exit 1
}
echo "assertion 2: PASS"
# Prove the check actually catches a duplicate rather than always reporting
# zero by construction: feed it a synthetic (uid,reason) sequence whose two
# events are byte-identical in every field the check compares, and confirm
# it is flagged.
SYNTH_DUPLICATE_COUNT="$(jq -c "${DUPLICATE_JQ} | length" <<<'
  [{"id":1,"kind":"cluster","data":{"uid":"synthetic","reason":"Synthetic","resolved":false}},
   {"id":2,"kind":"cluster","data":{"uid":"synthetic","reason":"Synthetic","resolved":false}}]
')"
[[ "${SYNTH_DUPLICATE_COUNT}" -eq 1 ]] || {
  echo "assertion 2's duplicate check did not detect a synthetic repeat (got ${SYNTH_DUPLICATE_COUNT}, want 1) -- its own machinery is broken" >&2
  exit 1
}

echo "--- assert 3: zero bus gaps and zero drops across the run (event IDs contiguous)"
# What the old events-per-run ceiling was only ever a proxy for: every ID the
# Bus hands out must be retained, in order, with nothing skipped. Span ==
# count rules out both a gap (an ID the ring never retained or the bus never
# issued to a slow subscriber) and a duplicate (two events sharing an ID) in
# one comparison.
ID_COUNT="$(jq '[.[].id] | length' "${EVENTS_FILE}")"
ID_MIN="$(jq '[.[].id] | min' "${EVENTS_FILE}")"
ID_MAX="$(jq '[.[].id] | max' "${EVENTS_FILE}")"
ID_SPAN=$((ID_MAX - ID_MIN + 1))
echo "bus event IDs span [${ID_MIN}, ${ID_MAX}] (${ID_SPAN} slots) across ${ID_COUNT} retained events (all kinds)"
[[ "${ID_SPAN}" -eq "${ID_COUNT}" ]] || {
  MISSING="$(jq -c --argjson min "${ID_MIN}" --argjson max "${ID_MAX}" '
    ([.[].id]) as $have
    | [range($min; $max + 1) | select(. as $w | $have | index($w) == null)]
  ' "${EVENTS_FILE}")"
  echo "${ID_SPAN} slots expected across [${ID_MIN},${ID_MAX}] but only ${ID_COUNT} events retained -- gap or drop; missing IDs: ${MISSING}" >&2
  exit 1
}
echo "assertion 3: PASS"
# Prove the span check actually detects a gap: ids 1,2,4 span 4 slots but
# carry only 3 events, so the same comparison must read false.
SYNTH_CONTIGUOUS="$(jq -n '[1,2,4] as $a | (($a | max) - ($a | min) + 1) == ($a | length)')"
[[ "${SYNTH_CONTIGUOUS}" == "false" ]] || {
  echo "assertion 3's span check did not detect a synthetic gap (ids 1,2,4) -- its own machinery is broken" >&2
  exit 1
}

dump_run_baseline

echo "PASS: apply-real e2e green (run ${RUN_ID}; ${TOTAL} components installed for real, state=active, every workload at desired replicas, ${CLUSTER_TOTAL} cluster events attributed and contiguous)"
