#!/usr/bin/env bash
# repro-kai-reinstall.sh — does a REINSTALLED kai-scheduler still place a gang?
#
# WHY THIS EXISTS
# After a Reset, a second aicrme cycle installs 16/16 components and then
# fails in Prove: 0/2 gang members placed inside a 3-minute budget, where a
# first install places in seconds. Reproduced on real GKE H100s 2026-08-26.
# test/e2e/reset.sh:49-56 records a weaker form of the same thing on KWOK and
# absorbs it by widening the budget from 45s to 3m rather than explaining it.
#
# This script isolates ONE variable. It does not install a recipe, drive the
# console, or run aicrme at all: it installs kai-scheduler alone, at the exact
# chart version and with the exact values AICR generates, places a gang,
# uninstalls, reinstalls, and places the same gang again. If cycle 2 is slower
# or fails, the cause is inside that loop and nothing else -- 15 other
# components, the console, and the whole engine are excluded by construction.
#
# THE TWO STATIC FINDINGS IT IS BUILT TO TEST
# Both were read out of the pinned chart (v0.14.1) and AICR's values, and
# neither needs a cluster to establish:
#
#   1. templates/default-shard.yaml annotates SchedulingShard/default with
#      `helm.sh/resource-policy: keep`. That is an explicit instruction to
#      helm NOT to delete it on uninstall. The shard therefore survives every
#      Reset by chart design -- teardown removes helm releases, and this is
#      the one object in the release helm is told to leave alone.
#
#   2. AICR sets `postCleanup.enabled: false`, which suppresses the chart's
#      post-delete-cleanup Job. That Job deletes the leftover Deployments AND
#      `Config/kai-config`. AICR's stated reason is that the hook "does not
#      inherit global.tolerations and will hang Pending on clusters with
#      tainted nodes"; in v0.14.1 templates/hooks/post/post-delete-job.yaml
#      does inherit global.tolerations, so the reason no longer holds for the
#      pinned version. The comment also asserts "helm uninstall already
#      removes the deployments this hook would delete" and says nothing about
#      kai-config, which helm does NOT remove -- it is a pre-install hook
#      resource, and hook resources are not part of the release manifest.
#
# MODES
#   CYCLES=<n>   how many install/place/uninstall cycles to run (default 3)
#   PURGE=<n>    from cycle <n> onward, delete kai's surviving CRs between the
#                uninstall and the next install (default 3). This is the
#                candidate fix, applied as the experiment's treatment arm: if
#                cycle 2 fails and cycle 3 succeeds, the residue is the cause
#                and purging it is the remedy. Set PURGE=99 to disable and get
#                a pure control run.
#   KEEP=1       leave the Kind cluster standing for inspection afterwards.
#
# It creates and destroys its OWN Kind cluster and touches nothing else. Every
# cluster call is pinned to that cluster's context -- see e2e_kubectl.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=../e2e/lib.sh
source "${SCRIPT_DIR}/../e2e/lib.sh"
cd "${REPO_ROOT}"

# ITS OWN KUBECONFIG, not the operator's.
#
# kind writes the cluster it creates into $KUBECONFIG and, on a laptop, that
# is the same file holding live production contexts -- the file whose
# current-context an unrelated `gcloud get-credentials` rewrote mid-run on
# 2026-08-26 while a teardown suite was in flight. Pointing this experiment at
# a throwaway file means it cannot add to, reorder, or repoint the real one,
# and the cluster it creates disappears with the file. Set REPRO_KUBECONFIG to
# override if you want the cluster reachable from your normal kubectl after a
# KEEP=1 run.
KUBECONFIG="${REPRO_KUBECONFIG:-$(mktemp -t aicrme-repro-kubeconfig.XXXXXX)}"
export KUBECONFIG

CLUSTER="${CLUSTER:-aicrme-repro-kai}"
CYCLES="${CYCLES:-3}"
PURGE="${PURGE:-3}"
GANG_NS="${GANG_NS:-repro-gang}"
PLACE_BUDGET="${PLACE_BUDGET:-180}"

# Pinned to what the 2026-08-26 failure ran, read out of that run's own
# bundle (~/.aicrme/runs/<id>/bundle/011-kai-scheduler/upstream.env). A
# different version would be a different experiment.
KAI_CHART='oci://ghcr.io/kai-scheduler/kai-scheduler/kai-scheduler'
KAI_VERSION="${KAI_VERSION:-v0.14.1}"

