#!/usr/bin/env bash
# Chart contract test: renders charts/aicrme and asserts the invariants the
# spec depends on. `helm lint` only catches syntax and schema problems; every
# assertion below exists because a semantic bug in this chart shipped past
# lint, or because approach.md states the property as a hard requirement.
#
# Runs offline. `helm template` evaluates `lookup` to empty, so the generated
# password differs per render — nothing here asserts on it.
set -euo pipefail

CHART="${CHART:-charts/aicrme}"
FAILURES=0

pass() { printf '  \033[0;32m✓\033[0m %s\n' "$1"; }
fail() { printf '  \033[0;31m✗\033[0m %s\n     %s\n' "$1" "$2"; FAILURES=$((FAILURES + 1)); }

render() { helm template aicrme "${CHART}" "$@"; }

# doc KIND — emits the rendered manifest(s) of that kind.
doc() {
  local kind="$1"
  yq "select(.kind == \"${kind}\")"
}

# val — reads a scalar from stdin, dropping yq's nulls. Never fails the script:
# a missing value must surface as a failed assertion with a diagnosable
# message, not abort the run under `set -e` and look like a crash.
val() {
  grep -v '^null$' | head -1 || true
}

echo "== helm lint =="
if helm lint "${CHART}" >/dev/null 2>&1; then
  pass "helm lint clean"
else
  helm lint "${CHART}" || true
  fail "helm lint" "chart failed lint"
fi

echo "== rendering =="
if ! render >/dev/null 2>&1; then
  render || true
  fail "helm template" "chart does not render with default values"
  exit 1
fi
pass "renders with default values"

# --- Invariant 1: the username is not a user-facing knob -------------------
# approach.md: "Single user. ... No user management, no OIDC." The SPA login
# posts a fixed `admin`; a settable auth.username silently 401s every login.
echo "== auth =="
u_default=$(render | doc Secret | yq -r '.data.username' | val | base64 -d)
if [[ "${u_default}" == "admin" ]]; then
  pass "Secret username is admin by default"
else
  fail "Secret username default" "got '${u_default}', want 'admin'"
fi

u_override=$(render --set auth.username=demo | doc Secret | yq -r '.data.username' | val | base64 -d)
if [[ "${u_override}" == "admin" ]]; then
  pass "--set auth.username is inert (stays admin)"
else
  fail "auth.username override" "got '${u_override}', want 'admin' — a settable username 401s every login against the SPA's hardcoded admin"
fi

# NOTES.txt is asserted against its template source: `helm template` does not
# render the parent chart's notes, so the rendered output cannot be inspected.
NOTES="${CHART}/templates/NOTES.txt"
if grep -q 'aicrme.authUsername' "${NOTES}"; then
  pass "NOTES.txt prints the username from the same helper as the Secret"
else
  fail "NOTES.txt username" "NOTES.txt does not use the aicrme.authUsername helper — it can drift from the Secret"
fi

# --- Invariant 2: the privileged agent Job stays inside the release --------
# cmd/aicrme defaults AICRME_NAMESPACE to "aicrme". If the chart does not
# override it, installing anywhere else runs the PRIVILEGED snapshot agent in
# a separate namespace that `helm uninstall` never removes.
echo "== namespace containment =="
ns_cm=$(render -n someother | doc ConfigMap | yq -r '.data.AICRME_NAMESPACE' | val)
if [[ "${ns_cm}" == "someother" ]]; then
  pass "ConfigMap AICRME_NAMESPACE follows the release namespace"
else
  fail "AICRME_NAMESPACE" "got '${ns_cm}', want 'someother' — the privileged agent Job would run outside the release"
fi

if render -n someother | doc Deployment |
   yq -r '.spec.template.spec.containers[].env[] | select(.name == "AICRME_NAMESPACE") | .name' |
   grep -q AICRME_NAMESPACE; then
  pass "Deployment references AICRME_NAMESPACE (not an orphaned ConfigMap key)"
else
  fail "AICRME_NAMESPACE env" "Deployment does not consume the ConfigMap key"
fi

