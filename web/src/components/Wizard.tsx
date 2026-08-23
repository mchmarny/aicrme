import { useEffect, useMemo, useState } from 'react'
import type { AicrEvent } from '../useEvents'
import { ApiError, decide as decideApi, discardRun, fetchOptions, resetRun, retryRun, stopRun, type Options } from '../api'
import { deriveComponents } from '../pipeline'
import { Cockpit } from './Cockpit'
import { Discover, type CapabilityReport } from './Discover'
import { Prove } from './Prove'
import { Reset, ResetGate } from './Reset'
import { Recommend, type RecipeSummary } from './Recommend'
import { Timeline } from './Timeline'

/**
 * RunState is derived entirely from the AicrEvent stream Wizard already
 * consumes, not fetched separately from GET /api/runs/{id}. The engine
 * (internal/engine/engine.go) publishes exactly one bus event per state
 * transition -- "phase started"/"phase complete" on runStep, a KindDecision
 * event on awaitDecisions, and a "run done"/"run failed" KindPhase on finish
 * -- so replaying that sequence client-side reproduces engine.Run.State
 * without a second endpoint. This is also why Wizard's own capability
 * report and recipe summary come off event Data rather than a new artifacts
 * route: steps.Discover and steps.Recommend already publish gap.Report and
 * steps.RecipeSummary as the Data field of a KindLog event (see
 * internal/steps/discover.go and internal/steps/recommend.go), and the
 * recorded fixture in src/fixtures/kwok-run.json confirms those payloads
 * carry every field both screens need. Reading the typed stream already
 * flowing through useEvents needs no new HTTP surface; a
 * GET /api/runs/{id}/artifacts/... endpoint was the alternative and was not
 * necessary.
 */
type RunPhase = 'idle' | 'running' | 'awaiting_decision' | 'failed' | 'active' | 'done' | 'resetting'

export interface RunState {
  runId?: string
  phase?: string
  state: RunPhase
  report: CapabilityReport | null
  recipe: RecipeSummary | null
  error?: string
  /**
   * True while this run is one restart recovery installed and the engine is
   * refusing new runs until an operator retries or discards it
   * (internal/engine's recoveredPending gate). Nothing else in the stream
   * carries it: a recovered `done` run publishes exactly what a run that
   * just finished normally publishes, and a recovered `failed` run publishes
   * exactly what an ordinary failure publishes. Optional so existing
   * constructors of this type (Cockpit's tests) stay valid; undefined reads
   * as false everywhere.
   */
  recovered?: boolean
  /**
   * Artifacts the store dropped from this run's checkpoint to fit its size
   * limit (internal/engine/envelope.go's encodeRun), carried on the recovery
   * marker's Data. Non-empty means Retry cannot work: recovery rewinds to
   * Bundle, and internal/steps/bundle.go reads snapshot.yaml, which is the
   * first artifact shed. The record is honest about the loss; this is what
   * lets the console be.
   */
  truncated?: string[]
  /**
   * The last Reset's outcome for this run, from engine.ResetSummaryData on
   * the terminal teardown log event (internal/engine/reset.go). Undefined
   * until a Reset has finished.
   *
   * `incomplete` is what the console keys every action off after a failed
   * teardown: the engine refuses Start, Retry and Discard while it holds
   * (hasIncompleteTeardown), so offering any of them would be three buttons
   * that all answer 409. It also arrives on the recovery marker, because
   * after a restart that marker is the only event in the stream carrying it.
   */
  residue?: ResidueSummary
}

/** ResidueItem mirrors Go's engine.ResidueItem (internal/engine/run.go). */
export interface ResidueItem {
  kind: 'release' | 'namespace'
  name: string
  namespace?: string
  removed?: boolean
  skip?: string
  error?: string
}

/** ResidueSummary mirrors Go's engine.ResetSummaryData. */
export interface ResidueSummary {
  incomplete: boolean
  summary: string
  items?: ResidueItem[]
}

function isResidueSummary(data: unknown): data is ResidueSummary {
  return typeof data === 'object' && data !== null && 'incomplete' in data && 'summary' in data
}

