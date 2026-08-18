import { bundleUrl } from '../api'
import { deriveComponents, deriveFailure, deploymentActionsTotal, type ComponentState } from '../pipeline'
import { slowStepNote } from '../slowSteps'
import type { AicrEvent } from '../useEvents'
import { ComponentConditions } from './ComponentConditions'
import type { RunState } from './Wizard'

const statusClass: Record<ComponentState['status'], string> = {
  started: 'text-slate-300',
  installed: 'text-emerald-400',
  retrying: 'text-amber-400',
  failed: 'text-red-400',
}

function ComponentRow({ c }: { c: ComponentState }) {
  const active = c.status === 'started' || c.status === 'retrying'
  const note = active ? slowStepNote(c.name) : undefined

  return (
    <li
      data-testid={`component-${c.name}`}
      className={c.generated ? 'ml-6 border-l border-slate-800 pl-3' : ''}
    >
      <div className={`flex items-baseline gap-2 font-mono text-sm ${statusClass[c.status]} ${c.generated ? 'text-xs opacity-70' : ''}`}>
        <span>{c.name}</span>
        <span className="text-xs uppercase text-slate-500">{c.status}</span>
        {c.status === 'retrying' && (
          <span className="text-xs text-amber-400">attempt {c.attempt}/{c.maxAttempts}</span>
        )}
        {c.status === 'failed' && c.attempt !== undefined && (
          <span className="text-xs text-red-400">after {c.attempt} attempts</span>
        )}
      </div>
      {note && <p className="mt-1 max-w-2xl text-xs text-slate-500">{note}</p>}
      <ComponentConditions name={c.name} conditions={c.conditions} />
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
        <h2 className="text-2xl font-semibold text-slate-100">Review the bundle before it touches the cluster</h2>
        <p className="mt-1 text-sm text-slate-400">
          {recipe
            ? `${recipe.componentCount} components, every version pinned and signed.`
            : 'Resolving the bundle…'}
        </p>
      </div>

      {recipe && (
        <ul className="space-y-1 font-mono text-xs text-slate-400">
          {recipe.components.map(c => (
            <li key={c.name}>{c.name} {c.version} → {c.namespace}</li>
          ))}
        </ul>
      )}

      <div className="flex items-center gap-6">
        <button
          onClick={() => onDecide({ apply: 'yes' })}
          className="rounded bg-emerald-600 px-4 py-2 text-white"
        >
          Install
        </button>
        {run.runId && (
          <a href={bundleUrl(run.runId)} className="text-sm text-slate-400 underline">
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
    <p className="text-amber-400 text-xs">
      The approved recipe hasn't loaded yet — steps below are shown as ordinary components until it does.
    </p>
  )
}

/** ProgressLine states the two counts side by side -- see OVERRIDE 1: a resolved recipe's component count and deploy.sh's own deployment-action total are different things and must never share one label. */
function ProgressLine({ recipeCount, actionTotal }: { recipeCount?: number; actionTotal?: number }) {
  return (
    <p className="text-sm text-slate-400">
      {recipeCount !== undefined && <span>{recipeCount} components</span>}
      {recipeCount !== undefined && actionTotal !== undefined && <span>, </span>}
      {actionTotal !== undefined && <span>{actionTotal} deployment actions</span>}
    </p>
  )
}

function Running({ components, recipeCount }: { components: ComponentState[]; recipeCount?: number }) {
  const actionTotal = deploymentActionsTotal(components)

  return (
    <section className="space-y-4">
      <h2 className="text-2xl font-semibold text-slate-100">Installing the bundle</h2>
      {recipeCount === undefined && <RecipeUnknownNote />}
      <ProgressLine recipeCount={recipeCount} actionTotal={actionTotal} />
      {components.length === 0 ? (
        <p className="text-slate-500 text-sm">Generating the bundle…</p>
      ) : (
        <ul className="space-y-3">
          {components.map(c => <ComponentRow key={c.name} c={c} />)}
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

  return (
    <section className="space-y-4">
      <h2 className="text-2xl font-semibold text-red-400">Install failed</h2>
      {recipeCount === undefined && <RecipeUnknownNote />}
      <ProgressLine recipeCount={recipeCount} actionTotal={actionTotal} />
      <ul className="space-y-3">
        {components.map(c => <ComponentRow key={c.name} c={c} />)}
      </ul>

      {failure && (
        <div className="space-y-2 rounded border border-red-900 bg-red-950/30 p-3">
          <p className="text-red-400 text-sm">
            {failure.component && <span className="font-mono">{failure.component}: </span>}
            {failure.exitError}
          </p>
          <details className="text-sm text-slate-500">
            <summary className="cursor-pointer">Diagnostic tail</summary>
            <pre data-testid="failure-tail" className="mt-2 overflow-auto text-xs">{failure.tail.join('\n')}</pre>
          </details>
        </div>
      )}

      <button
        onClick={onRetry}
        className="rounded border border-slate-700 px-3 py-1 text-sm text-slate-200"
      >
        Retry
      </button>
    </section>
  )
}

function Done({ components, recipeCount }: { components: ComponentState[]; recipeCount?: number }) {
  const actionTotal = deploymentActionsTotal(components)

  return (
    <section className="space-y-4">
      <h2 className="text-2xl font-semibold text-slate-100">Bundle installed</h2>
      <p data-testid="cockpit-success" className="text-emerald-400">
        Every component in the bundle installed successfully.
      </p>
      {recipeCount === undefined && <RecipeUnknownNote />}
      <ProgressLine recipeCount={recipeCount} actionTotal={actionTotal} />
      <ul className="space-y-3">
        {components.map(c => <ComponentRow key={c.name} c={c} />)}
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