# helm reaches ghcr anonymously through a registry config of its own. Without
# this it inherits ~/.docker/config.json, which on a machine with no docker
# CLI names a credential helper that is not installed, and every pull fails
# with `docker-credential-osxkeychain: executable file not found`.
REG_CFG="$(mktemp -t aicrme-repro-reg.XXXXXX.json)"
echo '{}' >"${REG_CFG}"

cleanup() {
  rm -f "${REG_CFG}" "${VALUES:-}"
  if [[ "${KEEP:-0}" != "1" ]]; then
    kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
    [[ -z "${REPRO_KUBECONFIG:-}" ]] && rm -f "${KUBECONFIG}"
  else
    echo "KEEP=1: cluster ${CLUSTER} left standing"
    echo "KEEP=1: reach it with KUBECONFIG=${KUBECONFIG} kubectl --context kind-${CLUSTER} ..."
  fi
  return 0
}
trap cleanup EXIT

helm_kai() { helm --kube-context "kind-${CLUSTER}" --registry-config "${REG_CFG}" "$@"; }

hdr() { echo; echo "════════ $* ════════"; }

# ---------------------------------------------------------------------------
# AICR's generated values, verbatim from the bundle of the run that failed.
# Reproduced here rather than read from ~/.aicrme so this script works on a
# machine that never had that run -- but they must stay identical, and the
# comment above each block records what AICR says it is for.
VALUES="$(mktemp -t aicrme-repro-values.XXXXXX.yaml)"
cat >"${VALUES}" <<'EOF'
admission:
  gpuPodRuntimeClassName: ""
global:
  tolerations:
    - operator: Exists
postCleanup:
  enabled: false
EOF

# ---------------------------------------------------------------------------
# The gang. internal/prove/workload.yaml reduced to what this experiment
# needs: two pods, all-or-nothing on kai's default queue, requesting the GPUs
# only the fake accelerated nodes advertise. Placement is read off
# spec.nodeName -- the field the scheduler writes the instant it binds --
# exactly as internal/prove/client.go's PlacedNodes does, and for the same
# reason: it means "scheduled" on a cluster with no kubelet running.
gang_yaml() {
  cat <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: repro-gang-$1
  namespace: ${GANG_NS}
spec:
  completions: 2
  parallelism: 2
  completionMode: Indexed
  backoffLimit: 0
  template:
    metadata:
      labels:
        app: repro-gang-$1
      annotations:
        kai.scheduler/queue: default
    spec:
      schedulerName: kai-scheduler
      restartPolicy: Never
      tolerations:
        - key: kwok.x-k8s.io/node
          operator: Equal
          value: fake
          effect: NoSchedule
        - key: nvidia.com/gpu
          operator: Exists
          effect: NoSchedule
      containers:
        - name: allreduce
          image: busybox:1.36
          command: ["sh", "-c", "echo placement proven; sleep infinity"]
          resources:
            limits:
              nvidia.com/gpu: 8
EOF
}

# place_gang creates gang <n> and returns the seconds it took for BOTH pods to
# be bound, or the string "TIMEOUT". Prints progress as it goes so a run that
# is going to fail says so while it is failing rather than after.
place_gang() {
  local n="$1" start now bound elapsed
  e2e_kubectl apply -f <(gang_yaml "${n}") >/dev/null
  start="$(date +%s)"
  while :; do
    bound="$(e2e_kubectl -n "${GANG_NS}" get pods -l "app=repro-gang-${n}" \
      -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' 2>/dev/null \
      | grep -c . || true)"
    now="$(date +%s)"
    elapsed=$((now - start))
    if [[ "${bound}" -ge 2 ]]; then
      echo "${elapsed}"
      return 0
    fi
    if [[ "${elapsed}" -ge "${PLACE_BUDGET}" ]]; then
      echo "TIMEOUT"
      return 0
    fi
    sleep 2
  done
}

# purge_kai_residue removes what a helm uninstall provably leaves behind.
#
# This is the candidate remedy, written as narrowly as the ownership rule
# requires: named kinds from kai's OWN api groups, nothing discovered by
# walking namespaces, and nothing outside the two groups the chart installs
# CRDs for. It is what "reset aicrme's own recipes" means for this component.
# PURGE_SCOPE selects how wide the remedy is, so the two can be compared:
#
#   chart  (default) delete the FOUR NAMED OBJECTS the chart itself creates
#          and then tells helm to keep. This is what aicrme can defensibly
#          remove: it installed kai, so the objects kai's chart created are
#          its own -- and a Queue an operator wrote by hand has a different
#          name and is left alone. This is the scope internal/teardown
#          implements.
#   all    delete every instance of every kai CRD plus the namespace. Wider
#          than ownership justifies -- it would take an operator's own Queues
#          with it -- and kept only as the comparison that proves `chart` is
#          sufficient rather than merely smaller.
PURGE_SCOPE="${PURGE_SCOPE:-chart}"

