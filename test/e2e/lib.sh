#!/usr/bin/env bash
# Shared helpers for the Kind-based e2e scripts (smoke.sh,
# discover-recommend.sh). Sourced, not executed: callers already have
# `set -euo pipefail` and their own CLUSTER/NS/IMAGE in scope.

# e2e_diagnose dumps what a human needs before a failing cluster disappears:
# pod status, recent namespace events, and the console's own logs.
e2e_diagnose() {
  local ns="$1"
  echo "--- FAILURE: diagnostics before teardown ---" >&2
  kubectl -n "${ns}" get pods -o wide >&2 2>&1 || true
  kubectl -n "${ns}" get events --sort-by=.lastTimestamp >&2 2>&1 || true
  kubectl -n "${ns}" logs deploy/aicrme --all-containers --tail=500 >&2 2>&1 || true
}

# e2e_build_and_load_image builds the console image and loads it into the
# named Kind cluster so the chart install never reaches out to a registry.
e2e_build_and_load_image() {
  local cluster="$1" image="$2"
  make image IMAGE="${image}"
  kind load docker-image "${image}" --name "${cluster}"
}

# e2e_install_chart installs the aicrme chart from a locally-loaded image
# and waits for the Deployment to roll out.
e2e_install_chart() {
  local ns="$1" image="$2"
  helm install aicrme charts/aicrme -n "${ns}" --create-namespace \
    --set image.repository="${image%:*}" --set image.tag="${image#*:}" \
    --set image.pullPolicy=Never --wait --timeout 5m
  kubectl -n "${ns}" rollout status deploy/aicrme --timeout=120s
}

# e2e_admin_password reads the generated console password from the
# aicrme-auth Secret.
e2e_admin_password() {
  local ns="$1"
  kubectl -n "${ns}" get secret aicrme-auth -o jsonpath='{.data.password}' | base64 -d
}

# e2e_login authenticates against the console at addr ($1, host:port),
# storing the session cookie in the jar at $2, using the password read from
# the aicrme-auth Secret in namespace $3. Content-Type is mandatory: the
# same-origin CSRF middleware (internal/api/server.go) treats a mutating
# request carrying neither Origin nor Sec-Fetch-Site as same-origin only
# when its Content-Type is not one a plain HTML <form> can produce -- curl
# sends neither header, so omitting this would get the login rejected.
e2e_login() {
  local addr="$1" jar="$2" ns="$3"
  local password
  password="$(e2e_admin_password "${ns}")"
  curl -fsS -c "${jar}" -X POST "http://${addr}/api/login" \
    -H 'Content-Type: application/json' \
    -d "{\"username\":\"admin\",\"password\":\"${password}\"}"
}
