import { useEffect, useState } from 'react'
import { bundleUrl } from '../api'
import { componentSeconds, deriveComponents, deriveFailure, deploymentActionsTotal, formatSeconds, installedCount, runElapsed, type ComponentState } from '../pipeline'
import { slowStepNote } from '../slowSteps'
import type { AicrEvent } from '../useEvents'
import { ComponentConditions } from './ComponentConditions'
import type { RunState } from './Wizard'

const statusClass: Record<ComponentState['status'], string> = {
  started: 'text-ink',
  installed: 'text-pass',
  retrying: 'text-warn',
  failed: 'text-fail',
  // Teardown statuses. `removed` is deliberately NOT emerald: a removal
  // succeeding is not the same good news as an install succeeding, and
  // colouring the two alike would make a torn-down cluster read as a
  // healthy one at a glance. `skipped` is amber because a skipped release
  // is something the operator now has to deal with by hand.
  removing: 'text-ink',
  removed: 'text-ink-soft',
  skipped: 'text-warn',
}

/**
 * `terminalState` defaults to undefined: Running is the one screen where
 * the run's observer is still watching, so a condition on it is genuinely
 * current. Ruling 38 (Task 7 final fix wave): Failed and Done both pass
 * their own `run.state` -- the observer tears down its informers the
 * instant a run reaches EITHER terminal state, not just StateDone (an
 * earlier version of this component treated Failed as still-live,
 * reasoning that it "can be mid-retry-decision"; that reasoning conflated
 * "an operator might click Retry next" with "the observer is still
 * watching", which it is not, in either terminal state, until Retry
 * actually restarts the run and the SPA's own `tracked` clears on "run
 * retrying"). The two terminal states pass DIFFERENT values, not a shared
 * boolean, because the wording differs -- see ComponentConditions's doc
 * comment on `terminalState` for why "installed" is a true claim on Done
 * and a false one on Failed.
 */
function ComponentRow({ c, now, terminalState }: {
  c: ComponentState
  now: number
  terminalState?: 'done' | 'failed'
}) {
  const active = c.status === 'started' || c.status === 'retrying'
  const note = active ? slowStepNote(c.name) : undefined
  const seconds = componentSeconds(c, now)

  return (
    <li
      data-testid={`component-${c.name}`}
      className={c.generated ? 'ml-6 border-l border-line pl-3' : ''}
    >
      <div className={`flex items-baseline gap-2 font-mono text-sm ${statusClass[c.status]} ${c.generated ? 'text-xs opacity-70' : ''}`}>
        {/* A glyph for the ordinary outcome and a word for every other one.
            Eleven identical INSTALLEDs down a column is a column of noise
            that hides the one row differing from it -- but the word still
            reaches a screen reader, which is what sr-only is for. */}
        {c.status === 'installed' ? (
          <span className="text-pass" aria-hidden="true">✓</span>
        ) : null}
        <span>{c.name}</span>
        {c.status === 'installed' ? (
          <span className="sr-only">installed</span>
        ) : (
          <span className="text-xs uppercase text-ink-faint">{c.status}</span>
        )}
        {/* The in-flight row has to be findable without reading the column.
            A pulse says "working" in the one place a stalled install is
            indistinguishable from a finished one. */}
        {active && (
          <span
            data-testid={`active-${c.name}`}
            className="inline-block h-1.5 w-1.5 animate-pulse rounded-full bg-accent"
            aria-label="in progress"
          />
        )}
        {seconds !== undefined && (
          <span className="text-xs text-ink-faint">{formatSeconds(seconds)}</span>
        )}
        {c.status === 'retrying' && (
          <span className="text-xs text-warn">attempt {c.attempt}/{c.maxAttempts}</span>
        )}
        {c.reason && <span className="text-xs text-ink-soft">{c.reason}</span>}
        {c.status === 'failed' && c.attempt !== undefined && (
          <span className="text-xs text-fail">after {c.attempt} attempts</span>
        )}
      </div>
      {note && <p className="mt-1 max-w-2xl text-xs text-ink-faint">{note}</p>}
      <ComponentConditions name={c.name} conditions={c.conditions} terminalState={terminalState} />
    </li>
  )
}

