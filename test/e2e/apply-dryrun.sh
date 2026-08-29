#!/usr/bin/env bash
# Drives the full Discover -> Recommend -> Apply arc against a live Kind
# cluster through the real HTTP API, with Apply running deploy.sh
# --dry-run: create the cluster, install the KWOK controller and AICR's
# simulated H100 nodes, start the console against it, connect, create a run,
# answer intent=training/platform=kubeflow, download the generated bundle at
# the confirm gate, click through it, and assert components install (dry-run)
# in the SSE stream.
#
# WHAT THIS DRIVES, AND WHAT IT NO LONGER PINS
# It used to build and load the production container image and assert the
# in-image helm major against the Dockerfile's HELM_VERSION, because the image
# pinned helm 3 while a developer laptop had helm 4 -- and the generated
# install.sh BRANCHES on helm's major version (helm>=4 gets --force-conflicts
# and server-side apply, helm 3 gets neither). A host probe's green result
# therefore proved nothing about what the console shipped.
#
# There is no image, no Dockerfile and no pin any more: the binary uses
# whatever helm the operator has, which is the point of the delivery model.
# The property that assertion protected did not disappear, it moved --
# internal/console/preflight.go resolves bash/jq/helm/kubectl before anything
# touches a cluster and records the versions on the run, so the evidence
# bundle answers "which helm installed this" for a real install rather than a
# CI log answering it for a build. What this script pins instead is the
# behavior of whatever helm is actually present, which is now the honest
# question.
#
# Why simulated GPU nodes, the KWOK setup, and training/kubeflow: see
# discover-recommend.sh's header -- this script shares that cluster setup
# via lib.sh's e2e_install_kwok/e2e_apply_kwok_nodes and drives the same
# resolvable (intent, platform) pair.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "${SCRIPT_DIR}/lib.sh"

CLUSTER="${CLUSTER:-aicrme-e2e-apply}"
NS="${NS:-aicrme}"
TARBALL="$(mktemp -t aicrme-apply-bundle.XXXXXX.tar.gz)"
ec=0

# dump_recent_events prints the last 80 SSE events for a failed run --
# more than discover-recommend.sh's 50, since Apply alone emits one event
# per component per attempt across up to 14 components.
dump_recent_events() {
  [[ -n "${CONSOLE_URL:-}" ]] || return 0
  echo "--- last 80 SSE events ---" >&2
  set +e
  e2e_api GET '/api/events?since=0' --max-time 5 2>/dev/null \
    | sed -n 's/^data: //p' | tail -n 80 >&2
  set -e
}

# fail_run prints the run's error, then exits 1. The SSE dump itself happens
# once, in cleanup, while the console is still running -- see cleanup's
# ordering below.
fail_run() {
  local run_json="$1"
  echo "run failed: $(echo "${run_json}" | jq -r '.error // "unknown error"')" >&2
  echo "full run: ${run_json}" >&2
  exit 1
}

cleanup() {
  local exit_code="$1"
  # Diagnostics run BEFORE the console is stopped, and exactly once here:
  # dump_recent_events curls the console, so dumping after it exits would
  # come back empty.
  if [[ "${exit_code}" -ne 0 ]]; then
    e2e_diagnose "${NS}"
    dump_recent_events
  fi
  e2e_console_cleanup
  rm -f "${TARBALL}"
  kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
}
# Exit status is captured before cleanup runs anything else, and re-asserted
# with an explicit exit at the end: bash otherwise reports the EXIT trap's
# own last command status (from `kind delete cluster ... || true`, always 0)
# as the script's exit code, which would hide a real failure from CI.
trap 'ec=$?; cleanup "$ec"; exit "$ec"' EXIT

echo "--- create Kind cluster"
kind create cluster --name "${CLUSTER}" --wait 120s

e2e_install_kwok

echo "--- apply simulated H100 nodes (2 system + 4x p5.48xlarge)"
e2e_apply_kwok_nodes

# AICRME_SNAPSHOT_NODE_SELECTOR: see discover-recommend.sh's header for why
# the snapshot agent must be pinned off the tainted simulated GPU nodes.
# AICRME_SNAPSHOT_REQUESTS: see discover-recommend.sh's header for why the
# agent's 1000m CPU default does not fit on a CI runner's single real node.
# AICRME_APPLY_DRY_RUN: internal/steps.ApplyConfig.DryRun -> the applier
# sets DRY_RUN_FLAG=--dry-run for deploy.sh (internal/applier/applier.go),
# which every generated install.sh interpolates into its `helm upgrade
# --install` invocation. Nothing is installed; every chart is still
# fetched and rendered through the real helm binary.
echo "--- start the console with Apply's dry-run on"
export AICRME_SNAPSHOT_NODE_SELECTOR='node-role.kubernetes.io/control-plane='
export AICRME_SNAPSHOT_REQUESTS='cpu=200m'
export AICRME_APPLY_DRY_RUN='true'
e2e_start_console
e2e_connect

