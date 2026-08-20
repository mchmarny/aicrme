# Phase 3 (Prove) — in progress, paused 2026-08-20

**Branch:** `phase-3-prove`, pushed to origin. Forked from `main` at `ab84834`.
**State:** 15 commits, `make qualify` green, aggregate coverage 90.2%.
**Tasks:** 8 of 11 complete.

`main` is green and unaffected — this branch has never been merged.

This file exists because the SDD ledger (`.superpowers/sdd/2026-08-19-aicrme-phase-3-prove/progress.md`)
is **git-ignored and lives only on one machine**. It holds 21 numbered rulings with their reasoning
and cost-if-wrong. What follows is the subset a future session cannot reconstruct from `git log`.

---

## Where to pick up

| Task | State |
|---|---|
| 1 `ActiveStep` / `StateActive` | complete |
| 2 `Start` + `Discard` guards | complete |
| 3 workload manifest + labels | complete (1 fix round) |
| 4 cluster client | complete (2 fix rounds) |
| 6 `Run.Workload` | complete (1 fix round) |
| 5 the Prove step | complete (1 fix round) |
| 7 Stop | complete (3 fix rounds) |
| **8 startup reconciliation** | **next — full review depth** |
| 9 Prove screen (`web/`) | not started — dispatch in parallel with 10 |
| 10 e2e (`test/e2e/`) | not started — dispatch in parallel with 9 |
| 11 docs | not started — write directly, no dispatch |

Then: whole-branch review → one fix wave → merge to `main` → watch e2e.

**Execution order was deliberately changed:** 6 runs before 5. Task 5 writes `run.Workload`, which
Task 6 defines; in plan order Task 5's implementer would have invented a parallel field. Remaining
order is unchanged.

## Rulings that bind future work

Numbered as in the ledger. Rulings not listed here were local to a completed task.

**Ruling 2 — provisional answers to the spec's open questions.** `GangTimeout` = 3 minutes; the Prove
screen *replaces* the cockpit; orphan adoption is silent (no confirmation click). Each is small and
localized to reverse. Task 9 inherits the second; Task 8 inherits the third.

**Ruling 12 + 15 — the `Start`-blocking guard, and the dead end it caused.** Spec §8 row 3 ("keep
`Start` blocked if cleanup cannot complete") was written into the spec and never assigned to any task —
a plan defect. It landed in Task 7. Blocking `Start` *and* `Discard` then produced a state where every
operation 409'd and only the unsafe `Retry` remained. **`Stop` is now the remedy** and works on an
unconfirmed-cleanup run. Do not re-block it.

**Ruling 20 — `Run`↔`envelope` parity is now enforced by test.** `envelope.go` is a hand-maintained
projection; before this, dropping `Pending` and `Err` from `encodeRun` left the entire module green.
`TestEnvelopeRoundTripsEveryRunField` closes that. Its exclusion map is **empty** — every exported
`Run` field is carried. A future deliberate exclusion must be stated there, not silent.

**Ruling 14 — deliberate process changes for the rest of the phase.** Tasks 9 and 10 dispatch in
parallel (disjoint `web/` vs `test/e2e/` trees, with stay-in-your-lane constraints, as 2b-iii proved
safe). Task 11 is written directly rather than dispatched. **Tasks 8 keeps full review depth** — Stop
and reconciliation are where a quiet defect leaves an operator with a cluster they cannot clean up.

## Open items this branch must not merge without

- **Task 9 must give `StateActive` a UI exit.** The engine now has `Stop` (`POST /api/runs/{id}/stop`)
  but no SPA affordance — it is curl-only today. A recovered `StateActive` run must offer **Stop**, and
  must **not** offer Discard, which the engine now rejects. Without this the operator is stranded.
- **`NEW-7`, an orphaned doc comment**, could not be located by the implementer after a systematic
  search of five files. Reported rather than guessed at. The whole-branch review reads the entire diff
  and should catch it if it exists.
- **The design's three recorded test gaps** (no workload executes on KWOK; the workload body is
  unexercised; DRA is entirely unexercised) belong next to the code as well as in the handoff — that is
  Task 11's job and is not done.

## Deferred minors, for the whole-branch review to triage

Roughly a dozen, all in the ledger. The ones with teeth:

- `runIDLabelKey` is a literal independent of `Labels()`' key; drift would silently empty `ListOwned`'s
  `RunID` rather than error.
- `PlacedNodes` correctly excludes `PodUnknown` but nothing pins it.
- `labelBlock` hand-builds YAML with no value escaping — safe only while label values stay colon-free.
- Both new `slog` calls in `Stop` sit inside `e.mu`, against the rule `Discard`'s own comment states.
- `Stop` leaves `cleanupUnconfirmed: true` on a `StateDone` record.

## What this phase cost, honestly

Six defects reached review that would have shipped: a workload whose entire gang-scheduling shape was
unpinned; a `go.mod` constraint violation; an `omitempty` no-op that made `if (run.workload)` truthy for
every run; a process-killing nil panic reachable on the laptop config `make demo` uses; a run landing
`StateActive` over a dead Job; and a restart that silently cleared the orphan guard.

**Five of the eleven dispatched briefs contained technically wrong code** — non-compiling test
snippets, wrong signatures, a `strings.NewReplacer` prefix bug, a helper that did not exist, and a test
stub that could not have exercised its own bite-proof. Two were caught by the tests those same briefs
specified; three cost an implementer turn. Verifying symbols against source before dispatch (started at
Task 4) reduced but did not eliminate this.

## Other repo state

- `main` is green. The `apt-get` CI hang was fixed on 2026-08-20 by deleting the `apt` step entirely
  (`bc` had one use; `awk` replaced it) and giving `qualify` a 30-minute timeout — it had none, so a
  hang cost two full 6-hour runs.
- `make demo` works and is documented in `DEMO.md`.
- UX feedback from the first human demo run is captured in `docs/ux-feedback.md`, including one
  correctness bug (a stale, already-converged warning pinned to a healthy row on the success screen).
