#!/usr/bin/env bash
# Drives the full Discover -> Recommend -> Apply arc against a live Kind
# cluster through the real HTTP API, with Apply running deploy.sh
# --dry-run: create the cluster, install the KWOK controller and AICR's
# simulated H100 nodes, build and load the PRODUCTION console image,
# install the chart, log in, create a run, answer intent=training/
# platform=kubeflow, download the generated bundle at the confirm gate,
# click through it, and assert components install (dry-run) in the SSE
# stream. This is Phase 2a's exit gate: the first time the whole arc runs
# against a live cluster inside the production container image.
#
# CRITICAL -- why this drives the console over HTTP against the image
# e2e_build_and_load_image builds and loads into Kind, and must NEVER be
# "optimised" into invoking deploy.sh (or the aicrme binary) on the host:
# an earlier Phase 2a probe (docs/phase-2a-task-1-findings.md) ran
# `deploy.sh --dry-run` successfully, but on the HOST, with helm v4.2.4 and
# kubectl v1.36.3. The production image pins helm 3.19.0 and kubectl
# 1.34.1 (Dockerfile ARG HELM_VERSION / ARG KUBECTL_VERSION), and the
# generated install.sh BRANCHES on helm's major version -- helm>=4 gets
# `--force-conflicts` and server-side apply, helm 3 gets neither. Different
# command line, different apply semantics. The host probe's green result
# therefore proves nothing about what this console actually ships; only a
# run through the real image, on the real helm 3 binary, does. See the
# in-image helm-major assertion below, which exists so this can never
# silently drift again.
#
# Why simulated GPU nodes, the KWOK setup, and training/kubeflow: see
# discover-recommend.sh's header -- this script shares that cluster setup
# via lib.sh's e2e_install_kwok/e2e_apply_kwok_nodes and drives the same
# resolvable (intent, platform) pair.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "${SCRIPT_DIR}/lib.sh"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

CLUSTER="${CLUSTER:-aicrme-e2e-apply}"
NS="${NS:-aicrme}"
IMAGE="${IMAGE:-aicrme:e2e-apply}"
PORT="${PORT:-18082}"
ADDR="localhost:${PORT}"

JAR="$(mktemp -t aicrme-apply-jar.XXXXXX)"
TARBALL="$(mktemp -t aicrme-apply-bundle.XXXXXX.tar.gz)"
PF_PID=""
ec=0

# dump_recent_events prints the last 80 SSE events for a failed run --
# more than discover-recommend.sh's 50, since Apply alone emits one event
# per component per attempt across up to 14 components.
dump_recent_events() {
  echo "--- last 80 SSE events ---" >&2
  set +e
  curl -fsS -b "${JAR}" --max-time 5 "http://${ADDR}/api/events?since=0" 2>/dev/null \
    | sed -n 's/^data: //p' | tail -n 80 >&2
  set -e
}

# fail_run prints the run's error, then exits 1. The SSE dump itself happens
# once, in cleanup, while the port-forward is still alive -- see cleanup's
# ordering below.
fail_run() {
  local run_json="$1"
  echo "run failed: $(echo "${run_json}" | jq -r '.error // "unknown error"')" >&2
  echo "full run: ${run_json}" >&2
  exit 1
}