/**
 * RECOVERY_INTERRUPTED_ERROR is internal/engine/recover.go's recoveredErr
 * verbatim. Recover sets it only on a run it found live or idle -- i.e. one
 * the restart actually cut off -- so a recovered run whose error is anything
 * else had already failed on its own before the pod went away. Those are
 * different things to tell an operator deciding whether Retry is safe, and
 * this constant is what keeps them apart. A string match, like the "run
 * done"/"run failed"/"run retrying" messages deriveRunState already keys on;
 * internal/engine/recover_test.go pins the producing side exactly.
 */
export const RECOVERY_INTERRUPTED_ERROR = 'interrupted by a console restart'

/**
 * RESIDUE_RECOVERED_SUMMARY stands in for the summary a completed Reset
 * would have published. A teardown interrupted by a restart published no
 * summary -- the goroutine that would have died with the pod -- so the
 * console says what it actually knows rather than inventing counts.
 */
export const RESIDUE_RECOVERED_SUMMARY =
  'a previous reset did not finish; what it removed was never recorded'

function isCapabilityReport(data: unknown): data is CapabilityReport {
  return typeof data === 'object' && data !== null && 'headline' in data && 'gaps' in data
}

function isRecipeSummary(data: unknown): data is RecipeSummary {
  return typeof data === 'object' && data !== null && 'componentCount' in data && 'components' in data
}

/**
 * isRecoveryMarkerData matches internal/engine/recover.go's
 * recoveryMarkerData. The field is omitted entirely for an intact record, so
 * its presence is the signal -- an empty array would be indistinguishable
 * from "not carried".
 */
function isRecoveryMarkerData(data: unknown): data is { truncated?: string[]; residueIncomplete?: boolean } {
  if (typeof data !== 'object' || data === null) return false
  return ('truncated' in data && Array.isArray((data as { truncated: unknown }).truncated))
    || 'residueIncomplete' in data
}

/**
 * currentRunIdOf returns the run id the console is currently showing: the
 * runId on the most recent event that carries one. The bus's replay ring is
 * global across every run this process has started (cmd/aicrme/main.go's
 * replayCapacity is one Bus for the whole server, not one per run), and
 * App.tsx starts a fresh run on mount whenever the previous one is no
 * longer live -- so a page reload after a run completed and a new one began
 * replays BOTH runs' events into useEvents' single accumulated list. Bus ids
 * are assigned by one monotonically increasing counter shared across every
 * run (internal/bus/bus.go's Publish), so the run identified by the last
 * event's runId is always the newest one, regardless of how many earlier
 * runs' events precede it in the array.
 */
function currentRunIdOf(events: AicrEvent[]): string | undefined {
  for (let i = events.length - 1; i >= 0; i--) {
    if (events[i].runId) return events[i].runId
  }
  return undefined
}

/**
 * deriveRunState replays the CURRENT run's events (see currentRunIdOf) in
 * arrival order into that run's phase, state, and any artifacts it has
 * published so far. Events belonging to an earlier run are excluded, so a
 * reload after one run finished and another began never shows the old
 * run's capability report or recipe attributed to the new one. Exported for
 * testing against the recorded fixture.
 */
