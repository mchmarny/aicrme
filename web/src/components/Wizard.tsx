import { useEffect, useMemo, useState } from 'react'
import type { AicrEvent } from '../useEvents'
import { decide as decideApi, fetchOptions, type Options } from '../api'
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

/** deriveRunState replays events in arrival order into the run's current phase, state, and any artifacts it has published so far. Exported for testing against the recorded fixture. */
export function deriveRunState(events: AicrEvent[]): RunState {
  const out: RunState = { state: 'idle', report: null, recipe: null }

  for (const e of events) {
    if (e.runId) out.runId = e.runId
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

export function Wizard({ events }: { events: AicrEvent[] }) {
  const run = useMemo(() => deriveRunState(events), [events])
  const [options, setOptions] = useState<Options | null>(null)
  const [decideError, setDecideError] = useState('')

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
  useEffect(() => {
    if (run.phase !== 'recommend') return
    let canceled = false
    fetchOptions().then(o => { if (!canceled) setOptions(o) }).catch(() => {})
    return () => { canceled = true }
  }, [run.phase])

  async function handleDecide(d: { intent: string; platform: string }) {
    if (!run.runId) return
    setDecideError('')
    try {
      await decideApi(run.runId, d)
    } catch (err) {
      setDecideError((err as Error).message)
    }
  }

  return (
    <div className="flex gap-8">
      <div className="min-w-0 flex-1">
        {run.error && <p className="mb-4 text-red-400 text-sm">{run.error}</p>}
        {decideError && <p className="mb-4 text-red-400 text-sm">{decideError}</p>}

        {run.phase === 'recommend' ? (
          options ? (
            <Recommend options={options} recipe={run.recipe} onDecide={handleDecide} />
          ) : (
            <p className="text-slate-500 text-sm">Loading the two questions this console asks…</p>
          )
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