cleanup() {
  local exit_code="$1"
  # Diagnostics run BEFORE killing the port-forward, and exactly once here:
  # dump_recent_events curls the console through PF_PID, so dumping after
  # the kill would come back empty.
  if [[ "${exit_code}" -ne 0 ]]; then
    e2e_diagnose "${NS}"
    dump_recent_events
  fi
  if [[ -n "${PF_PID}" ]]; then
    kill "${PF_PID}" 2>/dev/null || true
  fi
  rm -f "${JAR}" "${TARBALL}"
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

echo "--- build and load PRODUCTION console image (this is the whole point -- see header)"
e2e_build_and_load_image "${CLUSTER}" "${IMAGE}"

echo "--- install chart"
e2e_install_chart "${NS}" "${IMAGE}"

# Both overrides land in one `kubectl set env` / one rollout, not two: two
# separate calls would roll the Deployment twice and double the wait.
# AICRME_SNAPSHOT_NODE_SELECTOR: see discover-recommend.sh's header for why
# the snapshot agent must be pinned off the tainted simulated GPU nodes.
# AICRME_APPLY_DRY_RUN: internal/steps.ApplyConfig.DryRun -> the applier
# sets DRY_RUN_FLAG=--dry-run for deploy.sh (internal/applier/applier.go),
# which every generated install.sh interpolates into its `helm upgrade
# --install` invocation. Nothing is installed; every chart is still
# fetched and rendered through the real helm binary.
echo "--- pin the snapshot agent off the simulated GPU nodes and enable Apply's dry-run"
kubectl -n "${NS}" set env deploy/aicrme \
  'AICRME_SNAPSHOT_NODE_SELECTOR=node-role.kubernetes.io/control-plane=' \
  'AICRME_APPLY_DRY_RUN=true'
kubectl -n "${NS}" rollout status deploy/aicrme --timeout=120s

# The whole reason this script exists rather than just trusting the host
# probe: assert the shipped image's helm major actually matches what the
# Dockerfile pins, so a base-image bump that silently changes helm's major
# version surfaces here, in CI, rather than in a customer's install.
echo "--- assert in-image helm major matches the Dockerfile pin"
DOCKERFILE_HELM_VERSION="$(sed -n 's/^ARG HELM_VERSION=\(.*\)$/\1/p' "${REPO_ROOT}/Dockerfile")"
[[ -n "${DOCKERFILE_HELM_VERSION}" ]] || {
  echo "could not read ARG HELM_VERSION from Dockerfile" >&2
  exit 1
}
DOCKERFILE_HELM_MAJOR="${DOCKERFILE_HELM_VERSION%%.*}"
IMAGE_HELM_VERSION="$(kubectl -n "${NS}" exec deploy/aicrme -- helm version --template '{{.Version}}')"
IMAGE_KUBECTL_VERSION="$(kubectl -n "${NS}" exec deploy/aicrme -- kubectl version --client=true -o json | jq -r '.clientVersion.gitVersion')"
IMAGE_HELM_MAJOR="$(echo "${IMAGE_HELM_VERSION}" | sed -nE 's/^v?([0-9]+)\..*/\1/p')"
echo "Dockerfile pins helm ${DOCKERFILE_HELM_VERSION} (major ${DOCKERFILE_HELM_MAJOR}); in-image helm reports ${IMAGE_HELM_VERSION}; in-image kubectl reports ${IMAGE_KUBECTL_VERSION}"
[[ -n "${IMAGE_HELM_MAJOR}" ]] || {
  echo "could not parse a major version out of in-image helm's own version string: ${IMAGE_HELM_VERSION}" >&2
  exit 1
}
[[ "${IMAGE_HELM_MAJOR}" == "${DOCKERFILE_HELM_MAJOR}" ]] || {
  echo "in-image helm major (${IMAGE_HELM_MAJOR}, from ${IMAGE_HELM_VERSION}) does not match the Dockerfile pin (${DOCKERFILE_HELM_MAJOR}, from HELM_VERSION=${DOCKERFILE_HELM_VERSION}) -- the built image and the Dockerfile have drifted" >&2
  exit 1
}

kubectl -n "${NS}" port-forward "svc/aicrme" "${PORT}:8080" >/dev/null 2>&1 &
PF_PID=$!
sleep 3

echo "--- login"
e2e_login "${ADDR}" "${JAR}" "${NS}"

echo "--- POST /api/runs"
RUN_JSON="$(curl -fsS -b "${JAR}" -X POST "http://${ADDR}/api/runs" -H 'Content-Type: application/json')"
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
  RUN_JSON="$(curl -fsS -b "${JAR}" "http://${ADDR}/api/runs/${RUN_ID}")"
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
curl -fsS -b "${JAR}" -X POST "http://${ADDR}/api/runs/${RUN_ID}/decide" \
  -H 'Content-Type: application/json' \
  -d '{"intent":"training","platform":"kubeflow"}' >/dev/null

echo "--- poll until the run parks a second time (Recommend + Bundle complete, Apply's confirm gate)"
STATE=""
for _ in $(seq 1 60); do
  RUN_JSON="$(curl -fsS -b "${JAR}" "http://${ADDR}/api/runs/${RUN_ID}")"
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
CONTENT_TYPE="$(curl -fsS -b "${JAR}" -o "${TARBALL}" -w '%{content_type}' "http://${ADDR}/api/runs/${RUN_ID}/bundle")"
[[ -s "${TARBALL}" ]] || {
  echo "bundle download was empty" >&2
  exit 1
}
[[ "${CONTENT_TYPE}" == "application/gzip" ]] || {
  echo "bundle Content-Type was '${CONTENT_TYPE}', expected application/gzip" >&2
  exit 1
}
tar -tzf "${TARBALL}" | grep -qx 'deploy.sh' || {
  echo "bundle tarball does not contain deploy.sh" >&2
  exit 1
}
echo "bundle downloaded: $(wc -c <"${TARBALL}" | tr -d ' ') bytes, contains deploy.sh"

echo "--- POST /api/runs/${RUN_ID}/decide {apply:yes}"
curl -fsS -b "${JAR}" -X POST "http://${ADDR}/api/runs/${RUN_ID}/decide" \
  -H 'Content-Type: application/json' \
  -d '{"apply":"yes"}' >/dev/null

# deploy.sh --dry-run still fetches every chart from its real upstream repo
# (helm.ngc.nvidia.com, ghcr.io, ...) and renders it through helm -- it
# skips only the install. And should a component fail to render, deploy.sh
# retries it with quadratic backoff (5s, 20s, 45s, 80s, 120s cap) before
# giving up, since it runs WITHOUT --best-effort. 40 minutes covers 14
# real chart fetches plus one full retry storm on the slowest CI runner.
echo "--- poll until done or failed (Apply)"
STATE=""
for _ in $(seq 1 240); do
  RUN_JSON="$(curl -fsS -b "${JAR}" "http://${ADDR}/api/runs/${RUN_ID}")"
  STATE="$(echo "${RUN_JSON}" | jq -r '.state')"
  [[ "${STATE}" == "done" || "${STATE}" == "failed" ]] && break
  sleep 10
done

echo "--- extracting component statuses from the SSE stream"
# set +e/-e around this pipeline for the same reason as dump_recent_events
# above: /api/events is a long-lived stream, so --max-time force-closes it
# mid-read, and curl's own exit status for that (28, CURLE_OPERATION_TIMEDOUT)
# would otherwise abort the whole script under pipefail before the STATE
# checks below ever run.
set +e
COMPONENT_STATUSES="$(curl -fsS -b "${JAR}" --max-time 10 "http://${ADDR}/api/events?since=0" 2>/dev/null \
  | sed -n 's/^data: //p' \
  | jq -r 'select(.kind=="component") | .data.status' 2>/dev/null | sort -u)"