export function deriveRunState(events: AicrEvent[]): RunState {
  const runId = currentRunIdOf(events)
  const relevant = runId ? events.filter(e => e.runId === runId) : events
  const out: RunState = { runId, state: 'idle', report: null, recipe: null }

  for (const e of relevant) {
    if (e.phase) out.phase = e.phase

    switch (e.kind) {
      case 'phase':
        if (e.message === 'run done') out.state = 'done'
        else if (e.message === 'run failed') out.state = 'failed'
        // engine.finish publishes "run " + state for every state it reaches,
        // and StateActive is terminal-but-active: every step finished and the
        // Prove workload is deliberately still running. Without this branch
        // it fell through to 'running' below, which is what left an active
        // run rendering an ordinary phase body with no Stop control and --
        // for a recovered one -- a Discard button the engine now rejects.
        else if (e.message === 'run active') out.state = 'active'
        // engine.Reset publishes "run resetting" for exactly this branch --
        // see its own comment. Without it a teardown falls through to
        // 'running' below and renders as an install in progress, with the
        // actions that go with one.
        else if (e.message === 'run resetting') out.state = 'resetting'
        else out.state = 'running'
        // engine.Retry (internal/engine/engine.go) publishes this exact
        // message before relaunching execute for the same run ID. Replaying
        // it here is the signal that a fresh attempt has started, so the
        // previous attempt's error must not keep rendering above a
        // subsequently successful run -- out.error is otherwise sticky
        // because deriving replays every event this run has ever emitted,
        // including a 'kind === error' from an attempt that later succeeded.
        //
        // It is also the moment the recovery gate closes server-side:
        // engine.Retry clears recoveredPending as its first act, so the
        // console must stop offering retry/discard for this run too.
        if (e.message === 'run retrying') {
          out.error = undefined
          out.recovered = false
        }
        break
      case 'decision':
        out.state = 'awaiting_decision'
        break
      case 'error':
        out.state = 'failed'
        out.error = e.message
        break
      // Deliberately does not touch out.state: recovery publishes this ahead
      // of the "run <state>" event that carries the state, so a recovered run
      // resolves through exactly the same branches a live one does and only
      // gains the flag.
      case 'recovered':
        out.recovered = true
        if (isRecoveryMarkerData(e.data)) {
          out.truncated = e.data.truncated
          // After a restart this marker is the only event carrying the
          // fact that a teardown was interrupted or had failed. Without it
          // the console offers Start, Retry and Discard on a half-removed
          // cluster and collects three 409s.
          if (e.data.residueIncomplete) {
            out.residue = { incomplete: true, summary: RESIDUE_RECOVERED_SUMMARY }
          }
        }
        break
    }

    if (e.kind === 'log' && isResidueSummary(e.data)) out.residue = e.data
    if (e.kind === 'log' && e.phase === 'discover' && isCapabilityReport(e.data)) out.report = e.data
    if (e.kind === 'log' && e.phase === 'recommend' && isRecipeSummary(e.data)) out.recipe = e.data
  }

  return out
}

// A corrupt snapshot degrades /api/options to a provisional=true 200 rather
// than failing (internal/aicrclient/options.go's AvailableOptions), so a
// provisional answer can persist indefinitely -- retrying forever would hang
// the payoff screen exactly as badly as never retrying at all. Bounded
// exponential backoff mirrors the established pattern in useEvents.ts
// (MAX_GAP_RECONNECT_ATTEMPTS / GAP_RECONNECT_BASE_DELAY_MS) for the same
// reason: give a transient state (still mid-Discover, briefly) room to
// resolve on its own, then stop and show the best answer available with a
// visible caveat rather than silently presenting a widened, possibly
// dead-ending set as final.
export const MAX_PROVISIONAL_OPTIONS_RETRIES = 5
export const PROVISIONAL_OPTIONS_RETRY_BASE_DELAY_MS = 250

function ResolvedRecommend({ recipe }: { recipe: RecipeSummary | null }) {
  if (!recipe) {
    return <p className="text-slate-500 text-sm">Resolving the recipe for the answers you gave…</p>
  }
  return (
    <section className="mx-auto max-w-2xl space-y-4">
      <p className="text-slate-300">
        Decisions submitted — <strong>{recipe.componentCount} components</strong> resolved, every version pinned.
      </p>
      <ul className="space-y-1 font-mono text-xs text-slate-400">
        {recipe.components.map(c => (
          <li key={c.name}>{c.name} {c.version} → {c.namespace}</li>
        ))}
      </ul>
    </section>
  )
}

/**
 * recoverySummary is what the operator is actually told happened. The three
 * cases are genuinely different situations and collapsing them would be a
 * lie in at least one:
 *
 *  - a run the restart cut off mid-flight (engine.Recover flipped it to
 *    failed and set RECOVERY_INTERRUPTED_ERROR),
 *  - a run that had already failed on its own before the pod went away, whose
 *    error is its real one and whose Retry means "try that step again",
 *  - a run that had already finished. Nothing interrupted it and there is
 *    nothing to retry -- but its record outlives the pod, so every
 *    `helm upgrade` of a release that has completed one demo recovers it and
 *    finds Start refusing. Discard is the only way out, and saying
 *    "interrupted" here would send an operator looking for a failure that
 *    never happened.
 */