/**
 * Gate is the console's confirm gate: the run parks in awaiting_decision
 * before Apply because it is about to install the whole recipe with
 * cluster-admin, and that must not begin without an explicit click (see
 * internal/steps/apply.go's doc comment on NewApply). It lists recipe.json's
 * components -- what the user is actually approving -- not deploy.sh's
 * numbered steps, which do not exist until Apply runs them.
 */
function Gate({ run, onDecide }: { run: RunState; onDecide: (d: Record<string, string>) => void }) {
  const recipe = run.recipe

  return (
    <section className="space-y-6">
      <div>
        <h2 className="text-2xl font-semibold text-ink-strong">Review the bundle before it touches the cluster</h2>
        <p className="mt-1 text-sm text-ink-soft">
          {recipe
            ? `${recipe.componentCount} components, every version pinned.`
            : 'Resolving the bundle…'}
        </p>
      </div>

      {recipe && (
        <ul className="space-y-1 font-mono text-xs text-ink-soft">
          {recipe.components.map(c => (
            <li key={c.name}>{c.name} {c.version} → {c.namespace}</li>
          ))}
        </ul>
      )}

      <div className="flex items-center gap-6">
        <button
          onClick={() => onDecide({ apply: 'yes' })}
          className="rounded bg-accent px-4 py-2 font-medium text-bg"
        >
          Install
        </button>
        {run.runId && (
          <a href={bundleUrl(run.runId)} className="text-sm text-ink-soft underline">
            Download bundle
          </a>
        )}
      </div>
    </section>
  )
}

/**
 * RecipeUnknownNote is shown wherever the recipe hasn't loaded yet (see the
 * doc comment on deriveComponents's recipeComponentNames parameter). Gate
 * already covers this case with its own "Resolving the bundle…" text;
 * Running/Failed/Done need the equivalent, since those are reachable with
 * no recipe too -- e.g. after a page reload whose SSE replay missed the
 * recommend phase's log event.
 */
function RecipeUnknownNote() {
  return (
    <p className="text-warn text-xs">
      The approved recipe hasn't loaded yet — steps below are shown as ordinary components until it does.
    </p>
  )
}

/**
 * ProgressLine states the two counts side by side -- see OVERRIDE 1: a
 * resolved recipe's component count and deploy.sh's own deployment-action
 * total are different things and must never share one label -- and now gives
 * the action total the numerator it was missing.
 *
 * Both counts were denominators: the screen said "14 components, 16
 * deployment actions" and then listed statuses one by one, so an operator
 * sixteen minutes into an install could not tell minute 3 from minute 13
 * without counting rows by eye. `done` counts ACTIONS, matching the only
 * total that advances during Apply.
 */
function ProgressLine({ recipeCount, actionTotal, done, elapsed }: {
  recipeCount?: number
  actionTotal?: number
  done: number
  elapsed?: number
}) {
  const pct = actionTotal ? Math.round((done / actionTotal) * 100) : 0
  return (
    <div data-testid="cockpit-progress" className="space-y-2">
      <p className="text-sm text-ink-soft">
        {actionTotal !== undefined && (
          <span className="text-ink">{done} of {actionTotal} installed</span>
        )}
        {recipeCount !== undefined && <span> · {recipeCount} components</span>}
        {elapsed !== undefined && <span> · {formatSeconds(elapsed)} elapsed</span>}
      </p>
      {actionTotal !== undefined && (
        <div className="h-1 w-full max-w-xl overflow-hidden rounded bg-panel-2">
          <div className="h-full bg-accent transition-all duration-500" style={{ width: `${pct}%` }} />
        </div>
      )}
    </div>
  )
}

/**
 * useNow ticks once a second while a run is in flight, and not at all when it
 * is not.
 *
 * The elapsed figures are derived from event timestamps rather than stored,
 * so something has to re-render for a live one to advance -- but a timer that
 * kept running on a finished screen would repaint a static result forever.
 */
function useNow(live: boolean): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!live) return
    const id = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(id)
  }, [live])
  return now
}

