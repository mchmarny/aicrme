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

echo
if [[ "${FAILURES}" -gt 0 ]]; then
  printf '\033[0;31mFAIL\033[0m: %s chart contract assertion(s) failed\n' "${FAILURES}"
  exit 1
fi
printf '\033[0;32mPASS\033[0m: chart contract holds\n'
