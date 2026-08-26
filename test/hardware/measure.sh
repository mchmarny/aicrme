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
#   test/hardware/measure.sh <run-id> <tokenized-url>
# where <run-id> is a run that has COMPLETED an Apply on this cluster and
# <tokenized-url> is the URL the console printed on startup, verbatim --
# http://127.0.0.1:PORT/?t=TOKEN. The token is exchanged once for a session
# cookie, exactly as the browser does. Run this against the SAME console
# process that produced the run: the cookie dies with it.
set -euo pipefail

RUN_ID="${1:-}"
LAUNCH_URL="${2:-}"

if [[ -z "${RUN_ID}" || -z "${LAUNCH_URL}" ]]; then
  echo "usage: $0 <run-id> <tokenized-url>" >&2
  echo "  <run-id>         must be a run that already completed Apply on this cluster" >&2
  echo "  <tokenized-url>  the http://127.0.0.1:PORT/?t=... line the console printed" >&2
  exit 2
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../e2e/lib.sh
source "${SCRIPT_DIR}/../e2e/lib.sh"

CONSOLE_URL="${LAUNCH_URL%%/\?t=*}"
CONSOLE_JAR="$(mktemp -t aicrme-measure-jar.XXXXXX)"
export CONSOLE_URL CONSOLE_JAR
trap 'rm -f "${CONSOLE_JAR}"' EXIT
curl -fsS -c "${CONSOLE_JAR}" -X POST "${CONSOLE_URL}/api/session" \
  -H 'Content-Type: application/json' \
  -d "{\"token\":\"${LAUNCH_URL##*t=}\"}" >/dev/null

run_json() { e2e_api GET "/api/runs/$1" --max-time 15 2>/dev/null || true; }

hdr() { echo; echo "════════ $* ════════"; }
missing() { echo "  ⚠ NOTHING MATCHED — treat as a broken probe until confirmed: $*"; }

RECORD="$(run_json "${RUN_ID}")"
[[ -n "${RECORD}" ]] || { echo "no run record for ${RUN_ID} at ${CONSOLE_URL}" >&2; exit 1; }

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
hdr "Q2  how large a real snapshot gets (docs/phase-2-handoff.md:282)"
# The 800 KiB figure this question was written against was a ConfigMap limit
# and is gone: a file store has no such cap, and filePayloadCeiling is 64 MiB
# precisely so shedding is unreachable. The measurement is still worth taking
# -- a real cluster's snapshot carries more nodes and real device and driver
# detail than the 66-73 KB KWOK fixtures this project sized against -- but the
# question it answers is now "how big does this actually get", not "does it
# fit".
SNAP="$(e2e_api GET "/api/runs/${RUN_ID}/artifacts/snapshot.yaml" --max-time 30 2>/dev/null || true)"
if [[ -z "${SNAP}" ]]; then
  # The artifact endpoint may not exist; fall back to the persisted record,
  # which is a file under <work-dir>/runs/<cluster-uid>/ rather than a
  # ConfigMap. It is gzipped and envelope-encoded, so this reads its SIZE
  # rather than its content -- which is all Q2 is asking about.
  RECORD_FILE="$(find "${AICRME_WORK_DIR:-${HOME}/.aicrme}/runs" -name "${RUN_ID}.run" 2>/dev/null | head -1)"
  if [[ -n "${RECORD_FILE}" ]]; then
    echo "  (the artifact endpoint returned nothing; sizing the persisted record ${RECORD_FILE} instead)"
    echo "  persisted record:    $(wc -c <"${RECORD_FILE}" | tr -d ' ') bytes, already gzipped"
    echo "  filePayloadCeiling:  67108864 bytes (internal/engine/filestore.go)"
  fi
fi
if [[ -z "${SNAP}" ]]; then
  missing "could not read snapshot.yaml by either route"
else
  RAW=$(printf '%s' "${SNAP}" | wc -c | tr -d ' ')
  GZ=$(printf '%s' "${SNAP}" | gzip -9 | wc -c | tr -d ' ')
  echo "  snapshot raw:        ${RAW} bytes"
  echo "  snapshot gzipped:    ${GZ} bytes"
  echo "  filePayloadCeiling:  67108864 bytes"
  echo "  headroom:            $(( 67108864 - GZ )) bytes"
  [[ "${GZ}" -gt 67108864 ]] && echo "  ⇒ OVER THE CEILING. Runs on this cluster will checkpoint truncated."
  [[ "${GZ}" -gt 819200 ]] && echo "  ⇒ over the OLD 800 KiB ConfigMap limit -- worth recording, since the in-cluster console could not have stored this run at all."
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
echo "    curl -b <jar> ${CONSOLE_URL}/api/runs/${RUN_ID}/events > events.jsonl"

# ---------------------------------------------------------------------------
hdr "Q5  event volume on a real driver rollout (docs/phase-2-handoff.md:230)"
# The MECHANISM is pinned by TestPodUnchangedTroubleEmitsExactlyOnce and its
# siblings: ten identical informer deliveries publish one bus event. What is
# unmeasured is the real-world number on an 8-node driver rollout, where pods
# genuinely churn rather than repeating one state.
EVENTS="$(e2e_api GET "/api/runs/${RUN_ID}/events" --max-time 30 2>/dev/null | wc -l | tr -d ' ' || true)"
if [[ -z "${EVENTS}" || "${EVENTS}" == "0" ]]; then
  missing "the events endpoint returned nothing — check the path before concluding the bus is quiet"
else
  echo "  events published for this run: ${EVENTS}"
  echo "  replayCapacity (internal/console/console.go): 20000"
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
