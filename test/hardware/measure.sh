#!/usr/bin/env bash
# measure.sh — answer the questions that only real GPU hardware can answer.
#
# WHY THIS EXISTS
# Six open questions in docs/phase-2-handoff.md and approach.md are blocked on
# Phase 4 hardware. Every one of them is a MEASUREMENT, not a build: the code
# to observe them already exists, and what has been missing is a cluster with
# real GPUs. Writing the probes beforehand means the expensive session is
# "run this, paste the output" rather than "work out what to collect while the
# meter runs".
#
# READ-ONLY. It installs nothing, changes nothing, and deletes nothing. It
# reads the cluster and one completed run's record through the console API.
#
# WHAT IT CANNOT DO, STATED RATHER THAN IMPLIED
# This script has never run against real GPU hardware -- that is the whole
# point of it -- so treat a probe that reports nothing as a bug in the probe
# until proven otherwise. Every probe therefore prints what it looked for as
# well as what it found, and says explicitly when it finds nothing, because a
# probe that silently matches zero rows reads exactly like a clean result.
#
# USAGE
#   test/hardware/measure.sh <run-id> [addr]
# where <run-id> is a run that has COMPLETED an Apply on this cluster, and
# addr defaults to 127.0.0.1:8080 (port-forward the console first).
set -euo pipefail

RUN_ID="${1:-}"
ADDR="${2:-127.0.0.1:8080}"
NS="${NS:-aicrme}"

if [[ -z "${RUN_ID}" ]]; then
  echo "usage: $0 <run-id> [addr]" >&2
  echo "  <run-id> must be a run that already completed Apply on this cluster" >&2
  exit 2
fi

JAR="$(mktemp -t aicrme-measure-jar.XXXXXX)"
trap 'rm -f "${JAR}"' EXIT

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../e2e/lib.sh
source "${SCRIPT_DIR}/../e2e/lib.sh"
e2e_login "${ADDR}" "${JAR}" "${NS}"

run_json() { curl -fsS --max-time 15 -b "${JAR}" "http://${ADDR}/api/runs/$1" 2>/dev/null || true; }

hdr() { echo; echo "════════ $* ════════"; }
missing() { echo "  ⚠ NOTHING MATCHED — treat as a broken probe until confirmed: $*"; }

RECORD="$(run_json "${RUN_ID}")"
[[ -n "${RECORD}" ]] || { echo "no run record for ${RUN_ID} at ${ADDR}" >&2; exit 1; }

# ---------------------------------------------------------------------------
hdr "Q1  MIG resource names (docs/phase-2-handoff.md:272)"
# internal/observer/handlers.go's gpuResource tracks only "nvidia.com/gpu".
# Whether MIG-partitioned nodes expose their capacity under different names --
# nvidia.com/mig-1g.10gb and friends -- decides whether the allocatable-diff
# treatment needs to widen. No MIG node exists in KWOK, so this has never been
# observable before.
echo "every nvidia.com/* resource name this cluster's nodes advertise:"
NAMES="$(kubectl get nodes -o json 2>/dev/null \
  | jq -r '[.items[].status.allocatable // {} | keys[]] | unique[] | select(startswith("nvidia.com/"))' || true)"
if [[ -z "${NAMES}" ]]; then
  missing "no nvidia.com/* resource on any node — is the device plugin up?"
else
  while IFS= read -r n; do echo "  ${n}"; done <<<"${NAMES}"
  if echo "${NAMES}" | grep -q "nvidia.com/mig-"; then
    echo "  ⇒ MIG NAMES PRESENT. gpuResource must widen; the constant is a single string today."
  else
    echo "  ⇒ no MIG names. gpuResource's single constant is sufficient for this cluster."
  fi
fi

# ---------------------------------------------------------------------------
hdr "Q2  the 800 KiB envelope guard (docs/phase-2-handoff.md:282)"
# maxPayload in internal/engine/envelope.go is sized against 66-73 KB, the only
# figure this project had: KWOK snapshot fixtures. A real cluster's snapshot
# carries more nodes and real device and driver detail. The guard fails closed
# either way; what is unknown is whether 800 KiB is generous or tight.
SNAP="$(curl -fsS --max-time 30 -b "${JAR}" "http://${ADDR}/api/runs/${RUN_ID}/artifacts/snapshot.yaml" 2>/dev/null || true)"
if [[ -z "${SNAP}" ]]; then
  # The artifact endpoint may not exist; fall back to the persisted record.
  SNAP="$(kubectl -n "${NS}" get cm aicrme-runs -o jsonpath='{.data}' 2>/dev/null || true)"
  echo "  (read from the ConfigMap store rather than an artifact endpoint)"