function recoverySummary(run: RunState): string {
  if (run.state === 'done') {
    return 'It finished before the console restarted, so there is nothing left to run. Discard it to start a new one.'
  }
  if (run.state !== 'failed') {
    return 'It was recovered after a console restart and is holding the console until you decide what to do with it.'
  }
  if (run.error === RECOVERY_INTERRUPTED_ERROR) {
    return `The console restarted while this run was in the ${run.phase ?? 'discover'} phase, so it never finished. Retrying picks it up from the step it stopped on.`
  }
  return `This run had already failed during the ${run.phase ?? 'discover'} phase before the console restarted.`
}

/**
 * Recovered is the console's whole screen for a recovered run, in every
 * phase. It replaces the phase body rather than sitting above it because the
 * phase body is actively misleading here: a run recovered on `recommend`
 * renders "Resolving the recipe for the answers you gave…" forever, since no
 * step is running and none ever will be until the operator acts.
 *
 * It keys off run.state, never run.phase. The affordance used to live inside
 * Cockpit, which Wizard renders only for bundle/apply, so a restart at the
 * Recommend decision gate -- the longest idle window in the product, since it
 * waits on a human -- reached a console with no button anywhere. And Discard
 * is offered in every state, including the terminal ones Retry refuses, which
 * is the only exit from a recovered `done` run.
 */
function Recovered({ events, run, busy, onRetry, onDiscard }: {
  events: AicrEvent[]
  run: RunState
  busy: boolean
  onRetry: () => void
  onDiscard: () => void
}) {
  // Redraws whatever the persisted component projection carried, from the
  // bootstrap KindComponent events Recover replays -- the reason those events
  // exist at all. Empty for a run that never reached Apply.
  const components = deriveComponents(events, run.recipe?.components.map(c => c.name))
  const showOwnError = run.state === 'failed' && run.error && run.error !== RECOVERY_INTERRUPTED_ERROR
  // The record lost artifacts to the size guard, so a retry resumes at a step
  // that will read one of them and fail. Retry is still offered rather than
  // hidden: suppressing it would be a confident claim resting on the current
  // step slice (only Discover produces an artifact large enough to trigger
  // shedding, so every truncated record happens to sit at or past Recommend,
  // which reads it) -- an accidental property, not a structural one, and
  // removing the only forward action on an accident is worse than warning.
  const truncated = run.truncated ?? []

  return (
    <section data-testid="recovered-run" className="mx-auto max-w-2xl space-y-5">
      <div>
        <h2 className="text-2xl font-semibold text-amber-400">A previous run is waiting for you</h2>
        <p className="mt-2 text-sm text-slate-300">{recoverySummary(run)}</p>
      </div>

      {showOwnError && <p className="text-sm text-red-400">{run.error}</p>}

      {components.length > 0 && (
        <ul className="space-y-1 font-mono text-xs text-slate-400">
          {components.map(c => (
            <li key={c.name} data-testid={`recovered-component-${c.name}`}>
              {c.name} <span className="uppercase text-slate-500">{c.status}</span>
            </li>
          ))}
        </ul>
      )}

      {truncated.length > 0 && (
        <p data-testid="recovery-truncated" className="rounded border border-amber-900 bg-amber-950/30 p-3 text-xs text-amber-400">
          This run's checkpoint was too large to store in full, so{' '}
          <span className="font-mono">{truncated.join(', ')}</span>{' '}
          {truncated.length === 1 ? 'was' : 'were'} dropped from it. Retrying will
          almost certainly fail at the first step that needs one of them —
          discarding and starting over is the reliable way forward.
        </p>
      )}

      <p className="text-xs text-slate-500">
        The console will not start a new run until you choose.
      </p>

      <div className="flex items-center gap-4">
        {run.state === 'failed' && (
          <button
            data-testid="recovery-retry"
            disabled={busy}
            onClick={onRetry}
            className="rounded bg-emerald-600 px-4 py-2 text-white disabled:opacity-50"
          >
            Retry this run
          </button>
        )}
        <button
          data-testid="recovery-discard"
          disabled={busy}
          onClick={onDiscard}
          className="rounded border border-slate-700 px-3 py-2 text-sm text-slate-200 disabled:opacity-50"
        >
          Discard and start over
        </button>
      </div>
    </section>
  )
}

