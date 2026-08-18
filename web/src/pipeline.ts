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
  /**
   * Cluster conditions currently PLACED on this row, one per distinct (UID,
   * Reason) the observer has reported. "Placed", not "arose while this
   * action was active": Component is a moving cursor (see
   * clusterConditionSupersedes's doc comment / Ruling 27), so a condition's
   * placement can move to a later row than the one it arose on, or stay put
   * under a resolution that carried no attribution at all. This is NOT the
   * same as "what the row shows" -- see activeCondition -- a row keeps
   * every condition currently placed on it, resolved or not, so a later
   * resolution or recurrence on the same (UID, Reason) can update its own
   * slot instead of losing the others sharing this row.
   */
  conditions: ClusterCondition[]
}

/**
 * ClusterCondition mirrors Go's bus.ClusterData (internal/bus/cluster.go)
 * field for field, plus message -- which lives on the wrapping Event, not
 * ClusterData, but is carried alongside here because a row has nothing else
 * to render as the human-readable narration.
 */
export interface ClusterCondition {
  kind: string
  namespace?: string
  name: string
  uid: string
  container?: string
  reason: string
  ready?: number
  desired?: number
  /** 0 info / 1 warn / 2 error -- bus.Severity's own ordering. */
  severity: number
  resolved?: boolean
  at: string
  message: string
}

function isComponentData(data: unknown): data is ComponentData {
  return typeof data === 'object' && data !== null && 'name' in data && 'status' in data
}

function isFailureData(data: unknown): data is FailureInfo {
  return typeof data === 'object' && data !== null && 'exitError' in data
}

function isClusterData(data: unknown): data is Omit<ClusterCondition, 'message'> {
  return typeof data === 'object' && data !== null && 'uid' in data && 'reason' in data && 'severity' in data && 'at' in data
}

/** conditionKey identifies one row's condition slot: same UID AND same Reason, matching bus.ClusterData.Supersedes's own identity rule. */
function conditionKey(c: Pick<ClusterCondition, 'uid' | 'reason'>): string {
  return `${c.uid}::${c.reason}`
}

/**
 * clusterConditionSupersedes mirrors bus.ClusterData.Supersedes
 * (internal/bus/cluster.go) field for field: same UID AND same Reason (the
 * caller already guarantees this via conditionKey, but the check is kept
 * here too so this function matches its Go counterpart's contract on its
 * own, independent of the caller), then later At wins outright regardless
 * of Resolved or Severity; those two only break a tie on an identical At.
 * Kept as its own function rather than inlined into the fold so the
 * required bite-proof -- drop the UID half of this check -- is a one-line
 * mutation with an obvious, nameable blast radius. Exported so each branch
 * (the UID/Reason guard, At-primary ordering, and the Resolved-then-Severity
 * tie-break) can be pinned directly, without constructing a whole event
 * sequence to reach it (Important 1, Task 7 fix round 1: an unconditional
 * `return true` here previously left the suite green, because nothing
 * exercised anything past the guard).
 *
 * At is compared as a plain string, not via Date.parse: Observer.publish
 * always stamps RFC3339 in UTC (time.Now().UTC()), and for timestamps in
 * that one fixed format, lexicographic string order IS chronological
 * order -- including whatever sub-millisecond precision Go's clock
 * produces, which Date.parse would silently truncate to whole
 * milliseconds. It also cannot produce NaN the way Date.parse can on a
 * malformed value: a NaN > NaN comparison is always false, which would
 * pin a condition on its row forever rather than fail loudly (Minor 1,
 * Task 7 fix round 1) -- silently invisible is the worse failure mode of
 * the two.
 */
export function clusterConditionSupersedes(next: ClusterCondition, prev: ClusterCondition): boolean {
  if (next.uid !== prev.uid || next.reason !== prev.reason) return false
  if (next.at !== prev.at) return next.at > prev.at
  if (Boolean(next.resolved) !== Boolean(prev.resolved)) return Boolean(next.resolved)
  return next.severity > prev.severity
}

/**
 * TrackedCondition is the fold's per-(UID, Reason) unit of truth. `data` is
 * the current ClusterCondition; `component` is a SEPARATE, independently
 * updated field saying which row it currently renders under.
 *
 * Ruling 27 (Task 7 fix round 1, Critical 1). The previous version nested
 * the fold under Event.Component first and (UID, Reason) second -- i.e. it
 * treated Component as part of the condition's IDENTITY. It is not.
 * internal/engine/attribution.go's ActiveAction is a moving cursor, empty
 * between actions and outside Apply, so an arising condition and its own
 * resolution routinely carry DIFFERENT Component values, or the resolution
 * carries none at all -- that is the direct, unavoidable consequence of the
 * temporal framing this whole feature is built on (Section 1 of the design
 * doc), not a bug in the stamping. Nesting under Component first meant a
 * resolution attributed to a LATER action created a brand-new entry on that
 * later action's row instead of superseding the original -- the row that
 * actually displayed the condition never saw the superseding entry and kept
 * showing it, unresolved, forever. Exactly the failure
 * internal/bus/cluster.go's own doc comment opens with: "a stale
 * ImagePullBackOff ends up pinned to a row forever."
 *
 * Keeping identity flat on (UID, Reason) and placement as a trailing field
 * means a resolution ALWAYS finds and updates the one entry that matters,
 * regardless of which row either event was attributed to, or whether the
 * resolution was attributed at all.
 */