# The four objects, by name, with the reason each survives. All four are
# created by the chart and all four outlive `helm uninstall` by design:
#   SchedulingShard/default        templates/default-shard.yaml, resource-policy: keep
#   Queue/default-parent-queue     templates/default-queue.yaml, resource-policy: keep
#   Queue/default-queue            templates/default-queue.yaml, resource-policy: keep
#   Config/kai-config              templates/kai-config.yaml, a pre-install HOOK, and
#                                  hook resources are not in the release manifest
# The shard is the one that matters -- it owns the kai-scheduler-default
# Deployment, which is why a reinstall keeps running the PREVIOUS
# generation's scheduler pod -- but all four are equally ours and equally
# invisible to helm.
purge_kai_residue() {
  echo "  purging kai residue (scope=${PURGE_SCOPE}):"
  if [[ "${PURGE_SCOPE}" == "chart" ]]; then
    e2e_kubectl delete schedulingshard.kai.scheduler default --ignore-not-found --timeout=60s 2>&1 | sed 's/^/    /' || true
    e2e_kubectl delete queue.scheduling.run.ai default-queue --ignore-not-found --timeout=60s 2>&1 | sed 's/^/    /' || true
    e2e_kubectl delete queue.scheduling.run.ai default-parent-queue --ignore-not-found --timeout=60s 2>&1 | sed 's/^/    /' || true
    e2e_kubectl -n kai-scheduler delete config.kai.scheduler kai-config --ignore-not-found --timeout=60s 2>&1 | sed 's/^/    /' || true
    return 0
  fi
  # Order matters: the CRs first, then the CRDs that define them. Deleting a
  # CRD removes its instances without running their finalizers, which strands
  # nothing here but would if any instance were still owned by a live
  # controller -- and by this point kai is uninstalled, so none is.
  for kind in podgroups.scheduling.run.ai queues.scheduling.run.ai \
              bindrequests.scheduling.run.ai schedulingshards.kai.scheduler \
              configs.kai.scheduler topologies.kai.scheduler; do
    if e2e_kubectl get "${kind}" --all-namespaces >/dev/null 2>&1; then
      local n
      n="$(e2e_kubectl get "${kind}" --all-namespaces -o name 2>/dev/null | grep -c . || true)"
      echo "    ${kind}: ${n} instance(s)"
      e2e_kubectl delete "${kind}" --all --all-namespaces --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
    fi
  done
  # The namespace itself: kai's release leaves Leases and ServiceAccounts
  # behind, and a Lease from a previous generation's leader election is one of
  # the few objects that can make a fresh controller wait rather than act.
  e2e_kubectl delete ns kai-scheduler --ignore-not-found --timeout=120s >/dev/null 2>&1 || true
  echo "    namespace kai-scheduler: deleted"
}

# residue_report prints what survived, using the inventory script so the two
# stay consistent -- a second implementation of "what is left" would drift.
residue_report() {
  "${SCRIPT_DIR}/reset-residue.sh" "kind-${CLUSTER}" "$1" \
    | sed -n '/R3  CRDs/,/R5  admission/p'
}

# ---------------------------------------------------------------------------
# One worker, not the three test/e2e/reset.sh uses. That script installs a
# 14-component recipe; this one installs kai-scheduler and nothing else, and
# its six small Deployments fit on a single worker with room to spare. The
# GPU nodes the gang lands on are KWOK fakes and cost no memory at all. On a
# 3.6 GB podman VM the four-node shape lost its control plane mid-bringup,
# which is a machine-size problem rather than a finding -- but there is no
# reason to pay for nodes this experiment does not use.
hdr "cluster: kind ${CLUSTER} (control-plane + 1 worker), KWOK, 4 fake H100 nodes"
KIND_CFG="$(mktemp -t aicrme-repro-kind.XXXXXX.yaml)"
cat >"${KIND_CFG}" <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
EOF
kind create cluster --name "${CLUSTER}" --config "${KIND_CFG}" --wait 180s
rm -f "${KIND_CFG}"

