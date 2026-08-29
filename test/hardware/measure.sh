#!/usr/bin/env bash
# measure.sh — answer the questions that only real GPU hardware can answer.
#
# WHY THIS EXISTS
# Six open questions are blocked on real GPU hardware. Every one of them is a MEASUREMENT, not a build: the code
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
# cookie, exactly as the browser does.
#
# RUN IT BEFORE STOPPING ANYTHING, against the SAME console process that
# produced the run. Two of the six answers exist only inside that process: the
# session cookie dies with it, and so does the event bus, which is in memory
# and never written to disk. A console stopped first makes Q4 and Q5
# unrecoverable for that run.
#
# Two environment variables it reads:
#   SSE_WINDOW       seconds to hold the event stream open (default 10). The
#                    backlog arrives as one burst, so this is a burst-drain
#                    budget, not a run duration.
#   AICRME_WORK_DIR  must match what the console was started with, or Q2
#                    cannot find the persisted record (default ~/.aicrme).
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
hdr "Q1  MIG resource names"
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
hdr "Q2  how large a real snapshot gets"
# The 800 KiB figure this question was written against was a ConfigMap limit
# and is gone: a file store has no such cap, and filePayloadCeiling is 64 MiB
# precisely so shedding is unreachable. The measurement is still worth taking
# -- a real cluster's snapshot carries more nodes and real device and driver
# detail than the 66-73 KB KWOK fixtures this project sized against -- but the
# question it answers is now "how big does this actually get", not "does it
# fit".
#
# NO API ROUTE SERVES THE SNAPSHOT, and none is coming: engine.Run declares
# `Artifacts map[string][]byte json:"-"` (internal/engine/run.go:63), so
# GET /api/runs/{id} omits every artifact by construction, and there is no
# /artifacts/ route to ask instead. This probe used to request one and reported
# NOTHING MATCHED on real hardware on 2026-08-26 for exactly that reason.
#
# So the record on disk IS the measurement, not a fallback: <work-dir>/runs/
# <cluster-uid>/<run-id>.run, gzipped and envelope-encoded (internal/engine/
# filestore.go, envelope.go). Its size is an upper bound on the snapshot --
# it carries capability.json and recipe.json too -- and an upper bound is what
# a ceiling question needs.
#
# `|| true` is load-bearing under `set -o pipefail`: find exits non-zero when
# the runs directory does not exist at all -- the single likeliest way to run
# this against the wrong work dir -- and without it the assignment kills the
# script here, silently, three questions short of the end.
RECORD_FILE="$(find "${AICRME_WORK_DIR:-${HOME}/.aicrme}/runs" -name "${RUN_ID}.run" 2>/dev/null | head -1 || true)"
if [[ -z "${RECORD_FILE}" ]]; then
  missing "no ${RUN_ID}.run under ${AICRME_WORK_DIR:-${HOME}/.aicrme}/runs — if the console was started with --work-dir, set AICRME_WORK_DIR to match before rerunning"
else
  SIZE=$(wc -c <"${RECORD_FILE}" | tr -d ' ')
  echo "  persisted record:    ${RECORD_FILE}"
  echo "  encoded size:        ${SIZE} bytes (gzipped; snapshot.yaml plus every other artifact)"
  echo "  filePayloadCeiling:  67108864 bytes (internal/engine/filestore.go)"
  echo "  headroom:            $(( 67108864 - SIZE )) bytes"
  [[ "${SIZE}" -gt 67108864 ]] && echo "  ⇒ OVER THE CEILING. Runs on this cluster checkpoint truncated."
  [[ "${SIZE}" -gt 819200 ]] && echo "  ⇒ over the OLD 800 KiB ConfigMap limit -- worth recording, since the in-cluster console could not have stored this run at all."