# --- Invariant 3: exposure defaults ---------------------------------------
# approach.md: ClusterIP + port-forward is the default precisely because a
# cluster-admin console behind one password is a cluster-takeover surface.
echo "== exposure =="
svc_type=$(render | doc Service | yq -r '.spec.type' | val)
if [[ "${svc_type}" == "ClusterIP" ]]; then
  pass "Service defaults to ClusterIP"
else
  fail "Service type" "got '${svc_type}', want 'ClusterIP'"
fi

# --- Invariant 4: the cluster-admin grant is deliberate and disclosed ------
# approach.md requires the grant AND that it be stated plainly. Pinning it
# makes a silent widening or narrowing visible in review.
echo "== rbac disclosure =="
role=$(render | doc ClusterRoleBinding | yq -r '.roleRef.name' | val)
if [[ "${role}" == "cluster-admin" ]]; then
  pass "ClusterRoleBinding grants cluster-admin (deliberate, per spec)"
else
  fail "ClusterRoleBinding roleRef" "got '${role}', want 'cluster-admin'"
fi

binding_count=$(render | doc ClusterRoleBinding | yq -r '.kind' | grep -c '^ClusterRoleBinding$' || true)
if [[ "${binding_count}" == "1" ]]; then
  pass "exactly one ClusterRoleBinding"
else
  fail "ClusterRoleBinding count" "got ${binding_count}, want 1 — a second binding is undisclosed privilege"
fi

# approach.md requires each of these be stated plainly to the operator.
while IFS= read -r phrase; do
  if grep -qF "${phrase}" "${NOTES}"; then
    pass "NOTES.txt discloses: ${phrase}"
  else
    fail "NOTES.txt disclosure" "missing required phrase: ${phrase}"
  fi
done <<'PHRASES'
cluster-admin
DEMO AND EVAL TOOL
service.type=LoadBalancer
direct internet access
PHRASES

# The same disclosure is required in the README (approach.md, Security posture).
if grep -qF 'cluster-admin' README.md; then
  pass "README discloses cluster-admin"
else
  fail "README disclosure" "README.md does not mention cluster-admin, which the spec requires alongside NOTES.txt"
fi

# --- Invariant 5: pod hardening -------------------------------------------
echo "== pod hardening =="
pod_sec=$(render | doc Deployment | yq -r '.spec.template.spec.securityContext.runAsNonRoot' | val)
if [[ "${pod_sec}" == "true" ]]; then
  pass "runAsNonRoot is true"
else
  fail "runAsNonRoot" "got '${pod_sec}', want 'true'"
fi

esc=$(render | doc Deployment |
      yq -r '.spec.template.spec.containers[].securityContext.allowPrivilegeEscalation' | val)
if [[ "${esc}" == "false" ]]; then
  pass "allowPrivilegeEscalation is false"
else
  fail "allowPrivilegeEscalation" "got '${esc}', want 'false'"
fi

# --- Invariant 6: the work-dir emptyDir is what makes the root filesystem
# read-only safe ---------------------------------------------------------
# approach.md / Phase 0-1 review: readOnlyRootFilesystem was deferred until
# the deploy.sh wiring showed which helm/kubectl cache dirs need to be
# writable. cmd/aicrme now creates those dirs under one emptyDir, and every
# tool is pointed into it by env -- assert all of that actually renders.
echo "== work dir =="
root_fs=$(render | doc Deployment |
          yq -r '.spec.template.spec.containers[].securityContext.readOnlyRootFilesystem' | val)
if [[ "${root_fs}" == "true" ]]; then
  pass "readOnlyRootFilesystem is true"
else
  fail "readOnlyRootFilesystem" "got '${root_fs}', want 'true'"
fi

work_vol_type=$(render | doc Deployment |
                yq -r '.spec.template.spec.volumes[] | select(.name == "work") | has("emptyDir")' | val)
if [[ "${work_vol_type}" == "true" ]]; then
  pass "work volume is an emptyDir"
else
  fail "work volume" "got '${work_vol_type}', want an emptyDir volume named work"
fi

work_mount=$(render | doc Deployment |
             yq -r '.spec.template.spec.containers[].volumeMounts[] | select(.name == "work") | .mountPath' | val)