e2e_install_kwok
e2e_apply_kwok_nodes
e2e_kubectl create ns "${GANG_NS}" >/dev/null 2>&1 || true

RESULTS=()
for cycle in $(seq 1 "${CYCLES}"); do
  hdr "CYCLE ${cycle} of ${CYCLES}"

  if [[ "${cycle}" -ge "${PURGE}" && "${cycle}" -gt 1 ]]; then
    echo "--- TREATMENT ARM: purging kai residue before this install"
    purge_kai_residue
  elif [[ "${cycle}" -gt 1 ]]; then
    echo "--- CONTROL ARM: installing on top of whatever the uninstall left"
  fi

  echo "--- helm install kai-scheduler ${KAI_VERSION}"
  helm_kai upgrade --install kai-scheduler "${KAI_CHART}" \
    --version "${KAI_VERSION}" \
    --namespace kai-scheduler --create-namespace \
    -f "${VALUES}" --wait --timeout 10m

  echo "--- kai-scheduler pods:"
  e2e_kubectl -n kai-scheduler get pods -o wide

  # A control-plane pod that landed on a FAKE node is not running: KWOK marks
  # it Running with no container behind it. That would make every measurement
  # below meaningless, so it is checked rather than assumed.
  FAKED="$(e2e_kubectl -n kai-scheduler get pods -o json \
    | jq -r '[.items[] | select((.spec.nodeName // "") | startswith("gpu-") or startswith("system-"))] | length')"
  if [[ "${FAKED}" != "0" ]]; then
    echo "  ⚠ ${FAKED} kai pod(s) landed on simulated nodes and are not really running."
    echo "    Every placement number below is invalid. Pin kai to real nodes and rerun."
  fi

  echo "--- placing gang ${cycle} (budget ${PLACE_BUDGET}s)"
  T="$(place_gang "${cycle}")"
  echo "  ⇒ cycle ${cycle} placement: ${T}${T:+$([[ "${T}" == "TIMEOUT" ]] || echo "s")}"
  RESULTS+=("cycle ${cycle}: ${T}")

  if [[ "${T}" == "TIMEOUT" ]]; then
    echo "--- the gang did not place. Evidence:"
    e2e_kubectl -n "${GANG_NS}" get pods -o wide 2>/dev/null || true
    e2e_kubectl -n "${GANG_NS}" get events --sort-by=.lastTimestamp 2>/dev/null | tail -20 || true
    echo "--- podgroups:"
    e2e_kubectl get podgroups.scheduling.run.ai --all-namespaces 2>/dev/null || true
    echo "--- pod-grouper log:"
    PG="$(e2e_kubectl -n kai-scheduler get pods -o json 2>/dev/null \
      | jq -r '.items[] | select([.spec.containers[].name] | any(test("grouper"))) | .metadata.name' | head -1)"
    [[ -n "${PG}" ]] && e2e_kubectl -n kai-scheduler logs "${PG}" --tail=120 2>/dev/null | tail -60
    echo "--- scheduler log:"
    SC="$(e2e_kubectl -n kai-scheduler get pods -o json 2>/dev/null \
      | jq -r '.items[] | select(.metadata.name | test("^scheduler")) | .metadata.name' | head -1)"
    [[ -n "${SC}" ]] && e2e_kubectl -n kai-scheduler logs "${SC}" --tail=120 2>/dev/null | tail -60
  fi

  # Tear the gang down before the uninstall, exactly as Prove's own cleanup
  # does: a pending gang holding a queue across an uninstall would confound
  # the next cycle with a variable this experiment is not testing.
  e2e_kubectl -n "${GANG_NS}" delete job "repro-gang-${cycle}" --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1 || true

  if [[ "${cycle}" -lt "${CYCLES}" ]]; then
    echo "--- helm uninstall (what Reset does, and all it does)"
    helm_kai uninstall kai-scheduler -n kai-scheduler --ignore-not-found --wait --timeout 5m || true
    echo "--- residue after uninstall:"
    residue_report "after-uninstall-${cycle}"
  fi
done

hdr "RESULT"
for r in "${RESULTS[@]}"; do echo "  ${r}"; done
echo
echo "Read it as: cycle 1 is the baseline. A cycle in the CONTROL arm that is"
echo "dramatically slower or TIMEOUT reproduces the failure. A cycle in the"
echo "TREATMENT arm (>= PURGE=${PURGE}) that returns to baseline confirms the"
echo "residue is the cause and that purging it is the remedy."
