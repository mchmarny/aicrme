#!/usr/bin/env bash
# reset-residue.sh — inventory what a Reset leaves behind.
#
# WHY THIS EXISTS
# Reset uninstalls helm releases and nothing else (internal/teardown), and
# helm has never deleted CRDs on uninstall. So a cluster that has been through
# one Discover→Prove→Reset cycle still holds every CRD the recipe installed,
# every custom resource inside those CRDs, and every namespace. A second cycle
# installs 16/16 cleanly on top of that residue and then fails in Prove: the
# gang does not place, 0/2 members, where a first install places in seconds.
# Reproduced on real GKE H100s on 2026-08-26, and visible in weaker form on
# KWOK -- test/e2e/reset.sh:49-56 records the second-cycle gang missing a 45s
# budget that a first install clears in about two seconds, and widens the
# budget to 3m rather than explaining it.
#
# The 2026-08-26 session suspected the SchedulingShard, deleted it, restarted
# every kai-scheduler deployment, and the retry failed identically. That
# exonerates the shard and nothing else: the leftover PodGroups and Queues
# were never enumerated, let alone removed. This script enumerates them.
#
# READ-ONLY. It installs nothing, changes nothing, deletes nothing.
#
# HOW TO USE IT — it is a snapshot, so take two and diff them:
#   test/hardware/reset-residue.sh <context> before-cycle-1 > /tmp/r0.txt
#   ... run cycle 1, then Reset ...
#   test/hardware/reset-residue.sh <context> after-reset    > /tmp/r1.txt
#   diff /tmp/r0.txt /tmp/r1.txt
# Anything in r1 that is not in r0 is residue this cluster did not start with,
# and is what the next install inherits.
#
# THE CONTEXT IS A REQUIRED ARGUMENT, NOT A DEFAULT. Every cluster call here
# is pinned to it. Defaulting to `kubectl config current-context` is exactly
# the hazard that on 2026-08-26 aimed a teardown suite at a live H100 cluster
# when an unrelated `gcloud get-credentials` rewrote the shared kubeconfig
# mid-run; this script is read-only, but it is meant to be read alongside
# scripts that are not, and a habit of passing the context explicitly is worth
# more than the two seconds it costs.
set -euo pipefail

CONTEXT="${1:-}"
MOMENT="${2:-unlabeled}"

if [[ -z "${CONTEXT}" ]]; then
  echo "usage: $0 <kube-context> [moment-label]" >&2
  echo "  <kube-context>   pinned explicitly; there is deliberately no default" >&2
  echo "  [moment-label]   free text recorded in the header, e.g. after-reset" >&2
  echo >&2
  echo "available contexts:" >&2
  kubectl config get-contexts -o name >&2 2>/dev/null || true
  exit 2
fi

KC=(kubectl --context "${CONTEXT}" --request-timeout=30s)

if ! "${KC[@]}" version -o json >/dev/null 2>&1; then
  echo "cannot reach a cluster on context ${CONTEXT}" >&2
  exit 1
fi

hdr() { echo; echo "════════ $* ════════"; }
missing() { echo "  ⚠ NOTHING MATCHED — treat as a broken probe until confirmed: $*"; }

echo "context:  ${CONTEXT}"
echo "moment:   ${MOMENT}"
echo "server:   $("${KC[@]}" version -o json 2>/dev/null | jq -r '.serverVersion.gitVersion // "unknown"')"

# RECIPE_GROUPS are the API groups the AICR recipe's components own. Instances
# are counted only for these: a cluster runs hundreds of CRDs it installed
# itself (a cloud provider's own, the ones kube-system ships with), and
# listing every instance of every one of them turns a diff into noise and the
# script into a multi-minute wait. Everything outside this list is still
# REPORTED by name below, just not enumerated -- so a group that belongs here
# and is missing shows up as an unexpected CRD rather than as silence.
RECIPE_GROUPS=(
  scheduling.run.ai      # kai-scheduler: podgroups, queues, bindrequests
  kai.scheduler          # kai-scheduler: schedulingshards
  nvidia.com             # gpu-operator: clusterpolicies, nvidiadrivers
  resource.nvidia.com    # nvidia-dra-driver-gpu
  skyhook.nvidia.com     # nodewright customizations
  nvsentinel.nvidia.com  # nvsentinel
  cert-manager.io        # cert-manager
  acme.cert-manager.io   # cert-manager
  monitoring.coreos.com  # kube-prometheus-stack, prometheus-operator-crds
  nfd.k8s-sigs.io        # node-feature-discovery
  trainer.kubeflow.org   # kubeflow-trainer
  kubeflow.org           # kubeflow-trainer
)

# ---------------------------------------------------------------------------
hdr "R1  namespaces the recipe uses"
# Reset deletes no namespaces by design (internal/teardown, and Mark's ruling
# that uninstall is best-effort about COMPLETENESS but never destructive).
# This is therefore expected residue, not a defect -- but it is the container
# for everything below, so it is inventoried first. A namespace listed here
# after a Reset is one the NEXT run's ownership snapshot will record as
# `existed: true`, which is precisely what the cycle-2 record from
# 2026-08-26 shows for all ten.
RECIPE_NS=(cert-manager gpu-operator kai-scheduler kubeflow monitoring
           node-feature-discovery nvidia-dra-driver nvsentinel skyhook
           aicrme-prove kai-resource-reservation)
