# Phase 3 (Prove) — all 11 tasks complete, not yet merged

**Branch:** `phase-3-prove`, pushed to origin. Forked from `main` at `ab84834`.
**State:** `make qualify` green, aggregate coverage 90.1%. `test/e2e/prove.sh` verified end to
end on a real cluster (13 minutes, all six assertions).
**Tasks:** 11 of 11 complete.

`main` is green and unaffected — this branch has never been merged.

This file exists because the SDD ledger (`.superpowers/sdd/2026-08-19-aicrme-phase-3-prove/progress.md`)
is **git-ignored and lives only on one machine**. What follows is the subset a future session
cannot reconstruct from `git log`.

---

## Where to pick up

Every task is done. What remains is the close-out:

1. **Whole-branch review** — the full diff against `main`, in one pass.
2. **One fix wave** from that review.
3. **Merge to `main`** (locally, push straight up, no PR) and **watch e2e**, which now carries a
   new `prove` job alongside `apply-real`.

## What the arc does now

A run reaches `StateActive` with a 2-pod gang placed by kai-scheduler on simulated GPU nodes,
and **Stop is its only exit** — Discard is refused while a workload is running, Retry needs a
failed run. A restart recovers the record and reconciles it against the cluster; a workload
whose record was lost is *adopted*, never deleted. `make demo` runs the whole thing.

## Rulings that bind future work

Numbered as in the ledger. Rulings not listed here were local to a completed task.

**Ruling 2 — provisional answers to the spec's open questions, now partly settled by
measurement.** `GangTimeout` = 3 minutes: kept, and now justified rather than guessed — both
gang members were bound within two seconds on the demo cluster, so the default is generous by
two orders of magnitude, which is the right direction for real hardware. The Prove screen
*replaces* the cockpit. Orphan adoption is silent (no confirmation click) — and it is silent
only in the sense of not asking: it logs, publishes to the stream, and the screen says the
workload was already running when the console started.

**Ruling 12 + 15 — the `Start`-blocking guard, and the dead end it caused.** Spec §8 row 3
("keep `Start` blocked if cleanup cannot complete") was written into the spec and never assigned
to any task — a plan defect. Blocking `Start` *and* `Discard` produced a state where every
operation 409'd and only the unsafe `Retry` remained. **`Stop` is the remedy** and works on an
unconfirmed-cleanup run. Do not re-block it. `test/e2e/prove.sh`'s assertion 6 now pins the live
shape of this: a Stop whose deletion is blocked leaves the run active and `POST /api/runs` at 409.

**Ruling 20 — `Run`↔`envelope` parity is enforced by test.** `envelope.go` is a hand-maintained
projection; before this, dropping `Pending` and `Err` from `encodeRun` left the entire module
green. `TestEnvelopeRoundTripsEveryRunField` closes that. Its exclusion map is **empty**. A
future deliberate exclusion must be stated there, not silent.

**New: only an operator action clears the recovered-run gate.** `ReconcileWorkloads` changes run
state per spec §3 but never touches `recoveredPending` — Retry, Discard and a successful Stop
are the only things that do. A run it finishes at `StateDone` (workload gone) therefore still
waits to be acknowledged rather than vanishing under the next page load.

## What the e2e found that the unit suite could not

Four defects, every one green in `go test ./...` and broken on a cluster. Recorded because the
shape is the lesson, not the individual bugs — each is a property of a live API server (taints,
client-side rate limiting, deadline propagation, controller timing) and a fake clientset has
none of them:

1. The workload tolerated **neither taint its own GPU nodes carry**, so kai-scheduler answered
   *"no nodes with enough resources were found: 4 node(s) had untolerated taint(s)"*. Prove
   could not have succeeded on any cluster this console can demo on.
2. Placement was polled every **20ms across a three-minute budget** — ~9000 List calls, which
   client-go's own rate limiter refused long before the deadline.
3. That refusal became the run's **entire recorded error**, hiding the 0/2 placement.
4. `PlacedNodes` excluded `Succeeded` pods, and **KWOK completes a pod in the second it binds
   it**, so every simulated run timed out over a gang that had been placed immediately.

`apply-real.sh` was also broken by Task 5 and nobody had noticed: it waited for `done`, which a
run ending at `active` never reaches. Fixed in the same commit. **e2e only runs on push to
`main` and on PRs, and this branch has had neither** — so nothing on it had ever been exercised
in CI until the local run.

## Deferred minors, for the whole-branch review to triage

- `prove.runIDLabelKey` is a literal independent of `Labels()`' key. Drift would empty
  `ListOwned`'s `RunID` rather than error. Reconciliation is now defended against it
  (`matchesRun` accepts either the label or the derived name) but the two literals still are not
  single-sourced.
- `labelBlock` hand-builds YAML with no value escaping — safe only while label values stay
  colon-free.
- Both `slog` calls in `Stop` sit inside `e.mu`, against the rule `Discard`'s own comment states.
  (`reconcile.go` deliberately keeps every log and publish outside the lock.)
- The recovery marker text — *"recovered a previous run; retry or discard it"* — is published for
  **every** recovered state including `StateActive`, where both named remedies 409. The Prove
  screen says the true thing next to it (`prove-recovered`), but the marker itself still misleads
  in the timeline rail.
- `adopt`'s `StartedAt` is when the console adopted the workload, not when the workload started:
  `ListOwned` does not return the object's creation timestamp.

## Observations worth keeping, not defects

**Re-applying the recipe onto a cluster where kai-scheduler is already running can wedge the
running scheduler.** A second Apply rolled `admission`/`binder`/`pod-grouper`/
`podgroup-controller`/`queue-controller` while the older `kai-scheduler-default` pod kept
running; that pod then logged `Failed to update pod group status ...: Unauthorized` at ~50/second
and nothing scheduled until it was restarted. Mechanism not chased. Harmless in CI (one install
per fresh cluster); the remedy on a live `make demo` cluster is
`kubectl -n kai-scheduler rollout restart deploy/kai-scheduler-default`.

## What this phase cost, honestly

Six defects reached review that would have shipped, and four more reached a real cluster with
the whole unit suite green (above). **Five of the eleven dispatched briefs contained technically
wrong code** — non-compiling test snippets, wrong signatures, a `strings.NewReplacer` prefix bug,
a helper that did not exist, and a test stub that could not have exercised its own bite-proof.
Verifying symbols against source before dispatch (started at Task 4) reduced but did not
eliminate this.

The single highest-value hour of the phase was standing up `make demo` and driving one run by
hand. It refuted a shipped assumption within two minutes.

## Other repo state

- `main` is green. The `apt-get` CI hang was fixed on 2026-08-20 by deleting the `apt` step
  entirely and giving `qualify` a 30-minute timeout.
- **Local toolchain drift to watch:** `golangci-lint` v2.12.2 (the `.settings.yaml` pin) cannot
  typecheck a Go 1.27 stdlib, and Homebrew has moved local Go past `.go-version`'s 1.26.5. Run
  the gate as `GOTOOLCHAIN=go1.26.5 make qualify` until the pins move together, or lint fails on
  `math/rand/v2` with nothing to do with this repo.
- UX feedback from the first human demo run is in `docs/ux-feedback.md`, including one
  correctness bug (a stale, already-converged warning pinned to a healthy row on the success
  screen). Unassigned to any phase.