fi
if [[ -z "${SNAP}" ]]; then
  missing "could not read snapshot.yaml by either route"
else
  RAW=$(printf '%s' "${SNAP}" | wc -c | tr -d ' ')
  GZ=$(printf '%s' "${SNAP}" | gzip -9 | wc -c | tr -d ' ')
  echo "  snapshot raw:        ${RAW} bytes"
  echo "  snapshot gzipped:    ${GZ} bytes"
  echo "  maxPayload guard:    819200 bytes"
  echo "  headroom:            $(( 819200 - GZ )) bytes"
  [[ "${GZ}" -gt 819200 ]] && echo "  ⇒ OVER THE GUARD. Runs on this cluster will checkpoint truncated."
  [[ "${GZ}" -gt 409600 ]] && echo "  ⇒ over half the budget. 800 KiB is tight, not generous."
fi

# ---------------------------------------------------------------------------
hdr "Q3  the headline number: N of M GPUs usable (docs/phase-2-handoff.md:369)"
# Unit-tested, never exercised by a fixture: even the simulated-H100 KWOK
# snapshot reports gpu-present: false, because the agent finds no real device.
# This is the first time the number is computed from real devices.
echo "${RECORD}" | jq -r '{gap: (.artifacts["gap.json"] // "absent"), report: (.report // "absent")}' 2>/dev/null \
  | head -20 || missing "no gap/report field on the run record"
echo "  cross-check, straight from the nodes:"
kubectl get nodes -o json 2>/dev/null \
  | jq -r '[.items[] | select((.status.allocatable // {})["nvidia.com/gpu"] != null)]
           | "  nodes advertising GPUs: \(length), total allocatable: \([.[].status.allocatable["nvidia.com/gpu"] | tonumber] | add // 0)"' \
  || missing "no node advertises nvidia.com/gpu"

# ---------------------------------------------------------------------------
hdr "Q4  Apply duration and the slow-step map (approach.md:503)"
# approach.md predicts 10-20 minutes on real hardware, most of it driver
# compilation. web/src/slowSteps.ts tells the operator which steps are
# expected to hang; on KWOK every step is fast, so the map has never been
# calibrated against a step that genuinely takes ten minutes.
echo "${RECORD}" | jq -r '
  "  run state:  \(.state)   phase: \(.phase)",
  "  started:    \(.startedAt)",
  "  updated:    \(.updatedAt)",
  "  components: \(.components | length)"' 2>/dev/null || missing "run record has no timing fields"
echo "  per-component wall time is in the event stream; capture it with:"
echo "    curl -b <jar> http://${ADDR}/api/runs/${RUN_ID}/events > events.jsonl"

# ---------------------------------------------------------------------------
hdr "Q5  event volume on a real driver rollout (docs/phase-2-handoff.md:230)"
# The MECHANISM is pinned by TestPodUnchangedTroubleEmitsExactlyOnce and its
# siblings: ten identical informer deliveries publish one bus event. What is
# unmeasured is the real-world number on an 8-node driver rollout, where pods
# genuinely churn rather than repeating one state.
EVENTS="$(curl -fsS --max-time 30 -b "${JAR}" "http://${ADDR}/api/runs/${RUN_ID}/events" 2>/dev/null | wc -l | tr -d ' ' || true)"
if [[ -z "${EVENTS}" || "${EVENTS}" == "0" ]]; then
  missing "the events endpoint returned nothing — check the path before concluding the bus is quiet"
else
  echo "  events published for this run: ${EVENTS}"
  echo "  replayCapacity (cmd/aicrme/main.go): 20000"
  [[ "${EVENTS}" -gt 20000 ]] && echo "  ⇒ OVER the ring. A late-joining tab cannot replay the whole run."
fi

# ---------------------------------------------------------------------------
hdr "Q6  does ValidateState still false-pass? (docs/superpowers/specs, Validate)"
# Validate was scoped out on 2026-08-18 because ValidateState reported `passed`
# for checks that never executed on simulated GPU nodes. That measurement was
# taken on KWOK. Whether it holds on real hardware is the question that decides
# whether Validate can come back, and it is NOT answered by this script --
# answering it means running ValidateState here and reading its per-check
# output against what the cluster actually does.
echo "  NOT probed. Deliberately: answering it means RUNNING ValidateState, which"
echo "  mutates nothing but is a real call this read-only script will not make."
echo "  See docs/spikes/2026-08-18-validate-on-kwok.md for the KWOK baseline to"
echo "  compare against, and run it by hand on this cluster."

hdr "done"
echo "Paste this whole output into the Phase 4 notes. A probe that printed"
echo "NOTHING MATCHED is a broken probe until proven otherwise -- none of these"
echo "has ever run against real GPU hardware."