set -e
echo "component statuses observed: $(echo "${COMPONENT_STATUSES}" | tr '\n' ' ')"

# state=="failed" is a hard failure here, not a soft "progress reached"
# floor: the host probe this repo's plan leaned on (deploy.sh --dry-run
# via helm v4.2.4, docs/phase-2a-task-1-findings.md) reached gate outcome
# 1 -- exit 0, all 14 components installed -- but that probe never ran
# through the shipped image's helm 3.19.0. Run against the real image,
# component 3/14 (network-operator) fails deterministically: its chart
# renders a NodeFeatureRule CR owned by nfd's CRD, but nfd (component
# 2/14) was itself only ever dry-run, so that CRD was never actually
# registered with the API server. helm 3's --dry-run builds typed k8s
# objects client-side against live discovery and fails hard ("no matches
# for kind NodeFeatureRule ... ensure CRDs are installed first"); the host
# probe's helm 4 -- whose own --dry-run is deprecated in favor of
# --dry-run=client, implying its default already round-trips the server --
# tolerated the same missing CRD. This is a genuine, reproducible defect
# in the shipped toolchain path, not a flake: asserting through it here
# instead of softening the check is deliberate, so it stays visible in CI
# until it's actually fixed rather than silently downgrading this gate.
[[ "${STATE}" == "failed" ]] && fail_run "${RUN_JSON}"
[[ "${STATE}" == "done" ]] || {
  echo "run did not reach done or failed within the deadline (state=${STATE})" >&2
  fail_run "${RUN_JSON}"
}

# This is the assertion that proves Apply actually drove deploy.sh through
# the marker parser end to end, not just that the run reached done with no
# error -- same reasoning as discover-recommend.sh's componentCount check.
echo "${COMPONENT_STATUSES}" | grep -qx 'installed' || {
  echo "no component reached status=installed in the SSE stream" >&2
  fail_run "${RUN_JSON}"
}

echo "PASS: apply-dryrun e2e green (run ${RUN_ID}, helm ${IMAGE_HELM_VERSION} in-image, state=done)"