# Logged, not asserted against a pin: there is no Dockerfile to compare with,
# and this is the version the run itself will record and ship in its evidence
# bundle. It is here so a CI log still says which helm produced the outcome
# the pinned dry-run ceiling below describes -- that ceiling IS helm-major
# sensitive (helm 3 fails on the missing NodeFeatureRule CRD, helm 4.2.4 was
# observed tolerating it), so a reader diagnosing a drift needs this line.
HELM_VERSION="$(helm version --template '{{.Version}}')"
KUBECTL_VERSION="$(kubectl version --client=true -o json | jq -r '.clientVersion.gitVersion')"
echo "toolchain driving this run: helm ${HELM_VERSION}, kubectl ${KUBECTL_VERSION}"

echo "--- POST /api/runs"
RUN_JSON="$(e2e_api POST /api/runs -H 'Content-Type: application/json')"
RUN_ID="$(echo "${RUN_JSON}" | jq -r '.id')"
[[ -n "${RUN_ID}" && "${RUN_ID}" != "null" ]] || {
  echo "no run id in response: ${RUN_JSON}" >&2
  exit 1
}
echo "run id: ${RUN_ID}"

# Discover deploys the AICR snapshot agent as a Kubernetes Job (image pull +
# schedule + run + report), so this loop allows several minutes.
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

PENDING="$(echo "${RUN_JSON}" | jq -cS '.pending')"
[[ "${PENDING}" == '["intent","platform"]' ]] || {
  echo "unexpected pending decisions: ${PENDING}" >&2
  exit 1
}

echo "--- POST /api/runs/${RUN_ID}/decide {intent:training, platform:kubeflow}"
e2e_api POST "/api/runs/${RUN_ID}/decide" \
  -H 'Content-Type: application/json' \
  -d '{"intent":"training","platform":"kubeflow"}' >/dev/null

echo "--- poll until the run parks a second time (Recommend + Bundle complete, Apply's confirm gate)"
STATE=""
for _ in $(seq 1 60); do
  RUN_JSON="$(e2e_api GET "/api/runs/${RUN_ID}")"
  STATE="$(echo "${RUN_JSON}" | jq -r '.state')"
  [[ "${STATE}" == "awaiting_decision" || "${STATE}" == "failed" ]] && break
  sleep 3
done
[[ "${STATE}" == "failed" ]] && fail_run "${RUN_JSON}"
[[ "${STATE}" == "awaiting_decision" ]] || {
  echo "run did not park at the confirm gate within the deadline (state=${STATE})" >&2
  fail_run "${RUN_JSON}"
}

PENDING="$(echo "${RUN_JSON}" | jq -cS '.pending')"
# This is the confirm gate: the console must not begin mutating the cluster
# until a human clicks. Asserting pending == ["apply"] here -- not merely
# state == awaiting_decision -- is what proves the gate actually fired
# rather than, say, the run stalling for an unrelated reason that happens
# to also park it.
[[ "${PENDING}" == '["apply"]' ]] || {
  echo "confirm gate did not fire as expected: pending=${PENDING}" >&2
  exit 1
}
echo "confirm gate fired: pending == [\"apply\"]"

echo "--- GET /api/runs/${RUN_ID}/bundle"
# -w writes the response's Content-Type to stdout after the body is
# streamed to -o, so this is one request, not a HEAD-then-GET pair.
CONTENT_TYPE="$(e2e_api GET "/api/runs/${RUN_ID}/bundle" -o "${TARBALL}" -w '%{content_type}')"
[[ -s "${TARBALL}" ]] || {
  echo "bundle download was empty" >&2
  exit 1
}
[[ "${CONTENT_TYPE}" == "application/gzip" ]] || {
  echo "bundle Content-Type was '${CONTENT_TYPE}', expected application/gzip" >&2
  exit 1
}
# Capture tar's listing before matching it, rather than piping into
# `grep -q`. Under `set -o pipefail`, `writer | grep -q` is unsound whenever
# the writer keeps producing after the match: grep -q exits at the first hit
# and closes the pipe, the writer takes SIGPIPE/EPIPE, and pipefail then
# reports the WRITER's failure -- so a SUCCESSFUL assertion fails the script.
# Reproduced directly: `seq 1 500000 | grep -qx 3` exits 141 under pipefail.
#
# This is not a timing race, which is why it looked machine-specific. macOS
# ships bsdtar, which tolerates the truncated write and exits 0; GNU tar, on
# the Linux CI runner, reports "tar: stdout: write error" and exits non-zero.
# So it passed on every developer laptop and failed on the very first CI run,
# claiming the bundle did not contain deploy.sh about a tarball that did.
BUNDLE_ENTRIES="$(tar -tzf "${TARBALL}")"
grep -qx 'deploy.sh' <<<"${BUNDLE_ENTRIES}" || {
  echo "bundle tarball does not contain deploy.sh" >&2
  exit 1
}
echo "bundle downloaded: $(wc -c <"${TARBALL}" | tr -d ' ') bytes, contains deploy.sh"