interface TrackedCondition {
  data: ClusterCondition
  /**
   * Row this condition currently renders under. Undefined when the
   * condition has never been observed with an active action -- it still
   * exists (so a later resolution or recurrence can supersede it), it just
   * has nowhere to render, matching "unattributed events do not attach to
   * any row". Only moves when a SUPERSEDING event itself carries a
   * Component; an unattributed superseding event (between actions, or
   * after the run's terminal state clears ActiveAction) still updates
   * `data` -- that is what lets a between-actions resolution clear a row
   * the arising condition was attributed to -- but leaves placement where
   * it last was, rather than un-rendering a condition that has not
   * actually changed rows.
   */
  component?: string
}

/**
 * foldClusterEvent updates the flat (UID, Reason) identity map from one
 * KindCluster event. Every cluster event is folded here, attributed or not
 * -- see TrackedCondition's doc comment for why an unattributed event must
 * still be able to supersede an existing entry rather than being dropped.
 */
function foldClusterEvent(tracked: Map<string, TrackedCondition>, e: AicrEvent) {
  if (!isClusterData(e.data)) return
  const cond: ClusterCondition = { ...e.data, message: e.message }
  const key = conditionKey(cond)
  const prev = tracked.get(key)
  if (prev && !clusterConditionSupersedes(cond, prev.data)) return
  tracked.set(key, { data: cond, component: e.component || prev?.component })
}

/**
 * activeCondition picks the one condition a row displays: the
 * highest-severity member that is not resolved. A resolved (UID, Reason)
 * keeps its slot in the row's full set -- a later recurrence supersedes it
 * in place -- it just stops competing to be shown. Returns undefined when
 * every condition on the row has resolved, which is a full clear: the row
 * genuinely has nothing outstanding, not "waiting on the next one."
 *
 * On an exact severity tie, the later At wins (Minor 4, Task 7 fix round
 * 1). `conditions`' array order reflects Map insertion order -- i.e. when a
 * (UID, Reason) FIRST arose, not its most recent update, since
 * clusterConditionSupersedes updates a slot in place without moving it --
 * so picking the first same-severity match would silently prefer whichever
 * condition happened to arrive first and never budge again. The contract
 * does not state a tie-break; "most recent" is the more defensible default
 * of the two, and matches the At-primary rule Supersedes itself uses one
 * level up.
 */
export function activeCondition(conditions: ClusterCondition[]): ClusterCondition | undefined {
  let best: ClusterCondition | undefined
  for (const c of conditions) {
    if (c.resolved) continue
    if (!best || c.severity > best.severity || (c.severity === best.severity && c.at > best.at)) best = c
  }
  return best
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
 * RUN_RETRYING_MESSAGE is the exact KindPhase Message engine.Retry publishes
 * (internal/engine/engine.go's Retry: `bus.Event{RunID: runID, Kind:
 * bus.KindPhase, Message: "run retrying"}`) at the instant it starts
 * re-executing a failed run. Ruling 28 (Task 7 fix round 1, Important 2):
 * Retry reuses the SAME RunID -- attribution.go's own doc comment says so
 * explicitly -- so relevantTo's RunID filter cannot tell attempt 1 from
 * attempt 2, and a condition observed during the failed attempt would
 * otherwise ride into the retried one forever, describing a pod the retry
 * may have already replaced. This message is the signal that actually marks
 * a new attempt starting; clearing tracked conditions here, not on a RunID
 * change that never happens, uses the real boundary instead of a proxy for
 * one that doesn't exist on this path.
 */
const RUN_RETRYING_MESSAGE = 'run retrying'

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
  // Flat on (UID, Reason) -- see TrackedCondition's doc comment (Ruling 27)
  // for why Component must NOT be part of this map's key. Kept apart from
  // byName because a cluster event's placement can name a row that hasn't
  // been assigned yet at this point in the replay (only the FINAL merge
  // below requires the row to exist); accumulating independently means fold
  // order within this pass never matters to the result.
  const tracked = new Map<string, TrackedCondition>()
  let lastRealComponent: string | undefined

  for (const e of relevantTo(events)) {
    if (e.kind === 'phase' && e.message === RUN_RETRYING_MESSAGE) {
      tracked.clear()
      continue
    }
    if (e.kind === 'cluster') {
      foldClusterEvent(tracked, e)
      continue
    }
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
      // `tracked` is the single source of truth for conditions -- it's
      // computed once, below, after every event in the run has been folded,
      // because a condition's placement can still move on a LATER event
      // than this one (Ruling 27). Carrying `existing`'s conditions forward
      // here would just be a second copy that the final map immediately
      // discards; an empty placeholder makes that discarding obvious rather
      // than implying this value ever gets read.
      conditions: [],
    }

    if (!existing) order.push(data.name)
    byName.set(data.name, next)
    if (!generated) lastRealComponent = data.name
  }

  return order.map(name => ({
    ...byName.get(name)!,
    conditions: [...tracked.values()].filter(t => t.component === name).map(t => t.data),
  }))
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