if [[ "${work_mount}" == "/var/lib/aicrme" ]]; then
  pass "work volume mounted at /var/lib/aicrme"
else
  fail "work volumeMount path" "got '${work_mount}', want '/var/lib/aicrme'"
fi

for var in AICRME_WORK_DIR TMPDIR HOME HELM_CACHE_HOME HELM_CONFIG_HOME HELM_DATA_HOME KUBECACHEDIR; do
  got=$(render | doc Deployment |
        yq -r ".spec.template.spec.containers[].env[] | select(.name == \"${var}\") | .value" | val)
  if [[ -n "${got}" ]]; then
    pass "env ${var} set (${got})"
  else
    fail "env ${var}" "missing -- deploy.sh's tools would fall back to the read-only root filesystem"
  fi
done

size_default=$(render | doc Deployment | yq -r '.spec.template.spec.volumes[] | select(.name == "work") | .emptyDir.sizeLimit' | val)
if [[ "${size_default}" == "1Gi" ]]; then
  pass "work volume sizeLimit defaults to 1Gi"
else
  fail "work volume sizeLimit default" "got '${size_default}', want '1Gi'"
fi

size_override=$(render --set workDir.sizeLimit=2Gi | doc Deployment | yq -r '.spec.template.spec.volumes[] | select(.name == "work") | .emptyDir.sizeLimit' | val)
if [[ "${size_override}" == "2Gi" ]]; then
  pass "--set workDir.sizeLimit=2Gi flows through to the emptyDir"
else
  fail "work volume sizeLimit override" "got '${size_override}', want '2Gi'"
fi

# --- Invariant 7: the shutdown budget is explicit, not inherited ----------
# cmd/aicrme drains HTTP and cancels any in-flight run concurrently, bounded
# by runShutdownTimeout, before it can return from main -- returning early
# would tear down the PID namespace and SIGKILL helm mid-release, stranding it
# in pending-install. That only fits inside Kubernetes' default
# terminationGracePeriodSeconds (30s) by luck; pinning it here makes the
# budget survive an unrelated edit to the Deployment.
echo "== shutdown budget =="
grace_default=$(render | doc Deployment | yq -r '.spec.template.spec.terminationGracePeriodSeconds' | val)
if [[ "${grace_default}" == "45" ]]; then
  pass "terminationGracePeriodSeconds defaults to 45"
else
  fail "terminationGracePeriodSeconds default" "got '${grace_default}', want '45'"
fi

grace_override=$(render --set terminationGracePeriodSeconds=60 | doc Deployment | yq -r '.spec.template.spec.terminationGracePeriodSeconds' | val)
if [[ "${grace_override}" == "60" ]]; then
  pass "--set terminationGracePeriodSeconds=60 flows through"
else
  fail "terminationGracePeriodSeconds override" "got '${grace_override}', want '60'"
fi

# The two halves of the budget live in different languages and neither
# toolchain can see the other, exactly like the AICR image pin `make
# check-aicr-pin` greps for. main.go's constants are the process's own wait;
# the chart's grace period is the wall clock Kubernetes allows before SIGKILL.
# Raising one without the other is silent until a real Apply is interrupted,
# so read both and compare.
SHUTDOWN_SRC="cmd/aicrme/main.go"

# go_const_seconds FILE NAME — reads `const NAME = <n> * time.Second` from a Go
# source file as whole seconds, or nothing if it is not declared that way.
go_const_seconds() {
  # [0-9][0-9]* rather than [0-9]\+ : BSD sed's basic regex has no \+.
  sed -n "s/^const $2 = \([0-9][0-9]*\) \* time.Second$/\1/p" "$1" | head -1
}

# go_const_int FILE NAME — reads a bare `const NAME = <n>`.
go_const_int() {
  sed -n "s/^const $2 = \([0-9][0-9]*\)$/\1/p" "$1" | head -1
}

go_seconds() { go_const_seconds "${SHUTDOWN_SRC}" "$1"; }
run_budget=$(go_seconds runShutdownTimeout)
http_budget=$(go_seconds httpShutdownTimeout)
if [[ -z "${run_budget}" || -z "${http_budget}" ]]; then
  fail "shutdown constants readable" \
    "could not read runShutdownTimeout/httpShutdownTimeout as whole seconds from ${SHUTDOWN_SRC}"