echo "--- POST /api/runs/${RUN_ID}/decide {apply:yes}"
e2e_api POST "/api/runs/${RUN_ID}/decide" \
  -H 'Content-Type: application/json' \
  -d '{"apply":"yes"}' >/dev/null

# deploy.sh --dry-run still fetches every chart from its real upstream repo
# (helm.ngc.nvidia.com, ghcr.io, ...) and renders it through helm -- it
# skips only the install. And should a component fail to render, deploy.sh
# retries it with quadratic backoff (5s, 20s, 45s, 80s, 120s cap) before
# giving up, since it runs WITHOUT --best-effort. 40 minutes covers 14
# real chart fetches plus one full retry storm on the slowest CI runner.
#
# This loop's ceiling never bites today -- the dry-run ceiling below fails
# the run at component 3/14 within minutes -- but it is sized for a world
# where that ceiling has moved or been resolved and the run genuinely goes
# the distance. Budget check: this loop's 40m + the Discover poll's 7.5m +
# the Recommend/Bundle poll's 3m + 10-15m of cluster/image/chart setup can
# total roughly an hour in the worst case these loops permit.
# .github/workflows/e2e.yaml's apply-dryrun job sets timeout-minutes: 75 to
# actually cover that worst case -- if either number changes, change both
# together so the job's ceiling and this script's own budget keep agreeing;
# a job that times out before the script's own diagnostics ever print loses
# exactly the failure detail cleanup()/fail_run() exist to capture.
echo "--- poll until done or failed (Apply)"
STATE=""
for _ in $(seq 1 240); do
  RUN_JSON="$(e2e_api GET "/api/runs/${RUN_ID}")"
  STATE="$(echo "${RUN_JSON}" | jq -r '.state')"
  [[ "${STATE}" == "done" || "${STATE}" == "failed" ]] && break
  sleep 10
done

echo "--- extracting the SSE stream (component statuses, the failing component, and its error)"
# set +e/-e around this fetch for the same reason as dump_recent_events
# above: /api/events is a long-lived stream, so --max-time force-closes it
# mid-read, and curl's own exit status for that (28, CURLE_OPERATION_TIMEDOUT)
# would otherwise abort the whole script under pipefail before the STATE
# checks below ever run. The same force-close can also truncate the FINAL
# line mid-object -- `jq -R -c 'fromjson? // empty'` re-parses each line on
# its own and drops (rather than errors on) any that don't parse, so a
# truncated tail is silently discarded here, once, at the source, instead
# of resurfacing as a `jq` parse failure (and a script abort, under
# pipefail, hiding which downstream assertion never got to run) in every
# one of the several jq calls below that read EVENTS. EVENTS is read once
# and queried multiple times rather than re-fetched, since the stream is
# replayed from the start (since=0) and would return the same content
# again anyway.
set +e
EVENTS="$(e2e_api GET '/api/events?since=0' --max-time 10 2>/dev/null \
  | sed -n 's/^data: //p' \
  | jq -R -c 'fromjson? // empty')"
set -e

COMPONENT_STATUSES="$(echo "${EVENTS}" | jq -r 'select(.kind=="component") | .data.status' | sort -u)"
echo "component statuses observed: $(echo "${COMPONENT_STATUSES}" | tr '\n' ' ')"

# This is the assertion that proves Apply actually drove deploy.sh through
# the marker parser end to end, not just that the run reached some
# terminal state with no error -- same reasoning as discover-recommend.sh's
# componentCount check.
grep -qx 'installed' <<<"${COMPONENT_STATUSES}" || {
  echo "no component reached status=installed in the SSE stream" >&2
  fail_run "${RUN_JSON}"
}

