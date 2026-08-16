import type { AicrEvent } from './useEvents'

/**
 * ComponentData mirrors Go's applier.ComponentData (internal/applier/parse.go)
 * field for field. It is the Data payload on every KindComponent event.
 */
export interface ComponentData {
  name: string
  namespace?: string
  index?: number
  total?: number
  status: 'started' | 'installed' | 'failed' | 'retrying'
  attempt?: number
  maxAttempts?: number
  retryInSeconds?: number
}

/** FailureInfo mirrors Go's applier.FailureData. */
export interface FailureInfo {
  component?: string
  exitError: string
  tail: string[]
}

export interface ComponentState extends ComponentData {
  /** Wall-clock of the header event, so the UI can show elapsed time. */
  startedAt?: string
  /** Wall-clock of the terminal event for this component. */
  endedAt?: string
  /**
   * True when this deploy.sh step's name is absent from the recipe's own
   * component list -- a generated action (e.g. kubeflow-trainer-post) the
   * bundler inserted and numbered alongside real components, not one the
   * user reviewed and approved on the confirm gate. See OVERRIDE 1: this is
   * a set-difference classification, never a name-suffix guess, because the
   * set of generated-action kinds changes between AICR releases.
   */
  generated: boolean
  /** For a generated action, the name of the component step it trails. Undefined for a real component. */
  parent?: string
}

function isComponentData(data: unknown): data is ComponentData {
  return typeof data === 'object' && data !== null && 'name' in data && 'status' in data
}

function isFailureData(data: unknown): data is FailureInfo {
  return typeof data === 'object' && data !== null && 'exitError' in data
}

/**
 * currentRunIdOf mirrors Wizard.tsx's helper of the same name: the run id
 * the console is currently showing is the runId on the most recent event
 * that carries one. Not imported from Wizard.tsx -- that module isn't
 * relocating its state derivation, and this is five lines, not a module
 * worth sharing across a component boundary for.
 */
function currentRunIdOf(events: AicrEvent[]): string | undefined {
  for (let i = events.length - 1; i >= 0; i--) {
    if (events[i].runId) return events[i].runId
  }
  return undefined
}

function relevantTo(events: AicrEvent[]): AicrEvent[] {
  const runId = currentRunIdOf(events)
  return runId ? events.filter(e => e.runId === runId) : events
}

/**
 * deriveComponents replays the current run's KindComponent events into one
 * ComponentState per deploy.sh step, in the order deploy.sh first reported
 * them -- a single pass keyed by name so a later status event (installed,
 * failed, retrying) updates the same entry rather than appending a new one.
 *
 * recipeComponentNames is recipe.json's own component list (RunState.recipe
 * on the confirm gate) -- what the user reviewed and approved. deploy.sh
 * numbers its own generated actions (e.g. kubeflow-trainer-post) alongside
 * real components, so a marker name absent from that list is classified as
 * a generated action subordinate to the most recent real component, not as
 * a peer (OVERRIDE 1).
 *
 * recipeComponentNames is undefined, not just empty, when the recipe hasn't
 * loaded yet -- e.g. the SSE replay ring dropped or hasn't yet delivered the
 * recommend phase's log event (see useEvents.ts's doc comment on
 * subscriberBuffer), which is reachable on a page reload or simply by
 * rendering the cockpit before that event arrives. "Unknown" and
 * "known-empty" are different claims and only the latter licenses calling a
 * name absent: collapsing them (e.g. via `?? []` at the call site) makes
 * every step -- including genuinely approved components -- classify as a
 * generated action, which is a confident claim about the recipe with no
 * basis. When the recipe is unknown, nothing is classified as generated;
 * every marker genuinely is a deployment action, and presenting it as an
 * ordinary component is the fail-safe reading.
 *
 * `index`/`total` are only present on a step's header ("started") event --
 * internal/applier/parse.go's reInstalled/reFailed/reRetry markers carry
 * neither -- so they are carried forward from the header rather than
 * derived from components.length, which would undercount a component that
 * never started (deploy.sh stopped before reaching it).
 */
export function deriveComponents(events: AicrEvent[], recipeComponentNames: string[] | undefined): ComponentState[] {
  const recipeKnown = recipeComponentNames !== undefined
  const recipeSet = new Set(recipeComponentNames ?? [])
  const order: string[] = []
  const byName = new Map<string, ComponentState>()
  let lastRealComponent: string | undefined

  for (const e of relevantTo(events)) {
    if (e.kind !== 'component' || !isComponentData(e.data)) continue
    const data = e.data
    const existing = byName.get(data.name)
    const generated = recipeKnown && !recipeSet.has(data.name)

    const next: ComponentState = {
      ...data,
      index: data.index ?? existing?.index,
      total: data.total ?? existing?.total,
      namespace: data.namespace ?? existing?.namespace,
      // The terminal "failed" marker carries only the final Attempt, not
      // MaxAttempts (internal/applier/parse.go's reFailed) -- carry it
      // forward from the retrying event that preceded it so the failure
      // screen can still say "failed on attempt 2 of 2" rather than losing
      // the denominator on the very last event.
      maxAttempts: data.maxAttempts ?? existing?.maxAttempts,
      generated,
      parent: generated ? (existing?.parent ?? lastRealComponent) : undefined,
      startedAt: existing?.startedAt ?? (data.status === 'started' ? e.at : undefined),
      endedAt: data.status === 'installed' || data.status === 'failed' ? e.at : existing?.endedAt,
    }

    if (!existing) order.push(data.name)
    byName.set(data.name, next)
    if (!generated) lastRealComponent = data.name
  }

  return order.map(name => byName.get(name)!)
}

/**
 * deploymentActionsTotal is deploy.sh's own N-of-M total, read off the most
 * recently derived step rather than components.length. Never rounds this to
 * the recipe's component count -- see OVERRIDE 1: a resolved recipe can list
 * 13 components while deploy.sh runs 14 numbered steps, and a UI whose
 * progress disagrees with the log a user reads in `kubectl logs` when
 * something fails is worse than one showing an unfamiliar number.
 */
export function deploymentActionsTotal(components: ComponentState[]): number | undefined {
  return components.length > 0 ? components[components.length - 1].total : undefined
}

/**
 * deriveFailure returns the single terminal KindError event's FailureData,
 * scanning from the end because the applier's own detailed KindError (with
 * Data) is followed by a second, generic KindError the engine publishes
 * from the step's returned error (internal/engine/engine.go's runStep) --
 * same Kind, no Data. Scanning for the last event whose Data actually
 * carries exitError skips that trailing generic one instead of returning it
 * empty-handed.
 */
export function deriveFailure(events: AicrEvent[]): FailureInfo | null {
  const relevant = relevantTo(events)
  for (let i = relevant.length - 1; i >= 0; i--) {
    const e = relevant[i]
    if (e.kind === 'error' && isFailureData(e.data)) {
      return { component: e.data.component, exitError: e.data.exitError, tail: e.data.tail }
    }
  }
  return null
}