else
  # Concurrent, so the process's budget is the larger of the two, not the sum.
  process_budget="${run_budget}"
  if (( http_budget > process_budget )); then
    process_budget="${http_budget}"
  fi
  if (( process_budget < grace_default )); then
    pass "process shutdown budget ${process_budget}s fits inside terminationGracePeriodSeconds ${grace_default}s"
  else
    fail "shutdown budget fits the grace period" \
      "process waits up to ${process_budget}s (max of runShutdownTimeout=${run_budget}s, httpShutdownTimeout=${http_budget}s) but the chart allows only ${grace_default}s before SIGKILL"
  fi

  # The run budget must also cover the engine's worst case: killGrace for the
  # applier's process-group SIGTERM -> SIGKILL escalation, plus
  # terminalSaveTimeout for the detached terminal-state write once the step
  # returns. cmd.WaitDelay is not a third window: os/exec starts that timer
  # the instant cmd.Cancel returns, the same moment the escalation goroutine
  # starts its own, so the two race concurrently rather than run back to
  # back -- see the WaitDelay doc comment in os/exec.
  kill_grace=$(sed -n 's/^var killGrace = \([0-9][0-9]*\) \* time.Second$/\1/p' internal/applier/exec.go | head -1)
  terminal_save=$(sed -n 's/^const terminalSaveTimeout = \([0-9][0-9]*\) \* time.Second$/\1/p' internal/engine/engine.go | head -1)
  if [[ -z "${kill_grace}" || -z "${terminal_save}" ]]; then
    fail "killGrace/terminalSaveTimeout readable" \
      "could not read killGrace from internal/applier/exec.go and terminalSaveTimeout from internal/engine/engine.go as whole seconds"
  elif (( run_budget >= kill_grace + terminal_save )); then
    pass "runShutdownTimeout ${run_budget}s covers killGrace + terminalSaveTimeout (${kill_grace}s + ${terminal_save}s)"
  else
    fail "runShutdownTimeout covers the engine's worst case" \
      "runShutdownTimeout=${run_budget}s but cancellation can take killGrace=${kill_grace}s + terminalSaveTimeout=${terminal_save}s"
  fi

  # Decide runs synchronously inside handleDecide, so its own Save call
  # (bounded by decideSaveTimeout, not the run's execution context -- Task 7
  # threads request contexts through it) is bounded by the HTTP drain, not
  # the run-cancellation one. Nothing today stops decideSaveTimeout from
  # being raised past httpShutdownTimeout and letting shutdown abandon an
  # in-flight decision write mid-Save; pin it the same way terminal_save is
  # pinned against run_budget above.
  decide_save=$(sed -n 's/^const decideSaveTimeout = \([0-9][0-9]*\) \* time.Second$/\1/p' internal/engine/engine.go | head -1)
  if [[ -z "${decide_save}" ]]; then
    fail "decideSaveTimeout readable" \
      "could not read decideSaveTimeout from internal/engine/engine.go as whole seconds"
  elif (( http_budget >= decide_save )); then
    pass "httpShutdownTimeout ${http_budget}s covers decideSaveTimeout ${decide_save}s"
  else
    fail "httpShutdownTimeout covers Decide's worst case" \
      "httpShutdownTimeout=${http_budget}s but handleDecide's Save call can take decideSaveTimeout=${decide_save}s"
  fi
fi

# --- Invariant 8: single writer against the run ConfigMap ------------------
# The chart sets replicas: 1 but, with no strategy key, defaults to
# RollingUpdate, whose maxSurge rounds up to 1 -- so old and new pods overlap
# during every upgrade. Both would recover from and write to the same run
# checkpoint concurrently, against a design that assumes exactly one writer.
# strategy: Recreate accepts a few seconds of downtime on upgrade instead.
echo "== single writer =="
strategy_type=$(render | doc Deployment | yq -r '.spec.strategy.type' | val)
if [[ "${strategy_type}" == "Recreate" ]]; then
  pass "Deployment strategy is Recreate"