# ---- The dry-run ceiling: pinned, not asserted away ----
#
# state=="done" is NOT reachable here, and this is not a toolchain defect
# to chase: on a REAL install, deploy.sh runs components in numbered
# order, so nfd (2/14) actually installs and registers its
# NodeFeatureRule CRD before network-operator (3/14) -- which renders one
# -- ever runs. The chain works. But under --dry-run nothing is ever
# really installed, so a later component's dependency on an earlier
# component's CRD can never be satisfied, by helm 3, helm 4, or anything
# else. helm 3's --dry-run builds typed k8s objects client-side against
# live discovery and fails hard when the kind isn't there ("no matches
# for kind NodeFeatureRule ... ensure CRDs are installed first"); the
# earlier host probe (the "dry-run ceiling" measurement; helm v4.2.4)
# happened to tolerate the same missing CRD and
# reached exit 0 on all 14 components -- arguably the LESS correct
# behavior, and evidence that probe validated a code path (install.sh
# branches on helm major) production doesn't take, not evidence this
# console is broken.
#
# So this is the genuine ceiling of dry-run verification for an ordered
# bundle with cross-component CRD dependencies, not a bug awaiting a fix:
# full-chain validation needs a real install, which is Phase 4's job.
# Asserting state=="done" would therefore never pass; asserting nothing
# more than "it failed somewhere" would never catch a real regression
# either. So the expected outcome is pinned by name: THIS component, AT
# THIS position, failing with THIS reason. If any of the three drifts --
# a different component fails, network-operator moves to a different
# recipe position, or the error stops mentioning NodeFeatureRule -- something
# real changed (upstream chart, recipe shape, or deploy.sh itself) and
# this assertion must break so a human re-verifies the ceiling, rather
# than silently keep passing (or silently keep failing) through it.
#
# Pinning EXPECTED_FAILING_INDEX in addition to the component name is
# stricter than strictly required (the name alone already identifies the
# component), and it is a second, independent thing that can legitimately
# drift on an AICR recipe-catalog bump even when network-operator itself
# is unaffected -- e.g. a new component inserted earlier in the recipe
# would shift network-operator's position without changing anything
# about the CRD-ordering limitation itself. That is an acceptable,
# expected reason for this specific check to need updating; it is called
# out here so a future reader isn't surprised that a routine AICR bump
# can legitimately break this one line without indicating a real bug.
EXPECTED_FAILING_COMPONENT="network-operator"
EXPECTED_FAILING_INDEX="3"

[[ "${STATE}" == "failed" ]] || {
  echo "run did not fail as the known dry-run ceiling predicts (state=${STATE}) -- if this now reaches done, the network-operator/nfd CRD-ordering limitation may have been resolved upstream; a human needs to re-verify and update this pinned assertion, not just let it pass" >&2
  fail_run "${RUN_JSON}"
}

FAILED_COMPONENT="$(echo "${EVENTS}" | jq -r 'select(.kind=="component" and .data.status=="failed") | .data.name' | tail -n 1)"
[[ "${FAILED_COMPONENT}" == "${EXPECTED_FAILING_COMPONENT}" ]] || {
  echo "run failed at an unexpected component: got '${FAILED_COMPONENT}', expected '${EXPECTED_FAILING_COMPONENT}' -- this is a real regression (or a real fix, if the previously-failing component now succeeds), not the known dry-run ceiling; investigate before touching this assertion" >&2
  fail_run "${RUN_JSON}"
}

FAILED_INDEX="$(echo "${EVENTS}" | jq -r --arg c "${EXPECTED_FAILING_COMPONENT}" \
  'select(.kind=="component" and .data.name==$c and .data.status=="started") | .data.index' | tail -n 1)"
[[ "${FAILED_INDEX}" == "${EXPECTED_FAILING_INDEX}" ]] || {
  echo "${EXPECTED_FAILING_COMPONENT} failed at recipe position ${FAILED_INDEX}, expected ${EXPECTED_FAILING_INDEX} -- the recipe shape changed; re-verify the dry-run ceiling still applies" >&2
  fail_run "${RUN_JSON}"
}

ERROR_TAIL="$(echo "${EVENTS}" | jq -r 'select(.kind=="error" and .data.tail != null) | .data.tail[]')"
grep -q 'no matches for kind "NodeFeatureRule"' <<<"${ERROR_TAIL}" || {
  echo "${EXPECTED_FAILING_COMPONENT} failed, but not with the known nfd CRD-ordering error -- a different, unverified failure mode; investigate before touching this assertion" >&2
  fail_run "${RUN_JSON}"
}

echo "PASS: apply-dryrun e2e green (run ${RUN_ID}, helm ${HELM_VERSION}; confirm gate fired, bundle downloaded, >=1 component installed, and the known dry-run ceiling confirmed: fails at ${EXPECTED_FAILING_COMPONENT} (${EXPECTED_FAILING_INDEX}/14) on the nfd CRD-ordering limitation, exactly as pinned)"
