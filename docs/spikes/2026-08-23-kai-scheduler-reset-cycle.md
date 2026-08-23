# Why the gang does not place after a Reset — and what fixes it

**Date:** 2026-08-23. **Cluster:** Kind + KWOK, 4 nodes, 10 simulated GPU nodes.
**Question:** Phase 5's unresolved question 3 — a run installed on a cluster this console had
already Reset installs all 14 components and then dies in Prove. Why, and is the remedy bounded?

**Answer:** kai-scheduler is left internally inconsistent by the teardown-then-reinstall cycle.
Its pod-grouper cannot read back the PodGroup it just created, the scheduler therefore sees
zero PodGroups, and the gang stays `Pending` until Prove times out. **Refreshing kai-scheduler's
control plane fixes it immediately, without deleting anything cluster-scoped.**

## What was measured

| Arm | What it did | Result |
|---|---|---|
| cycle 1 | install on a clean cluster | `active` — gang placed in ~2s |
| reset | teardown | `done`, clean |
| cycle 2 | install on the reset cluster | **`failed`** — all 14 installed, gang never placed |
| A (control) | retry Prove, change nothing | `failed` — so it is not settling time |
| B | delete `SchedulingShard/default`, retry | `failed`, **void as evidence** — nothing rebuilt the shard, so this arm ran with no scheduler at all (`deployments.apps "kai-scheduler-default" not found`) |
| C | recreate the shard, `rollout restart` every kai-scheduler Deployment, retry | **`active`** — gang placed within ~10s |

## The mechanism

During cycle 2's Prove, with both gang pods `Pending` and unassigned for three minutes:

```
pod-grouper: Failed to apply metadata for pod group
  error: podgroups.scheduling.run.ai "pg-prove-...-32219eca-..." not found

kai-scheduler-default: There are <0> PodGroupInfos and <1> Queues in total for scheduling
```

The PodGroup **did** exist — `kubectl get podgroups -A` showed it, three minutes old, by exactly
the name the error names. There is only one PodGroup CRD (`scheduling.run.ai/v2alpha2`). So a
freshly-started pod-grouper could not read an object that was present.

The object ages explain why. After the Reset, every kai-scheduler Deployment still carried cycle
1's timestamp — `helm uninstall` removes the release record, and these workloads are materialized
by the operator from CRs rather than templated by the chart. Cycle 2's install then recreated
most of them (`11:08`), but **not** `kai-scheduler-default` (`11:01`): its owning
`SchedulingShard` had survived, so the operator saw its desired state already satisfied. The CRDs
also survived, from `11:00:50`. Cycle 2 therefore ran cycle-2 controllers against a cycle-1
scheduler and cycle-1 CRDs.

An earlier reading of this — that the pods never restarted at all — was wrong, and the
timestamps above are what refuted it.

## Why this is not a Reset defect

Reset behaved exactly as specified. Everything it kept, it named. The survivors are
runtime-created objects (`SchedulingShard`, the Deployment it owns) and cluster-scoped CRDs,
and the design deliberately removes neither: CRDs are shared and removing them takes every
custom resource on the cluster, and widening the ownership rule is the one change the spec
singles out as how that rule becomes unsafe.

## The remedy, and why it is safe

Arm C recreated `SchedulingShard/default` and restarted all six kai-scheduler Deployments. The
gang placed on the next poll. **This is a restart, not a deletion** — no CRD is removed, no
ownership claim is widened, nothing outside the `kai-scheduler` namespace is touched. That is
what makes it a candidate for automation where the CRD-deleting alternative is not.

Arm C did not isolate the *minimal* remedy: it changed the shard and the controllers together.
Two candidates, both cheap to test, neither tested here:

- **Delete `SchedulingShard/default` during teardown**, so the next install recreates it and its
  scheduler cleanly. Narrow, and it targets the one object shown to be stale.
- **Restart kai-scheduler's controllers after an install that found the namespace pre-existing.**
  Broader, but it does not require Reset to know anything about kai-scheduler's CRs.

## Limits of this result

- **n=1, on KWOK.** Whether it reproduces on real hardware is untested. KWOK completes a pod the
  instant it binds, so "placed" here means bound, not run.
- Arm C hand-wrote the shard as `spec: {}`. The chart's own shard may differ, and a remedy built
  on recreating it should let helm do so rather than synthesizing one.
- The exact reason a fresh pod-grouper reads `NotFound` on a present object is still unexplained.
  It is consistent with a stale cache or a conversion mismatch against the surviving CRD, but
  neither was confirmed, and that is kai-scheduler's internals rather than this console's.

## Prior art this spike missed, and what it adds

`docs/phase-3-status.md` recorded the same family of failure during Phase 3, under
"Observations worth keeping": re-applying the recipe onto a cluster where kai-scheduler was
already running left the older `kai-scheduler-default` pod alive while the chart-managed
controllers rolled, and that pod then logged
`Failed to update pod group status ...: Unauthorized` at ~50/second — **and nothing scheduled
until it was restarted.** The mechanism was not chased at the time.

That is a sharper clue than anything measured here, and it points somewhere this spike did not
look: **authorization**, not caching. A pod that outlives the reinstall of its own
ServiceAccount and RBAC keeps a token the new install no longer honours. It also independently
corroborates Arm C — "nothing scheduled until it was restarted" is exactly what Arm C measured,
two months apart, on a different path into the same state.

Stated carefully, because the two observations are not identical: in Phase 3 the *scheduler* was
stale and unauthorized; here the *pod-grouper* was freshly created and still read `NotFound`.
The common thread is a kai-scheduler control plane whose parts were replaced at different times,
not one specific stale component. Anyone chasing this further should start from the
`Unauthorized` line, not from the cache theory.

## Reproducing

`/tmp/claude/kai-spike.sh` (arms 1–B) and `/tmp/claude/kai-armc.sh` (arm C) were throwaway
investigation scripts, deliberately not committed: they hard-code a run ID and keep the cluster
alive for inspection. The measurements above are the artifact worth keeping.