/**
 * onDiscarded lets the console start a fresh run the moment a discard
 * succeeds, without a page reload. Discard publishes no bus event -- it
 * deletes the run rather than transitioning it -- so nothing in the stream
 * would otherwise tell App.tsx that POST /api/runs has stopped 409ing.
 *
 * onStopped is the same signal for the same reason, one state over. A
 * successful Stop clears every gate that was refusing new runs -- the active
 * workload itself, and the recovery gate engine.Stop clears on success -- so
 * without it the console sits on a stopped run with no way forward but a page
 * reload, which is precisely the dead end the recovery panel was built to
 * remove.
 */
export function Wizard({ events, onDiscarded, onStopped }: {
  events: AicrEvent[]
  onDiscarded?: () => void
  onStopped?: () => void
}) {
  const run = useMemo(() => deriveRunState(events), [events])
  const [options, setOptions] = useState<Options | null>(null)
  const [optionsError, setOptionsError] = useState('')
  const [actionError, setActionError] = useState('')
  const [retryToken, setRetryToken] = useState(0)
  const [busy, setBusy] = useState(false)
  // The id of the run this console just discarded. The discard is already
  // durable server-side, but the event stream still ends on that run until
  // the replacement publishes its first event, so deriveRunState keeps
  // reporting it as the current, recovered run for that window. Remembering
  // the id is what stops the panel flashing back up with buttons that would
  // now 404.
  const [discardedRunId, setDiscardedRunId] = useState<string | undefined>(undefined)

  const recovered = !!run.recovered

  // CLIENT CONTRACT (internal/api/options.go handleOptions): /api/options is
  // not safe to fetch once on mount and cache -- before Discover finishes
  // there is no snapshot, so the endpoint widens and over-offers. Gating on
  // run.phase reaching "recommend" -- rather than firing unconditionally on
  // mount -- defers the fetch until the moment snapshot.yaml actually
  // exists, so the console never fetches, caches, and shows a provisional
  // answer. It also fires exactly once for the pipeline's one recommend
  // phase: on a live run that is the moment the run parks in
  // awaiting_decision; on a page reload that replays a run already past
  // that point (SSE resumes from the bus's replay ring), phase is already
  // "recommend" on the very first render, so the effect still fires and
  // fetches the verified answer instead of leaving the screen stuck loading.
  //
  // A response whose `provisional` flag is still true is not treated as
  // final: the handler contract explicitly forbids that (see the doc
  // comment on internal/api/options.go's handleOptions), and it is
  // reachable here even after the run parks on recommend, via a corrupt
  // snapshot (internal/aicrclient/options.go). So a provisional answer
  // schedules a bounded, backed-off refetch instead of being handed
  // straight to Recommend; retryToken lets the "Retry" button on an
  // outright fetch failure restart the same cascade from attempt zero.
  //
  // A recovered run is skipped outright: it is parked on the recovery panel
  // with no step running, so the questions cannot be answered until the
  // operator retries -- and engine.Retry publishes "run retrying", which
  // clears run.recovered and re-fires this effect for the resumed run.
  useEffect(() => {
    if (run.phase !== 'recommend' || recovered) return
    let canceled = false
    let timer: ReturnType<typeof setTimeout> | undefined

    function attempt(n: number) {
      setOptionsError('')
      fetchOptions()
        .then(o => {
          if (canceled) return
          if (o.provisional && n < MAX_PROVISIONAL_OPTIONS_RETRIES) {
            const delay = PROVISIONAL_OPTIONS_RETRY_BASE_DELAY_MS * 2 ** n
            timer = setTimeout(() => { if (!canceled) attempt(n + 1) }, delay)
            return
          }
          setOptions(o)
        })
        .catch(err => {
          if (!canceled) setOptionsError(err instanceof Error ? err.message : 'Failed to fetch options')
        })
    }

    attempt(0)
    return () => { canceled = true; clearTimeout(timer) }
  }, [run.phase, recovered, retryToken])

  // Record<string, string> rather than the original { intent, platform }
  // literal: the cockpit's confirm gate sends { apply: 'yes' } through this
  // same path. Recommend's own call site keeps compiling unchanged --
  // { intent, platform } is assignable to Record<string, string>, and
  // function-parameter contravariance holds under strictFunctionTypes.
  async function handleDecide(d: Record<string, string>) {
    if (!run.runId) return
    setActionError('')
    try {
      await decideApi(run.runId, d)
    } catch (err) {
      setActionError(err instanceof ApiError ? err.message : (err as Error).message)
    }
  }

  async function handleRetry() {
    if (!run.runId) return
    setActionError('')
    setBusy(true)
    try {
      await retryRun(run.runId)
    } catch (err) {
      setActionError(err instanceof ApiError ? err.message : (err as Error).message)
    } finally {
      setBusy(false)
    }
  }

  // The only exit from an active run. It can take minutes on a real cluster
  // -- engine.Stop deletes the workload and then waits for its pods to
  // actually be gone -- which is what `busy` is for: the button disables for
  // the whole round trip rather than inviting a second click that would race
  // the first (harmless server-side, since Stop is idempotent, but it would
  // leave the operator with no idea which call the screen is reflecting).
  async function handleStop() {
    if (!run.runId) return
    setActionError('')
    setBusy(true)
    try {
      await stopRun(run.runId)
      onStopped?.()
    } catch (err) {
      setActionError(err instanceof ApiError ? err.message : (err as Error).message)
    } finally {
      setBusy(false)
    }
  }

  // The teardown. Backgrounded server-side, so this resolves as soon as it
  // is accepted -- `busy` covers only that round trip, and the operation
  // itself is followed on the event stream like any other phase.
  async function handleReset() {
    if (!run.runId) return
    setActionError('')
    setBusy(true)
    try {
      await resetRun(run.runId)
    } catch (err) {
      setActionError(err instanceof ApiError ? err.message : (err as Error).message)
    } finally {
      setBusy(false)
    }
  }

  // Only reachable from the recovery panel: engine.Discard refuses a live
  // run, and a recovered one is the only non-live run the console ever shows.
  async function handleDiscard() {
    if (!run.runId) return
    const discarded = run.runId
    setActionError('')
    setBusy(true)
    try {
      await discardRun(discarded)
      setDiscardedRunId(discarded)
      onDiscarded?.()
    } catch (err) {
      setActionError(err instanceof ApiError ? err.message : (err as Error).message)
    } finally {
      setBusy(false)
    }
  }

  function renderRecommend() {
    // Regression guard: the run's own state, not just its phase, gates
    // the ask-form. Phase stays "recommend" for the rest
    // of the run once reached -- finish() (internal/engine/engine.go:262)
    // emits no Phase on "run done"/"run failed" -- so gating on phase alone
    // kept re-rendering an enabled Continue button after the decision was
    // already submitted, and clicking it 409s against Decide's own
    // "run is not awaiting a decision" guard.
    if (run.state !== 'awaiting_decision') {
      return <ResolvedRecommend recipe={run.recipe} />
    }

    if (optionsError) {
      return (
        <div className="mx-auto max-w-2xl space-y-3">
          <p className="text-red-400 text-sm">{optionsError}</p>
          <button
            onClick={() => setRetryToken(n => n + 1)}
            className="rounded border border-slate-700 px-3 py-1 text-sm text-slate-200"
          >
            Retry
          </button>
        </div>
      )
    }

    if (!options) {
      return <p className="text-slate-500 text-sm">Loading the two questions this console asks…</p>
    }

    return (
      <div className="mx-auto max-w-2xl space-y-4">
        {options.provisional && (
          <p className="text-amber-400 text-xs">
            These options could not be verified against the cluster snapshot — some may not resolve.
          </p>
        )}
        <Recommend options={options} onDecide={handleDecide} />
      </div>
    )
  }

  // The layout expands into the cockpit once the run reaches Bundle/Apply:
  // the timeline rail narrows from w-96 to w-80, and Cockpit itself (unlike
  // Discover/Recommend) renders full-width rather than centered under
  // mx-auto max-w-2xl, so the live component pipeline gets the room a
  // one-line-per-decision wizard screen never needed. A recovered run is
  // never the cockpit, whatever phase it stopped in: it is a decision, not a
  // pipeline, and Recovered redraws the persisted component rows itself.
  const cockpit = !recovered && (run.phase === 'bundle' || run.phase === 'apply')

  function renderBody() {
    // First of all, before even the active branch. A teardown in flight is
    // the only thing happening to this run, and every other screen would
    // offer actions the engine refuses while StateResetting holds.
    if (run.state === 'resetting') {
      return <Reset events={events} run={run} />
    }
    // Second. A run whose reset did not finish has exactly one action the
    // engine will accept: hasIncompleteTeardown blocks Start, Retry and
    // Discard, so the recovery panel below -- which offers two of those
    // three -- would be a panel of buttons that all answer 409.
    if (run.residue?.incomplete) {
      return (
        <div className="space-y-4">
          <Reset events={events} run={run} />
          <ResetGate
            run={run}
            components={deriveComponents(events, run.recipe?.components.map(c => c.name))}
            busy={busy}
            onReset={handleReset}
          />
        </div>
      )
    }
    // Before the recovery branch, deliberately. A recovered active run is
    // still holding a workload in the cluster, and the recovery panel offers
    // exactly the two actions the engine rejects for it (Retry needs a failed
    // run; Discard refuses one with a workload running). Stop is the only
    // thing that works, so the screen that carries Stop has to win here or
    // the operator is stranded on a panel of dead buttons.
    if (run.state === 'active') {
      return (
        <div className="space-y-6">
          <Prove events={events} run={run} busy={busy} onStop={handleStop} />
          <ResetGate
            run={run}
            components={deriveComponents(events, run.recipe?.components.map(c => c.name))}
            busy={busy}
            onReset={handleReset}
          />
        </div>
      )
    }
    if (recovered) {
      // The discard already succeeded; the panel's buttons would 404 now, and
      // App.tsx's POST /api/runs is in flight. Say so rather than re-offering
      // an action against a run that no longer exists.
      if (run.runId && run.runId === discardedRunId) {
        return <p className="text-slate-500 text-sm">Discarded. Starting a new run…</p>
      }
      return (
        <Recovered
          events={events}
          run={run}
          busy={busy}
          onRetry={handleRetry}
          onDiscard={handleDiscard}
        />
      )
    }
    // A run that reached Prove and has since been stopped. Without this it
    // fell through to the report branch below and redrew the Discover screen
    // over a finished demo, which reads as the console having started over.
    if (run.phase === 'prove') {
      return (
        <div className="space-y-6">
          <Prove events={events} run={run} busy={busy} onStop={handleStop} />
          <ResetGate
            run={run}
            components={deriveComponents(events, run.recipe?.components.map(c => c.name))}
            busy={busy}
            onReset={handleReset}
          />
        </div>
      )
    }
    if (cockpit) {
      return <Cockpit events={events} run={run} onDecide={handleDecide} onRetry={handleRetry} />
    }
    if (run.phase === 'recommend') return renderRecommend()
    if (run.report) return <Discover report={run.report} />
    return <p className="text-slate-500 text-sm">Discovering the cluster…</p>
  }

  return (
    <div className="flex gap-8">
      <div className="min-w-0 flex-1">
        {/* Suppressed for a recovered run: Recovered states the situation in
            its own words, and repeating the raw engine error above it reads
            as a second, unrelated failure. */}
        {!recovered && run.error && <p className="mb-4 text-red-400 text-sm">{run.error}</p>}
        {actionError && <p className="mb-4 text-red-400 text-sm">{actionError}</p>}

        {renderBody()}
      </div>

      <aside className={`${cockpit ? 'w-80' : 'w-96'} shrink-0 border-l border-slate-800 pl-8`}>
        <Timeline events={events} />
      </aside>
    </div>
  )
}