function Running({ components, recipeCount }: { components: ComponentState[]; recipeCount?: number }) {
  const actionTotal = deploymentActionsTotal(components)
  const now = useNow(true)
  const done = installedCount(components)
  const elapsed = runElapsed(components, now)

  return (
    <section className="space-y-4">
      <h2 className="text-2xl font-semibold text-ink-strong">Installing the bundle</h2>
      {recipeCount === undefined && <RecipeUnknownNote />}
      <ProgressLine recipeCount={recipeCount} actionTotal={actionTotal} done={done} elapsed={elapsed} />
      {components.length === 0 ? (
        <p className="text-ink-faint text-sm">Generating the bundle…</p>
      ) : (
        <ul className="space-y-3">
          {components.map(c => <ComponentRow key={c.name} c={c} now={now} />)}
        </ul>
      )}
    </section>
  )
}

function Failed({
  components, recipeCount, failure, onRetry,
}: {
  components: ComponentState[]
  recipeCount?: number
  failure: ReturnType<typeof deriveFailure>
  onRetry: () => void
}) {
  const actionTotal = deploymentActionsTotal(components)
  const now = useNow(false)
  const done = installedCount(components)
  const elapsed = runElapsed(components, now)

  return (
    <section className="space-y-4">
      <h2 className="text-2xl font-semibold text-fail">Install failed</h2>
      {recipeCount === undefined && <RecipeUnknownNote />}
      <ProgressLine recipeCount={recipeCount} actionTotal={actionTotal} done={done} elapsed={elapsed} />
      <ul className="space-y-3">
        {components.map(c => <ComponentRow key={c.name} c={c} now={now} terminalState="failed" />)}
      </ul>

      {failure && (
        <div className="space-y-2 rounded border border-fail/40 bg-fail/10 p-3">
          <p className="text-fail text-sm">
            {failure.component && <span className="font-mono">{failure.component}: </span>}
            {failure.exitError}
          </p>
          <details className="text-sm text-ink-faint">
            <summary className="cursor-pointer">Diagnostic tail</summary>
            <pre data-testid="failure-tail" className="mt-2 overflow-auto text-xs">{failure.tail.join('\n')}</pre>
          </details>
        </div>
      )}

      <button
        onClick={onRetry}
        className="rounded border border-line px-3 py-1 text-sm text-ink"
      >
        Retry
      </button>
    </section>
  )
}

function Done({ components, recipeCount }: { components: ComponentState[]; recipeCount?: number }) {
  const actionTotal = deploymentActionsTotal(components)
  const now = useNow(false)
  const done = installedCount(components)
  const elapsed = runElapsed(components, now)

  return (
    <section className="space-y-4">
      <h2 className="text-2xl font-semibold text-ink-strong">Bundle installed</h2>
      <p data-testid="cockpit-success" className="text-pass">
        Every component in the bundle installed successfully.
      </p>
      {recipeCount === undefined && <RecipeUnknownNote />}
      <ProgressLine recipeCount={recipeCount} actionTotal={actionTotal} done={done} elapsed={elapsed} />
      <ul className="space-y-3">
        {components.map(c => <ComponentRow key={c.name} c={c} now={now} terminalState="done" />)}
      </ul>
    </section>
  )
}

/**
 * Cockpit is the hero component once the run reaches Bundle/Apply: a
 * presentation component only -- every derivation comes from pipeline.ts,
 * every action is a prop. Four branches off run.state (plus run.phase for
 * the gate, since awaiting_decision is also how Recommend parks):
 * the confirm gate, the running pipeline, the failure screen, and done.
 */
export function Cockpit({ events, run, onDecide, onRetry }: {
  events: AicrEvent[]
  run: RunState
  onDecide: (d: Record<string, string>) => void
  onRetry: () => void
}) {
  // undefined, not `?? []`, when run.recipe hasn't loaded yet: deriveComponents
  // treats "unknown" and "known-empty" as different claims, and only the
  // latter licenses calling a marker name a generated action.
  const recipeComponentNames = run.recipe?.components.map(c => c.name)
  const components = deriveComponents(events, recipeComponentNames)
  const failure = deriveFailure(events)
  const recipeCount = run.recipe?.componentCount

  if (run.state === 'awaiting_decision' && run.phase === 'apply') {
    return <Gate run={run} onDecide={onDecide} />
  }

  if (run.state === 'failed') {
    return <Failed components={components} recipeCount={recipeCount} failure={failure} onRetry={onRetry} />
  }

  if (run.state === 'done') {
    return <Done components={components} recipeCount={recipeCount} />
  }

  return <Running components={components} recipeCount={recipeCount} />
}
