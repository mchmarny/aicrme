import { useEffect, useMemo, useState } from 'react'
import type { AicrEvent } from '../useEvents'
import { ApiError, decide as decideApi, fetchOptions, type Options } from '../api'
import { Discover, type CapabilityReport } from './Discover'
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
type RunPhase = 'idle' | 'running' | 'awaiting_decision' | 'failed' | 'done'

interface RunState {
  runId?: string
  phase?: string
  state: RunPhase
  report: CapabilityReport | null
  recipe: RecipeSummary | null
  error?: string
}

function isCapabilityReport(data: unknown): data is CapabilityReport {
  return typeof data === 'object' && data !== null && 'headline' in data && 'gaps' in data
}

function isRecipeSummary(data: unknown): data is RecipeSummary {
  return typeof data === 'object' && data !== null && 'componentCount' in data && 'components' in data
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
        else out.state = 'running'
        break
      case 'decision':
        out.state = 'awaiting_decision'
        break
      case 'error':
        out.state = 'failed'
        out.error = e.message
        break
    }

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
        Decisions submitted — <strong>{recipe.componentCount} components</strong> resolved, every version pinned and signed.
      </p>
      <ul className="space-y-1 font-mono text-xs text-slate-400">
        {recipe.components.map(c => (
          <li key={c.name}>{c.name} {c.version} → {c.namespace}</li>
        ))}
      </ul>
    </section>
  )
}

export function Wizard({ events }: { events: AicrEvent[] }) {
  const run = useMemo(() => deriveRunState(events), [events])
  const [options, setOptions] = useState<Options | null>(null)
  const [optionsError, setOptionsError] = useState('')
  const [decideError, setDecideError] = useState('')
  const [retryToken, setRetryToken] = useState(0)

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
  useEffect(() => {
    if (run.phase !== 'recommend') return
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
  }, [run.phase, retryToken])

  async function handleDecide(d: { intent: string; platform: string }) {
    if (!run.runId) return
    setDecideError('')
    try {
      await decideApi(run.runId, d)
    } catch (err) {
      setDecideError(err instanceof ApiError ? err.message : (err as Error).message)
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

  return (
    <div className="flex gap-8">
      <div className="min-w-0 flex-1">
        {run.error && <p className="mb-4 text-red-400 text-sm">{run.error}</p>}
        {decideError && <p className="mb-4 text-red-400 text-sm">{decideError}</p>}

        {run.phase === 'recommend' ? (
          renderRecommend()
        ) : run.report ? (
          <Discover report={run.report} />
        ) : (
          <p className="text-slate-500 text-sm">Discovering the cluster…</p>
        )}
      </div>

      <aside className="w-96 shrink-0 border-l border-slate-800 pl-8">
        <Timeline events={events} />
      </aside>
    </div>
  )
}