else
  fail "Deployment strategy" "got '${strategy_type}', want 'Recreate' -- RollingUpdate overlaps two writers against the same run ConfigMap during every upgrade"
fi

# --- Invariant 9: the startup budget fits inside the probe that kills for it -
# Shutdown has three pinned budgets above; startup had none, and its margin was
# five seconds held up by an accident. cmd/aicrme resolves the run store's
# ownerReference (deploymentLookupTimeout) and then runs eng.Recover, whose
# every ConfigMap call is bounded by cmStoreCallTimeout, all BEFORE
# httpSrv.ListenAndServe -- so against an API server that accepts connections
# and never answers, nothing serves /healthz until that whole sum elapses, and
# the probe below starts failing the pod in the meantime. 2b-i's regression was
# exactly this shape (an unbounded WaitForCacheSync ahead of the listener) and
# cost a permanent CrashLoopBackOff.
#
# The accident: loadCurrentRetryable (internal/engine/recover.go) retries
# ErrCodeInternal only, and a hung API server surfaces as ErrCodeTimeout from
# withCallTimeout -- so the load does NOT consume all maxLoadAttempts. Widening
# that set to include ErrCodeTimeout reads like an obvious improvement (a
# timeout IS transient) and triples the load half of the budget. This
# assertion reads the retry set from source so that edit fails here instead of
# in a customer's CrashLoopBackOff.
echo "== startup budget =="

# probe_num PROBE FIELD DEFAULT — a probe field off the rendered Deployment,
# falling back to the Kubernetes default when the chart leaves it unset.
probe_num() {
  local got
  got=$(render | doc Deployment | yq -r ".spec.template.spec.containers[].$1.$2" | val)
  if [[ -z "${got}" ]]; then got="$3"; fi
  printf '%s' "${got}"
}

# A startupProbe, when present, is what governs startup: Kubernetes disables
# the liveness and readiness probes entirely until it succeeds. Without one,
# the liveness probe is the clock, which is the case today.
if [[ "$(render | doc Deployment | yq -r '.spec.template.spec.containers[] | has("startupProbe")' | val)" == "true" ]]; then
  probe="startupProbe"
else
  probe="livenessProbe"
fi

probe_initial=$(probe_num "${probe}" initialDelaySeconds 0)
probe_period=$(probe_num "${probe}" periodSeconds 10)
probe_threshold=$(probe_num "${probe}" failureThreshold 3)

lookup_budget=$(go_const_seconds cmd/aicrme/main.go deploymentLookupTimeout)
cm_call_budget=$(go_const_seconds internal/engine/cmstore.go cmStoreCallTimeout)
max_load_attempts=$(go_const_int internal/engine/recover.go maxLoadAttempts)

if [[ -z "${lookup_budget}" || -z "${cm_call_budget}" || -z "${max_load_attempts}" ]]; then
  fail "startup constants readable" \
    "could not read deploymentLookupTimeout (cmd/aicrme/main.go), cmStoreCallTimeout (internal/engine/cmstore.go), and maxLoadAttempts (internal/engine/recover.go)"
elif [[ -z "${probe_initial}" || -z "${probe_period}" || -z "${probe_threshold}" ]]; then
  fail "${probe} readable" "could not read initialDelaySeconds/periodSeconds/failureThreshold for the container's ${probe}"