for ns in "${RECIPE_NS[@]}"; do
  phase="$("${KC[@]}" get ns "${ns}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  if [[ -z "${phase}" ]]; then
    echo "  absent       ${ns}"
  else
    objs="$("${KC[@]}" -n "${ns}" get all -o name 2>/dev/null | wc -l | tr -d ' ')"
    echo "  ${phase}$(printf '%*s' $((13 - ${#phase})) '')${ns}  (${objs} core objects)"
  fi
done

# ---------------------------------------------------------------------------
hdr "R2  helm releases"
# The one thing Reset genuinely removes. A non-empty list after a Reset is a
# teardown defect; an empty list is Reset working, and says nothing at all
# about everything below it -- which is the entire point of this script.
if ! command -v helm >/dev/null 2>&1; then
  missing "no helm on PATH, so releases cannot be listed"
else
  REL="$(helm --kube-context "${CONTEXT}" list --all-namespaces -o json 2>/dev/null || echo '[]')"
  COUNT="$(echo "${REL}" | jq -r 'length')"
  echo "  releases: ${COUNT}"
  echo "${REL}" | jq -r '.[] | "    \(.namespace)/\(.name)  \(.chart)  \(.status)"'
fi

# ---------------------------------------------------------------------------
hdr "R3  CRDs from the recipe's API groups, and how many instances survive"
# THE CENTRAL MEASUREMENT. helm install creates CRDs from a chart's crds/
# directory; helm uninstall does not remove them, and never has. So these
# survive a Reset by construction -- and so does every instance inside them,
# because deleting a release deletes the controller, not the objects the
# controller managed.
#
# An instance count above zero here is residue the next install's controller
# inherits: objects written by a previous generation of that controller, whose
# finalizers name a webhook that no longer exists, and whose resourceVersions
# belong to a CRD registration that has been through a delete/recreate cycle.
ANY_INSTANCE=0
CRD_JSON="$("${KC[@]}" get crd -o json 2>/dev/null || echo '{"items":[]}')"
for grp in "${RECIPE_GROUPS[@]}"; do
  mapfile -t crds < <(echo "${CRD_JSON}" | jq -r --arg g "${grp}" '.items[] | select(.spec.group == $g) | .metadata.name')
  [[ ${#crds[@]} -eq 0 ]] && continue
  echo "  group ${grp}:"
  for crd in "${crds[@]}"; do
    plural="$(echo "${CRD_JSON}" | jq -r --arg n "${crd}" '.items[] | select(.metadata.name == $n) | .spec.names.plural')"
    scope="$(echo "${CRD_JSON}" | jq -r --arg n "${crd}" '.items[] | select(.metadata.name == $n) | .spec.scope')"
    stored="$(echo "${CRD_JSON}" | jq -r --arg n "${crd}" '.items[] | select(.metadata.name == $n) | (.status.storedVersions // []) | join(",")')"
    n="$("${KC[@]}" get "${plural}.${grp}" --all-namespaces -o name 2>/dev/null | wc -l | tr -d ' ')"
    [[ "${n}" -gt 0 ]] && ANY_INSTANCE=1
    flag=""
    # More than one stored version means the CRD has been re-registered by a
    # different chart version without a storage migration. A controller that
    # writes one version while the API server still stores another is a
    # documented way to get read-back failures that look exactly like the
    # pod-grouper's "already exists, then conflict" loop.
    [[ "${stored}" == *,* ]] && flag="  ⇒ MULTIPLE STORED VERSIONS (${stored})"
    printf '    %5s instances  %-46s %-10s stored=%s%s\n' "${n}" "${crd}" "${scope}" "${stored}" "${flag}"
  done
done
if [[ "${ANY_INSTANCE}" == "0" ]]; then
  echo "  no surviving instances in any recipe group"
else
  echo "  ⇒ instances above survive the next install and are inherited by a freshly"
  echo "    installed controller that did not create them."
fi

# ---------------------------------------------------------------------------
hdr "R4  kai-scheduler in detail — the suspect"
# Named separately from R3 because this is the component whose second-cycle
# behaviour is the actual failure, and a count is not enough to reason about
# it. The pod-grouper's reported error names a PodGroup by name, so the names
# are what is needed.
for kind in podgroups.scheduling.run.ai queues.scheduling.run.ai \
            bindrequests.scheduling.run.ai schedulingshards.kai.scheduler; do
  if ! "${KC[@]}" get "${kind}" --all-namespaces >/dev/null 2>&1; then
    echo "  ${kind}: CRD not registered on this cluster"
    continue
  fi
  echo "  ${kind}:"
  OUT="$("${KC[@]}" get "${kind}" --all-namespaces -o json 2>/dev/null || echo '{"items":[]}')"
  if [[ "$(echo "${OUT}" | jq -r '.items | length')" == "0" ]]; then
    echo "    (none)"
    continue
  fi
  # deletionTimestamp with finalizers still attached is an object nothing can
  # remove, because the controller whose finalizer it names was uninstalled.
  # That state survives every subsequent Reset too -- it cannot be cleaned by
  # uninstalling anything.
  echo "${OUT}" | jq -r '.items[] |
    "    \(.metadata.namespace // "-")/\(.metadata.name)"
    + "  created=\(.metadata.creationTimestamp)"
    + (if ((.metadata.finalizers // []) | length) > 0 then "  finalizers=\(.metadata.finalizers | join(","))" else "" end)
    + (if .metadata.deletionTimestamp then "  ⇒ TERMINATING since \(.metadata.deletionTimestamp) AND UNDELETABLE" else "" end)
    + (if .status then "  status=\(.status | tojson | .[0:120])" else "  (no status written)" end)'
done

# ---------------------------------------------------------------------------
hdr "R5  admission webhooks that outlive their service"
# A ValidatingWebhookConfiguration is cluster-scoped. helm owns the ones it
# creates and does remove them on uninstall -- but a webhook whose removal
# failed, or one a controller registered dynamically rather than via the
# chart, stays and points at a Service that no longer exists. Every create of
# a matching object then fails closed, which for kai's podgroup webhook would
# present as a PodGroup that cannot be written.
for wk in validatingwebhookconfigurations mutatingwebhookconfigurations; do
  echo "  ${wk}:"
  W="$("${KC[@]}" get "${wk}" -o json 2>/dev/null || echo '{"items":[]}')"
  FOUND="$(echo "${W}" | jq -r '.items[] |
    select([.webhooks[]?.clientConfig.service.namespace] |
           any(. == "kai-scheduler" or . == "gpu-operator" or . == "cert-manager"
               or . == "nvidia-dra-driver" or . == "monitoring" or . == "nvsentinel")) |
    "    \(.metadata.name)  →  " +
    ([.webhooks[]?.clientConfig.service | "\(.namespace)/\(.name)"] | unique | join(", ")) +
    "  failurePolicy=" + ([.webhooks[]?.failurePolicy // "Fail"] | unique | join(","))')"
  if [[ -z "${FOUND}" ]]; then
    echo "    (none pointing at a recipe namespace)"
  else
    echo "${FOUND}"
    # A webhook whose backing Service is gone fails closed on every matching
    # create. This is the check that distinguishes "left behind harmlessly"
    # from "left behind and now blocking".
    while IFS= read -r line; do
      svc="$(echo "${line}" | sed -n 's/.*→  \([^ ,]*\).*/\1/p')"
      [[ -z "${svc}" ]] && continue
      sns="${svc%%/*}"; sn="${svc##*/}"
      if ! "${KC[@]}" -n "${sns}" get svc "${sn}" >/dev/null 2>&1; then
        echo "      ⇒ BACKING SERVICE ${svc} DOES NOT EXIST — this webhook fails closed"
      fi
    done <<<"${FOUND}"
  fi
done

# ---------------------------------------------------------------------------
hdr "R6  anything stuck Terminating cluster-wide"
# The residue that no Reset can clear, because the finalizer names a
# controller that is gone. Worth separating from ordinary residue: ordinary
# residue is removed by deleting it, this is not.
STUCK="$("${KC[@]}" get ns -o json 2>/dev/null \
  | jq -r '.items[] | select(.status.phase == "Terminating") | "    namespace/\(.metadata.name)"')"
if [[ -z "${STUCK}" ]]; then
  echo "  no namespace is stuck Terminating"
else
  echo "${STUCK}"
  echo "  ⇒ a namespace in Terminating blocks recreation of anything inside it"
fi

# ---------------------------------------------------------------------------
hdr "R7  the pod-grouper itself"
# If kai-scheduler is currently installed, its pod-grouper log is where the
# failure actually speaks. Absent on a freshly-reset cluster, which is a
# correct and expected result at that moment rather than a broken probe.
if ! "${KC[@]}" get ns kai-scheduler >/dev/null 2>&1; then
  echo "  kai-scheduler namespace absent — nothing to read (expected right after a Reset)"
else
  "${KC[@]}" -n kai-scheduler get pods -o wide 2>/dev/null || missing "no pods in kai-scheduler"
  POD="$("${KC[@]}" -n kai-scheduler get pods -o json 2>/dev/null \
    | jq -r '.items[] | select([.spec.containers[].name] | any(test("grouper"))) | .metadata.name' | head -1)"
  if [[ -z "${POD}" ]]; then
    echo "  no pod-grouper pod found"
  else
    echo "  --- ${POD} log, lines mentioning podgroup/conflict/exists ---"
    "${KC[@]}" -n kai-scheduler logs "${POD}" --tail=2000 2>/dev/null \
      | grep -iE 'podgroup|already exists|conflict|object has been modified' \
      | tail -40 || echo "    (no matching line)"
  fi
fi

echo
echo "════════ end of inventory (${MOMENT}) ════════"