fi
# The product's own answer to "did it fit", which needs no arithmetic here:
# decodeRun populates Truncated with every artifact the store had to shed.
echo "${RECORD}" | jq -r '
  if ((.truncated // []) | length) == 0
  then "  truncated:           none — the store shed nothing"
  else "  ⇒ TRUNCATED: \(.truncated | join(", ")) — the record did not fit and this run cannot be retried"
  end' 2>/dev/null || missing "run record has no truncated field"

# ---------------------------------------------------------------------------
hdr "Q3  the headline number: N of M GPUs usable"
# Unit-tested, never exercised by a fixture: even the simulated-H100 KWOK
# snapshot reports gpu-present: false, because the agent finds no real device.
# This is the first time the number is computed from real devices.
#
# The run record does not carry this. It never did -- the `gap.json` artifact
# and `report` field this probe used to read do not exist on any type, and
# artifacts are unserialized anyway (see Q2). The console's own answer is on
# the connect payload, GET /api/cluster -> .nodes, and 439a7f8 is what made it
# truthful: gap.Analyze's headline comes from an in-pod PCI probe of the ONE
# node the agent landed on, which reported "8 of 8" for a sixteen-GPU cluster.
# Capacity vs allocatable is the distinction to read carefully -- what the
# hardware has, versus what a workload could schedule onto right now.
CLUSTER="$(e2e_api GET "/api/cluster" --max-time 15 2>/dev/null || true)"
if [[ -z "${CLUSTER}" ]]; then
  missing "GET /api/cluster returned nothing — this console is no longer connected, and the answer lives on the connect payload"
else
  echo "${CLUSTER}" | jq -r '.nodes // {} |
    "  usable GPUs:  \(.usableGPUs // 0) of \(.totalGPUs // 0) (allocatable of capacity)",
    "  GPU nodes:    \(.gpuNodes // 0) of \(.total // 0)",
    (["  shapes:"] + [.groups[]? |
        "\(.count)x \(.instanceType // "unknown")"
        + (if (.accelerator // "") == "" then "" else "  \(.accelerator)" end)
        + (if (.gpusPerNode // 0) == 0 then "  (no GPUs)" else "  \(.gpusPerNode) GPU/node" end)
        + (if .blocked then "  ⇒ BLOCKED: the agent cannot schedule here" else "" end)
        + (if .simulated then "  (simulated)" else "" end)] | join("\n    ")),
    (if (.more // 0) == 0 then empty else "    (+\(.more) further shapes the payload folded away)" end),
    (if (.remedy // "") == "" then empty else "  ⇒ a GPU shape is unreachable. Relaunch with AICRME_GPU_TOLERATIONS=\(.remedy)" end)' \
    2>/dev/null || missing "the cluster payload has no nodes composition — is this console older than f3e9254?"
fi
echo "  cross-check, straight from the nodes:"
kubectl get nodes -o json 2>/dev/null \
  | jq -r '[.items[] | select((.status.allocatable // {})["nvidia.com/gpu"] != null)]
           | "  nodes advertising GPUs: \(length), total allocatable: \([.[].status.allocatable["nvidia.com/gpu"] | tonumber] | add // 0)"' \
  || missing "no node advertises nvidia.com/gpu"

# ---------------------------------------------------------------------------
# The event stream, fetched ONCE here and shared by Q4 and Q5.
#
# GET /api/events is the only route there is -- no per-run one exists, and the
# `/api/runs/{id}/events` this script used to request never did. It is SSE, so
# three things follow, every one of them learned the expensive way when this
# script first ran on real hardware on 2026-08-26:
#
#   * The stream NEVER ends. --max-time is what terminates it, and curl exits
#     28 when it does -- success here, hence the `|| true`. Backlog replay is a
#     burst delivered at subscribe time (bus.Subscribe pre-fills the channel
#     before returning it), so the window only has to cover the burst, not the
#     run. Widen it with SSE_WINDOW if a very long run looks truncated.
#   * ?since=0 asks for the whole ring. Without it a subscriber is sent only
#     what arrives AFTER it connects, which on a finished run is nothing.
#   * `wc -l` counts framing, not events: every event is two lines plus a blank
#     one, an `event: epoch` frame precedes any replay, and a `: keepalive`
#     comment lands every 20s. Taking `data:` lines and filtering on runId
#     drops all of it -- the epoch frame carries no runId.
#
# The bus is in-memory per process (internal/console/console.go) and nothing
# writes it to disk. Stopping the console discards its events permanently, so
# run this BEFORE tearing anything down or the measurement is unrecoverable.
SSE_WINDOW="${SSE_WINDOW:-10}"
EVENTS_NDJSON="$(e2e_api GET "/api/events?since=0" --max-time "${SSE_WINDOW}" 2>/dev/null \
  | sed -n 's/^data: //p' \
  | jq -c --arg rid "${RUN_ID}" 'select(.runId == $rid)' 2>/dev/null || true)"
EVENT_COUNT=0
if [[ -n "${EVENTS_NDJSON}" ]]; then
  EVENT_COUNT="$(printf '%s\n' "${EVENTS_NDJSON}" | wc -l | tr -d ' ')"
fi

# ---------------------------------------------------------------------------
hdr "Q4  Apply duration and the slow-step map"
# The original estimate was 10-20 minutes on real hardware, most of it driver
# compilation. The first run to answer this found neither half true on GKE:
# 15m18s, but with the driver already on the node image, so the two slowest
# steps were kube-prometheus-stack (137s) and cert-manager (128s) at 44% of
# Apply and nothing else above 49s. web/src/slowSteps.ts was calibrated to
# that. Re-running this on a different cluster -- one whose nodes have no
# driver, above all -- is how that calibration gets checked rather than
# assumed: anything flagged below is a stall the operator is not told about.
echo "${RECORD}" | jq -r '
  "  run state:  \(.state)   phase: \(.phase)",
  "  started:    \(.startedAt)",
  "  updated:    \(.updatedAt)",
  "  components: \(.components | length)"' 2>/dev/null || missing "run record has no timing fields"

if [[ "${EVENT_COUNT}" == "0" ]]; then
  missing "no component events to time — see the Q4/Q5 note above on the event window"
else
  echo "  per-component wall time, slowest first (started → installed):"
  # RFC3339 by hand because jq's fromdateiso8601 accepts neither the
  # fractional seconds nor the numeric offset Go's time.Time emits. mktime
  # reads its input as UTC, so the offset is subtracted rather than added.
  printf '%s\n' "${EVENTS_NDJSON}" | jq -sr '
    def secs:
      capture("^(?<y>[0-9]{4})-(?<mo>[0-9]{2})-(?<d>[0-9]{2})T(?<h>[0-9]{2}):(?<mi>[0-9]{2}):(?<s>[0-9]{2})([.][0-9]+)?(?<z>Z|[-+][0-9]{2}:[0-9]{2})$")
      | ([(.y|tonumber), (.mo|tonumber)-1, (.d|tonumber), (.h|tonumber),
          (.mi|tonumber), (.s|tonumber), 0, 0] | mktime)
        - (if .z == "Z" then 0
           else (if (.z|startswith("-")) then -1 else 1 end)
                * ((.z[1:3]|tonumber) * 3600 + (.z[4:6]|tonumber) * 60)
           end);
    # Keep in step with EXACT in web/src/slowSteps.ts, or this question
    # re-reports components the console already explains. The name is bound
    # before the list is piped: inside `[...] | index(.)` the input is the
    # LIST, so index(.) searches the array for itself, finds it at 0, and
    # every component reads as covered.
    def covered: . as $name
      | endswith("-readiness")
      or (["gpu-operator", "kai-scheduler", "cert-manager", "kube-prometheus-stack"]
          | index($name) != null);
    map(select(.kind == "component" and ((.component // "") != "")))
    | group_by(.component)
    | map({ name:    .[0].component,
            started: (map(select(.data.status == "started") | .at) | min),
            done:    (map(select(.data.status == "installed" or .data.status == "failed") | .at) | max) })
    | map(select(.started != null and .done != null)
          | . + {seconds: ((.done | secs) - (.started | secs))})
    | (map(.seconds) | add) as $total
    | sort_by(-.seconds)
    | (.[] | "    \(.seconds | tostring | (" " * (5 - length)) + .)s  \(.name)\(if (.name | covered) then "" else "   ⇐ NOT in slowSteps.ts" end)"),
      "    ----- \($total)s summed across \(length) components"
  ' 2>/dev/null || missing "could not time any component — the events carried no started/installed pair"
  echo "  any component above with ⇐ takes real time and the operator is told nothing"
  echo "  about it; web/src/slowSteps.ts explains cert-manager, kube-prometheus-stack,"
  echo "  gpu-operator, kai-scheduler and *-readiness. That gap IS this question's"
  echo "  answer, and it is empty on the cluster the notes were calibrated against."
fi

# ---------------------------------------------------------------------------
hdr "Q5  event volume on a real driver rollout"
# The MECHANISM is pinned by TestPodUnchangedTroubleEmitsExactlyOnce and its
# siblings: ten identical informer deliveries publish one bus event. What is
# unmeasured is the real-world number on an 8-node driver rollout, where pods
# genuinely churn rather than repeating one state.
if [[ "${EVENT_COUNT}" == "0" ]]; then
  missing "the stream carried no event for this run — a quiet bus and a mis-parsed stream look identical, so check the frame count above before concluding"
else
  # Deliberately not a percentage: integer division rounds 397/20000 down to
  # 1%, and a ring-sizing decision should not be argued from a rounded number.
  echo "  events published for this run: ${EVENT_COUNT} of a 20000 ring"
  echo "  (replayCapacity, internal/console/console.go)"
  printf '%s\n' "${EVENTS_NDJSON}" | jq -sr '
    group_by(.kind) | sort_by(-length) | .[] | "    \(length)  \(.[0].kind)"' \
    || missing "could not break the count down by kind"
  [[ "${EVENT_COUNT}" -gt 20000 ]] && echo "  ⇒ OVER the ring. A late-joining tab cannot replay the whole run."
fi

# ---------------------------------------------------------------------------
hdr "Q6  does ValidateState still false-pass?"
# Validate was scoped out on 2026-08-18 because ValidateState reported `passed`
# for checks that never executed on simulated GPU nodes. That measurement was
# taken on KWOK. Whether it holds on real hardware is the question that decides
# whether Validate can come back, and it is NOT answered by this script --
# answering it means running ValidateState here and reading its per-check
# output against what the cluster actually does.
echo "  NOT probed. Deliberately: answering it means RUNNING ValidateState, which"
echo "  mutates nothing but is a real call this read-only script will not make."
echo "  The KWOK baseline to"
echo "  compare against, and run it by hand on this cluster."

hdr "done"
echo "Paste this whole output into the Phase 4 notes. A probe that printed"
echo "NOTHING MATCHED is a broken probe until proven otherwise -- none of these"
echo "has ever run against real GPU hardware."