else
  # How many cmStoreCallTimeout windows a hung API server can actually cost
  # the load path, read from loadCurrentRetryable's own body rather than
  # assumed.
  retry_set=$(awk '/^func loadCurrentRetryable/,/^}/' internal/engine/recover.go)
  if grep -q 'ErrCodeTimeout' <<<"${retry_set}"; then
    load_attempts="${max_load_attempts}"
    retry_note="loadCurrentRetryable retries ErrCodeTimeout, so a hung API server consumes all ${max_load_attempts} attempts"
  else
    load_attempts=1
    retry_note="loadCurrentRetryable excludes ErrCodeTimeout, so a hung API server costs one attempt, not ${max_load_attempts}"
  fi
  startup_budget=$(( lookup_budget + load_attempts * cm_call_budget ))

  # The pod dies on the failureThreshold-th CONSECUTIVE failure, so the last
  # probe that can still save it fires at initialDelay + period*(threshold-1),
  # one period earlier than the naive initialDelay + period*threshold. Using
  # the naive figure would make this assertion pass in a window where the pod
  # is already being killed.
  kill_at=$(( probe_initial + probe_period * (probe_threshold - 1) ))

  if (( startup_budget < kill_at )); then
    pass "startup budget ${startup_budget}s fits inside the ${probe}'s ${kill_at}s (${retry_note})"
  else
    fail "startup budget fits the ${probe}" \
      "worst-case startup is ${startup_budget}s (deploymentLookupTimeout=${lookup_budget}s + ${load_attempts} x cmStoreCallTimeout=${cm_call_budget}s) but the ${probe} kills the pod at ${kill_at}s (initialDelaySeconds=${probe_initial} + periodSeconds=${probe_period} x (failureThreshold=${probe_threshold} - 1)) -- ${retry_note}"
  fi
fi

# --- Invariant 10: the run store's identity is the chart's, not a guess -----
# main.go resolves the run ConfigMap's ownerReference with ONE Get against
# AICRME_DEPLOYMENT_NAME, so that env var must name this very Deployment
# object. If it drifts, the Get 404s, newRunStore logs an error, and the
# console silently falls back to an in-memory store -- /healthz stays green
# while the whole phase's durability is gone. Both sides come from
# aicrme.fullname today; nothing but this asserts they still agree.
echo "== run store identity =="
dep_name=$(render | doc Deployment | yq -r '.metadata.name' | val)
dep_env_name=$(render | doc Deployment |
               yq -r '.spec.template.spec.containers[].env[] | select(.name == "AICRME_DEPLOYMENT_NAME") | .value' | val)
if [[ -n "${dep_name}" && "${dep_env_name}" == "${dep_name}" ]]; then
  pass "AICRME_DEPLOYMENT_NAME (${dep_env_name}) names the Deployment object"
else
  fail "AICRME_DEPLOYMENT_NAME" \
    "env value '${dep_env_name}' does not equal the Deployment's metadata.name '${dep_name}' -- the ownerReference lookup would 404 and run state would silently stop surviving restarts"
fi

# Checked under a release name the fullname helper does NOT collapse
# (contains "aicrme" is false), because that is the branch where the two
# could realistically diverge.
alt_dep_name=$(helm template other "${CHART}" | doc Deployment | yq -r '.metadata.name' | val)
alt_env_name=$(helm template other "${CHART}" | doc Deployment |
               yq -r '.spec.template.spec.containers[].env[] | select(.name == "AICRME_DEPLOYMENT_NAME") | .value' | val)
if [[ -n "${alt_dep_name}" && "${alt_env_name}" == "${alt_dep_name}" ]]; then
  pass "AICRME_DEPLOYMENT_NAME follows the Deployment name under a non-collapsing release name (${alt_dep_name})"
else
  fail "AICRME_DEPLOYMENT_NAME under an alternate release name" \
    "env value '${alt_env_name}' does not equal the Deployment's metadata.name '${alt_dep_name}'"
fi

# The run store's ConfigMap is created at runtime and must never be templated:
# a templated one reverts to the chart's rendered content on every
# `helm upgrade`, wiping the state an in-flight Apply is actively
# checkpointing. main.go names it "<fullname>-run" (runStoreSuffix).
run_cm=$(render | doc ConfigMap | yq -r '.metadata.name' | grep -c "^${dep_name}-run$" || true)
if [[ "${run_cm}" == "0" ]]; then
  pass "no template renders the run store ConfigMap ${dep_name}-run"
else
  fail "run store ConfigMap is not templated" \
    "the chart renders a ConfigMap named ${dep_name}-run -- helm upgrade would revert it to the chart's content and wipe an in-flight run's checkpoints"
fi

echo
if [[ "${FAILURES}" -gt 0 ]]; then
  printf '\033[0;31mFAIL\033[0m: %s chart contract assertion(s) failed\n' "${FAILURES}"
  exit 1
fi
printf '\033[0;32mPASS\033[0m: chart contract holds\n'
