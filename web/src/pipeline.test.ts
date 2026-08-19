import { describe, expect, it } from 'vitest'
import { activeCondition, clusterConditionSupersedes, compareAt, deriveComponents, deriveFailure, deploymentActionsTotal, type ClusterCondition } from './pipeline'
import type { AicrEvent } from './useEvents'
import applyRun from './fixtures/apply-run.json'

// apply-run.json is hand-authored to match exactly what internal/applier's
// parse.go (Task 4) emits, transcribed from a real deploy.sh transcript
// (internal/applier/testdata/deploy-transcript-kwok.txt) rather than
// invented: cert-manager, gpu-operator, and kubeflow-trainer install
// cleanly; kubeflow-trainer-post -- the real bundler-generated post-install
// action that trails kubeflow-trainer in that transcript -- installs right
// after it; kai-scheduler retries once and then fails, ending the run. The
// recipe (recommend phase's log event) approves only 4 components --
// cert-manager, gpu-operator, kai-scheduler, kubeflow-trainer --
// deliberately NOT including kubeflow-trainer-post, so classifying it
// requires the set-difference rule (OVERRIDE 1), not a name-suffix guess.
const events = applyRun as AicrEvent[]
const recipeComponentNames = ['cert-manager', 'gpu-operator', 'kai-scheduler', 'kubeflow-trainer']

describe('deriveComponents', () => {
  it('returns one entry per deploy.sh step in first-seen order', () => {
    const got = deriveComponents(events, recipeComponentNames)
    expect(got.map(c => c.name)).toEqual([
      'cert-manager',
      'gpu-operator',
      'kubeflow-trainer',
      'kubeflow-trainer-post',
      'kai-scheduler',
    ])
  })

  it('ends a component seen started then installed at status installed', () => {
    const got = deriveComponents(events, recipeComponentNames)
    const certManager = got.find(c => c.name === 'cert-manager')!
    expect(certManager.status).toBe('installed')
  })

  it('ends a component seen started, retrying, then failed at status failed and retains its attempt counts', () => {
    const got = deriveComponents(events, recipeComponentNames)
    const kaiScheduler = got.find(c => c.name === 'kai-scheduler')!
    expect(kaiScheduler.status).toBe('failed')
    expect(kaiScheduler.attempt).toBe(2)
    expect(kaiScheduler.maxAttempts).toBe(2)
  })

  it('carries index/total from the header event onto every later status for that step', () => {
    const got = deriveComponents(events, recipeComponentNames)
    const certManager = got.find(c => c.name === 'cert-manager')!
    expect(certManager.index).toBe(1)
    expect(certManager.total).toBe(5)
    // The terminal "installed" marker carries neither field (see
    // internal/applier/parse.go's reInstalled) -- deriveComponents must
    // carry the header's values forward rather than letting them go
    // undefined once the header event scrolls out of view.
    const kaiScheduler = got.find(c => c.name === 'kai-scheduler')!
    expect(kaiScheduler.index).toBe(5)
    expect(kaiScheduler.total).toBe(5)
  })

  // This is the regression guard OVERRIDE 1 calls out by name: a fixture
  // using only names that appear in both lists would pass deriveComponents
  // without any classification logic at all. kubeflow-trainer-post is a
  // real marker name (see the transcript cited above) that is absent from
  // recipeComponentNames, so it must classify as a generated action
  // subordinate to kubeflow-trainer -- never as a fifth recipe component.
  it('classifies a marker name absent from the recipe as a generated action subordinate to its parent, not as a component', () => {
    const got = deriveComponents(events, recipeComponentNames)
    const post = got.find(c => c.name === 'kubeflow-trainer-post')!
    expect(post.generated).toBe(true)
    expect(post.parent).toBe('kubeflow-trainer')

    const real = got.filter(c => !c.generated).map(c => c.name)
    expect(real).toEqual(['cert-manager', 'gpu-operator', 'kubeflow-trainer', 'kai-scheduler'])
  })

  // Regression guard: when the recipe hasn't loaded yet (e.g. the SSE
  // replay ring dropped or hasn't yet delivered the recommend phase's log
  // event -- see useEvents.ts's doc comment on subscriberBuffer), the
  // caller cannot yet say a name is genuinely absent from the recipe. An
  // unknown recipe must not be treated the same as a known-empty one:
  // `?? []` at the call site collapsed that distinction and made every
  // step -- including cert-manager and gpu-operator -- classify as a
  // generated action, inverting the exact peer-vs-subordinate distinction
  // Override 1 exists to establish. undefined (recipe unknown) must
  // classify nothing as generated; only a known list licenses that call.
  it('classifies nothing as generated when the recipe is not yet known', () => {
    const got = deriveComponents(events, undefined)
    expect(got.every(c => !c.generated)).toBe(true)
    expect(got.every(c => c.parent === undefined)).toBe(true)
  })

  it('excludes events from an older run id, the same rule deriveRunState applies', () => {
    const staleThenCurrent: AicrEvent[] = [
      {
        id: 100, runId: 'old-run', at: '2026-08-15T00:00:00Z', kind: 'component', level: 'info',
        phase: 'apply', component: 'cert-manager', message: 'installing cert-manager',
        data: { name: 'cert-manager', index: 1, total: 1, status: 'started' },
      },
      {
        id: 101, runId: 'old-run', at: '2026-08-15T00:00:01Z', kind: 'component', level: 'info',
        phase: 'apply', component: 'cert-manager', message: 'cert-manager installed',
        data: { name: 'cert-manager', status: 'installed' },
      },
      ...events,
    ]
    const got = deriveComponents(staleThenCurrent, recipeComponentNames)
    expect(got.map(c => c.name)).toEqual([
      'cert-manager',
      'gpu-operator',
      'kubeflow-trainer',
      'kubeflow-trainer-post',
      'kai-scheduler',
    ])
  })
})

describe('deploymentActionsTotal', () => {
  it("reports deploy.sh's own total, not the recipe's component count", () => {
    const got = deriveComponents(events, recipeComponentNames)
    expect(deploymentActionsTotal(got)).toBe(5)
    expect(recipeComponentNames.length).toBe(4)
  })
})

describe('deriveFailure', () => {
  it('returns the component, exit error, and tail from the terminal error event', () => {
    const got = deriveFailure(events)
    expect(got).not.toBeNull()
    expect(got!.component).toBe('kai-scheduler')
    expect(got!.exitError).toBe('exit status 1')
    expect(got!.tail.length).toBeGreaterThan(0)
    expect(got!.tail[0]).toContain('kai-scheduler FAILED')
  })

  it('returns null when there is no error event', () => {
    const noError = events.filter(e => e.kind !== 'error')
    expect(deriveFailure(noError)).toBeNull()
  })
})

/**
 * clusterEvent builds a KindCluster AicrEvent whose Data mirrors Go's
 * bus.ClusterData (internal/bus/cluster.go) field-for-field. component is
 * Event.Component -- the attribution stamp, deliberately a top-level
 * parameter rather than folded into data, because it answers a different
 * question than the resource fields do: which row the observer's snapshot
 * says was active, not what the resource is.
 */
function clusterEvent(
  id: number,
  component: string | undefined,
  data: { uid: string; reason: string; severity: number; resolved?: boolean; at: string; name?: string },
  message = 'cluster event',
): AicrEvent {
  return {
    id,
    runId: 'run1',
    at: data.at,
    kind: 'cluster',
    level: 'info',
    phase: 'apply',
    component,
    message,
    data: { kind: 'Pod', namespace: 'gpu-operator', name: data.name ?? 'a-pod', ...data },
  }
}

describe('deriveComponents cluster conditions', () => {
  const headers: AicrEvent[] = [
    { id: 1, runId: 'run1', at: '2026-08-15T09:00:00Z', kind: 'component', level: 'info', phase: 'apply', component: 'cert-manager', message: 'installing cert-manager', data: { name: 'cert-manager', index: 1, total: 2, status: 'started' } },
    { id: 2, runId: 'run1', at: '2026-08-15T09:00:01Z', kind: 'component', level: 'info', phase: 'apply', component: 'cert-manager', message: 'cert-manager installed', data: { name: 'cert-manager', status: 'installed' } },
    { id: 3, runId: 'run1', at: '2026-08-15T09:00:02Z', kind: 'component', level: 'info', phase: 'apply', component: 'gpu-operator', message: 'installing gpu-operator', data: { name: 'gpu-operator', index: 2, total: 2, status: 'started' } },
  ]

  it('folds an attributed cluster event into the matching row, keyed on Component', () => {
    const withEvents: AicrEvent[] = [
      ...headers,
      clusterEvent(4, 'cert-manager', { uid: 'uid-cm', reason: 'Warning', severity: 1, at: '2026-08-15T09:00:03Z' }),
      clusterEvent(5, 'gpu-operator', { uid: 'uid-gpu', reason: 'ImagePullBackOff', severity: 2, at: '2026-08-15T09:00:04Z' }),
    ]
    const got = deriveComponents(withEvents, undefined)
    const certManager = got.find(c => c.name === 'cert-manager')!
    const gpuOperator = got.find(c => c.name === 'gpu-operator')!

    expect(certManager.conditions.map(c => c.reason)).toEqual(['Warning'])
    expect(gpuOperator.conditions.map(c => c.reason)).toEqual(['ImagePullBackOff'])
  })

  it('leaves an unattributed cluster event unattached to any row and still present in the timeline', () => {
    const unattributed = clusterEvent(4, undefined, { uid: 'uid-x', reason: 'Warning', severity: 1, at: '2026-08-15T09:00:03Z' })
    const withEvents: AicrEvent[] = [...headers, unattributed]

    const got = deriveComponents(withEvents, undefined)
    for (const row of got) expect(row.conditions).toHaveLength(0)
    expect(withEvents).toContain(unattributed)
  })

  // The stranded-entry pattern named in the phase's standing instruction: a
  // condition arising, a second coexisting on a different UID, both
  // resolving, and the first recurring is ONE sequence, asserted at each
  // step by re-deriving against a growing prefix of the same event log --
  // not three isolated single-condition tests, each of which would only
  // ever see one entry in play and could not catch a fold that quietly
  // grows a stray third entry instead of reusing the (UID, Reason) slot.
  it('coexists across UIDs, supersedes within a UID, clears on full resolution, and re-arms on recurrence -- without stranding an entry', () => {
    const podA = 'uid-pod-a'
    const podB = 'uid-pod-b'
    // Deliberately the SAME reason on both pods: two different Pods hitting
    // the same failure mode are two distinct conditions on two distinct
    // resources, not one condition racing to replace the other. This is
    // also what makes the required bite-proof (drop the UID half of the
    // fold key, keep only Reason) a real falsifier -- a fixture using two
    // different Reasons would still separate by Reason alone and the
    // mutation would pass unnoticed.
    const reason = 'ImagePullBackOff'

    const timeline: AicrEvent[] = [
      ...headers,
      clusterEvent(4, 'gpu-operator', { uid: podA, reason, severity: 1, at: '2026-08-15T09:01:00Z' }, 'pod a image pull backoff'),
      clusterEvent(5, 'gpu-operator', { uid: podB, reason, severity: 2, at: '2026-08-15T09:02:00Z' }, 'pod b image pull backoff'),
      clusterEvent(6, 'gpu-operator', { uid: podA, reason, severity: 1, resolved: true, at: '2026-08-15T09:03:00Z' }, 'pod a recovered'),
      clusterEvent(7, 'gpu-operator', { uid: podB, reason, severity: 2, resolved: true, at: '2026-08-15T09:04:00Z' }, 'pod b recovered'),
      clusterEvent(8, 'gpu-operator', { uid: podA, reason, severity: 1, at: '2026-08-15T09:05:00Z' }, 'pod a image pull backoff again'),
    ]

    const rowAfter = (n: number) => deriveComponents(timeline.slice(0, n), undefined).find(c => c.name === 'gpu-operator')!

    // Step 1: only pod A's condition exists.
    const afterA = rowAfter(4)
    expect(afterA.conditions).toHaveLength(1)
    expect(activeCondition(afterA.conditions)?.uid).toBe(podA)

    // Step 2: pod B coexists alongside pod A (different UID), and the row
    // surfaces B because it is the higher severity of the two.
    const afterB = rowAfter(5)
    expect(afterB.conditions).toHaveLength(2)
    expect(new Set(afterB.conditions.map(c => c.uid))).toEqual(new Set([podA, podB]))
    expect(activeCondition(afterB.conditions)?.uid).toBe(podB)

    // Step 3: pod A resolves. Still two entries -- resolution updates A's
    // slot, it does not remove it -- and B, still unresolved, keeps showing.
    const afterAResolved = rowAfter(6)
    expect(afterAResolved.conditions).toHaveLength(2)
    expect(activeCondition(afterAResolved.conditions)?.uid).toBe(podB)

    // Step 4: pod B also resolves. Nothing unresolved remains -- the row
    // genuinely clears.
    const afterBResolved = rowAfter(7)
    expect(afterBResolved.conditions).toHaveLength(2)
    expect(activeCondition(afterBResolved.conditions)).toBeUndefined()

    // Step 5: pod A recurs on the SAME (UID, Reason). Supersedes re-arms its
    // existing slot rather than appending a new one -- still exactly two
    // entries, not three -- and the row shows it again.
    const afterRecurrence = rowAfter(8)
    expect(afterRecurrence.conditions).toHaveLength(2)
    expect(activeCondition(afterRecurrence.conditions)?.uid).toBe(podA)
    expect(activeCondition(afterRecurrence.conditions)?.resolved).toBeFalsy()
  })

  // Ruling 27 (Task 7 fix round 1, Critical 1) / Ruling 30 (Task 7 fix round
  // 2, Important 1(new)). Every fixture above holds `component` constant
  // for its whole sequence -- the blind spot the fix round 1 review named.
  // The four tests below vary `component` (same row / a later row / no
  // attribution) and `resolved` (true / false) INDEPENDENTLY across a
  // cross-component sequence -- the exact combination the fix round 2
  // review found still constant (every prior cross-component fixture set
  // `resolved: true`, so an unresolved re-observation crossing rows was
  // never exercised).
  it('a resolution attributed to a LATER action still clears the row that showed the arising condition, without moving there (Ruling 30: placement is sticky)', () => {
    const timeline: AicrEvent[] = [
      ...headers, // cert-manager x2, gpu-operator started
      clusterEvent(4, 'gpu-operator', { uid: 'uid-1', reason: 'ImagePullBackOff', severity: 2, at: '2026-08-15T09:01:00Z' }, 'pod stuck'),
      { id: 5, runId: 'run1', at: '2026-08-15T09:02:00Z', kind: 'component', level: 'info', phase: 'apply', component: 'gpu-operator', message: 'gpu-operator installed', data: { name: 'gpu-operator', status: 'installed' } },
      { id: 6, runId: 'run1', at: '2026-08-15T09:02:01Z', kind: 'component', level: 'info', phase: 'apply', component: 'kai-scheduler', message: 'installing kai-scheduler', data: { name: 'kai-scheduler', index: 3, total: 3, status: 'started' } },
      // The observer happened to see the resolution while kai-scheduler,
      // not gpu-operator, was the active action -- the whole condition is
      // the same (uid, reason) that arose on gpu-operator's watch.
      clusterEvent(7, 'kai-scheduler', { uid: 'uid-1', reason: 'ImagePullBackOff', severity: 2, resolved: true, at: '2026-08-15T09:03:00Z' }, 'pod recovered'),
    ]

    const rows = deriveComponents(timeline, undefined)
    const gpuOperator = rows.find(c => c.name === 'gpu-operator')!
    const kaiScheduler = rows.find(c => c.name === 'kai-scheduler')!

    // The row that showed it unresolved must not keep showing it: the
    // resolution superseded the SAME (uid, reason) entry.
    expect(activeCondition(gpuOperator.conditions)).toBeUndefined()
    // Ruling 30, replacing the fix round 1 assertion this same test made
    // (Ruling 31 authorizes the rewrite: that assertion pinned "placement
    // moves to the resolving event's row" as a requirement nobody had
    // actually ruled on). The entry stays on the row it was FIRST
    // attributed to -- gpu-operator, still resolved, so it doesn't render
    // there either -- and never visits kai-scheduler's row at all.
    expect(gpuOperator.conditions).toHaveLength(1)
    expect(kaiScheduler.conditions).toHaveLength(0)
  })

  it('an UNRESOLVED re-observation under a LATER action does not move the row -- the visible red line stays where it was first seen (Ruling 30)', () => {
    const timeline: AicrEvent[] = [
      ...headers,
      clusterEvent(4, 'gpu-operator', { uid: 'uid-3', reason: 'ImagePullBackOff', severity: 2, at: '2026-08-15T09:01:00Z' }, 'still stuck'),
      { id: 5, runId: 'run1', at: '2026-08-15T09:02:00Z', kind: 'component', level: 'info', phase: 'apply', component: 'gpu-operator', message: 'gpu-operator installed', data: { name: 'gpu-operator', status: 'installed' } },
      { id: 6, runId: 'run1', at: '2026-08-15T09:02:01Z', kind: 'component', level: 'info', phase: 'apply', component: 'kai-scheduler', message: 'installing kai-scheduler', data: { name: 'kai-scheduler', index: 3, total: 3, status: 'started' } },
      // The SAME pod, SAME reason, STILL unresolved -- onDaemonSet/
      // onPodChange republish on every change, not just on recovery, so
      // this re-observation under kai-scheduler is the normal path for
      // trouble that outlives its own action's install window.
      clusterEvent(7, 'kai-scheduler', { uid: 'uid-3', reason: 'ImagePullBackOff', severity: 2, resolved: false, at: '2026-08-15T09:03:00Z' }, 'still stuck, seen again'),
    ]

    const rows = deriveComponents(timeline, undefined)
    const gpuOperator = rows.find(c => c.name === 'gpu-operator')!
    const kaiScheduler = rows.find(c => c.name === 'kai-scheduler')!

    // gpu-operator keeps showing the (still current, still unresolved)
    // condition it first surfaced -- installed does not mean quiet.
    expect(activeCondition(gpuOperator.conditions)?.uid).toBe('uid-3')
    expect(activeCondition(gpuOperator.conditions)?.resolved).toBeFalsy()
    // kai-scheduler never installed this pod and must not render a false
    // red line for it -- the bug this Ruling closes.
    expect(kaiScheduler.conditions).toHaveLength(0)
  })

  it('a resolution published with NO attribution at all still clears the row that showed the arising condition', () => {
    const timeline: AicrEvent[] = [
      ...headers,
      clusterEvent(4, 'gpu-operator', { uid: 'uid-2', reason: 'CrashLoopBackOff', severity: 2, at: '2026-08-15T09:01:00Z' }, 'pod crashlooping'),
      { id: 5, runId: 'run1', at: '2026-08-15T09:02:00Z', kind: 'component', level: 'info', phase: 'apply', component: 'gpu-operator', message: 'gpu-operator installed', data: { name: 'gpu-operator', status: 'installed' } },
      // No Component: ActiveAction is empty between actions
      // (attribution.go's clearActiveAction) or after the run's terminal
      // state. The resolution must not be dropped just because there is
      // nowhere new to place it -- it still supersedes wherever the
      // condition already lives.
      clusterEvent(6, undefined, { uid: 'uid-2', reason: 'CrashLoopBackOff', severity: 2, resolved: true, at: '2026-08-15T09:03:00Z' }, 'pod recovered'),
    ]

    const rows = deriveComponents(timeline, undefined)
    const gpuOperator = rows.find(c => c.name === 'gpu-operator')!
    expect(gpuOperator.conditions).toHaveLength(1)
    expect(activeCondition(gpuOperator.conditions)).toBeUndefined()
  })

  it('an UNRESOLVED re-observation with NO attribution at all does not un-place the condition either (Ruling 30)', () => {
    const timeline: AicrEvent[] = [
      ...headers,
      clusterEvent(4, 'gpu-operator', { uid: 'uid-4', reason: 'CrashLoopBackOff', severity: 2, at: '2026-08-15T09:01:00Z' }, 'pod crashlooping'),
      { id: 5, runId: 'run1', at: '2026-08-15T09:02:00Z', kind: 'component', level: 'info', phase: 'apply', component: 'gpu-operator', message: 'gpu-operator installed', data: { name: 'gpu-operator', status: 'installed' } },
      // Still broken, still no active action, so still no Component --
      // placement must stay exactly where the first attributed sighting
      // put it, not fall back to "nowhere" just because this later event
      // has nothing to offer.
      clusterEvent(6, undefined, { uid: 'uid-4', reason: 'CrashLoopBackOff', severity: 2, resolved: false, at: '2026-08-15T09:03:00Z' }, 'pod still crashlooping'),
    ]

    const rows = deriveComponents(timeline, undefined)
    const gpuOperator = rows.find(c => c.name === 'gpu-operator')!
    expect(activeCondition(gpuOperator.conditions)?.uid).toBe('uid-4')
  })

  // Ruling 28 (Task 7 fix round 1, Important 2). engine.Retry reuses the
  // SAME RunID, so relevantTo's RunID filter cannot separate attempt 1 from
  // the retried attempt 2 -- a condition from the failed attempt would
  // otherwise describe a pod the retry may have already replaced, forever.
  it('clears tracked conditions on "run retrying", so a failed attempt does not leak into the retry', () => {
    const timeline: AicrEvent[] = [
      { id: 1, runId: 'run1', at: '2026-08-15T09:00:00Z', kind: 'component', level: 'info', phase: 'apply', component: 'gpu-operator', message: 'installing gpu-operator', data: { name: 'gpu-operator', index: 1, total: 1, status: 'started' } },
      clusterEvent(2, 'gpu-operator', { uid: 'uid-old-pod', reason: 'CrashLoopBackOff', severity: 2, at: '2026-08-15T09:00:30Z' }, 'attempt 1 pod crashlooping'),
      { id: 3, runId: 'run1', at: '2026-08-15T09:01:00Z', kind: 'component', level: 'info', phase: 'apply', component: 'gpu-operator', message: 'gpu-operator failed', data: { name: 'gpu-operator', status: 'failed', attempt: 1, maxAttempts: 2 } },
      // engine.go's Retry: bus.Event{RunID: runID, Kind: bus.KindPhase, Message: "run retrying"} -- same RunID.
      { id: 4, runId: 'run1', at: '2026-08-15T09:01:30Z', kind: 'phase', level: 'info', message: 'run retrying' },
      { id: 5, runId: 'run1', at: '2026-08-15T09:01:31Z', kind: 'component', level: 'info', phase: 'apply', component: 'gpu-operator', message: 'installing gpu-operator', data: { name: 'gpu-operator', index: 1, total: 1, status: 'started' } },
    ]

    const rows = deriveComponents(timeline, undefined)
    const gpuOperator = rows.find(c => c.name === 'gpu-operator')!
    expect(gpuOperator.conditions).toHaveLength(0)
  })

  // Minor 1 (Task 7 fix round 2, new): the retry-clear guard checks BOTH
  // Kind and Message. engine.go's runStep publishes a KindPhase event on
  // every step -- "phase started" (engine.go:591) and "phase complete"
  // (engine.go:701) -- so a guard checking `e.kind === 'phase'` alone would
  // silently wipe every row's conditions at every ordinary phase
  // transition, not just on retry.
  it('an ordinary phase event ("phase complete") does NOT clear tracked conditions -- only "run retrying" does', () => {
    const timeline: AicrEvent[] = [
      { id: 1, runId: 'run1', at: '2026-08-15T09:00:00Z', kind: 'component', level: 'info', phase: 'apply', component: 'gpu-operator', message: 'installing gpu-operator', data: { name: 'gpu-operator', index: 1, total: 1, status: 'started' } },
      clusterEvent(2, 'gpu-operator', { uid: 'uid-still-broken', reason: 'ImagePullBackOff', severity: 2, at: '2026-08-15T09:00:30Z' }, 'pod stuck'),
      // A real, unrelated KindPhase event -- same shape as engine.go's
      // "phase complete" publish, just not the retry one.
      { id: 3, runId: 'run1', at: '2026-08-15T09:01:00Z', kind: 'phase', level: 'info', phase: 'apply', message: 'phase complete' },
    ]

    const rows = deriveComponents(timeline, undefined)
    const gpuOperator = rows.find(c => c.name === 'gpu-operator')!
    expect(activeCondition(gpuOperator.conditions)?.uid).toBe('uid-still-broken')
  })
})

describe('clusterConditionSupersedes', () => {
  function cond(overrides: Partial<ClusterCondition>): ClusterCondition {
    return {
      kind: 'Pod', namespace: 'gpu-operator', name: 'a-pod', uid: 'uid-1', reason: 'ImagePullBackOff',
      severity: 1, resolved: false, at: '2026-08-15T09:00:00Z', message: 'm', ...overrides,
    }
  }

  // Important 1 (Task 7 fix round 1): an unconditional `return true` after
  // the UID/Reason guard left the suite fully green, because nothing below
  // that guard had a falsifier. Each test here pins exactly one branch.

  it('never supersedes across a different UID or a different Reason, however much later or more severe', () => {
    const prev = cond({ uid: 'uid-1', reason: 'ImagePullBackOff', at: '2026-08-15T09:00:00Z' })
    expect(clusterConditionSupersedes(cond({ uid: 'uid-2', reason: 'ImagePullBackOff', severity: 2, resolved: true, at: '2026-08-15T09:05:00Z' }), prev)).toBe(false)
    expect(clusterConditionSupersedes(cond({ uid: 'uid-1', reason: 'CrashLoopBackOff', severity: 2, resolved: true, at: '2026-08-15T09:05:00Z' }), prev)).toBe(false)
  })

  it('At is primary: a later event supersedes even with lower severity and unresolved', () => {
    const prev = cond({ severity: 2, resolved: true, at: '2026-08-15T09:00:00Z' })
    const next = cond({ severity: 0, resolved: false, at: '2026-08-15T09:01:00Z' })
    expect(clusterConditionSupersedes(next, prev)).toBe(true)
  })

  it('an earlier event never supersedes, even if resolved and higher severity', () => {
    const prev = cond({ severity: 0, resolved: false, at: '2026-08-15T09:01:00Z' })
    const stale = cond({ severity: 2, resolved: true, at: '2026-08-15T09:00:00Z' })
    expect(clusterConditionSupersedes(stale, prev)).toBe(false)
  })

  it('a same-At tie breaks on Resolved first, whatever Severity says', () => {
    const at = '2026-08-15T09:00:00Z'
    const unresolvedHighSeverity = cond({ severity: 2, resolved: false, at })
    const resolvedLowSeverity = cond({ severity: 0, resolved: true, at })
    expect(clusterConditionSupersedes(resolvedLowSeverity, unresolvedHighSeverity)).toBe(true)
    expect(clusterConditionSupersedes(unresolvedHighSeverity, resolvedLowSeverity)).toBe(false)
  })

  it('a same-At, same-Resolved tie breaks on Severity', () => {
    const at = '2026-08-15T09:00:00Z'
    const low = cond({ severity: 0, resolved: false, at })
    const high = cond({ severity: 2, resolved: false, at })
    expect(clusterConditionSupersedes(high, low)).toBe(true)
    expect(clusterConditionSupersedes(low, high)).toBe(false)
  })

  // Minor 1 (Task 7 fix round 1): Date.parse truncates Go's nanosecond At to
  // milliseconds -- and a malformed At produced NaN, which is never greater
  // than anything, pinning the entry permanently.

  it('orders by At down to full nanosecond precision, which Date.parse would truncate away', () => {
    const prev = cond({ ready: 3, at: '2026-08-15T09:00:00.000000001Z' })
    const next = cond({ ready: 4, at: '2026-08-15T09:00:00.000000900Z' })
    expect(clusterConditionSupersedes(next, prev)).toBe(true)
    expect(clusterConditionSupersedes(prev, next)).toBe(false)
  })

  it('a malformed At falls through to the Resolved/Severity tie-break instead of blocking supersession outright (Minor 1, fix round 1 -- no NaN comparison)', () => {
    const malformed = cond({ at: 'not-a-real-timestamp', severity: 0, resolved: false })
    const alsoMalformed = cond({ at: 'also-not-a-real-timestamp', severity: 0, resolved: true })
    // Date.parse(malformed) was NaN, and `NaN !== x` is true while
    // `NaN > x` is false -- so the OLD code took the At branch and
    // returned false in BOTH directions, regardless of Resolved or
    // Severity, permanently blocking supersession. compareAt returns 0 for
    // an unparseable value instead, which falls through to the tie-break
    // below it -- so Resolved can still decide the outcome.
    expect(clusterConditionSupersedes(alsoMalformed, malformed)).toBe(true)
    expect(clusterConditionSupersedes(malformed, alsoMalformed)).toBe(false)
  })
})

describe('compareAt', () => {
  // Ruling 33 (Task 7 fix round 2, Important 3(new)). Go's time.Time
  // marshals as RFC3339Nano -- VARIABLE width: trailing zero fraction
  // digits are trimmed, and a whole second drops the fraction entirely.
  // Plain string comparison (the fix round 1 shape) gets this wrong
  // because 'Z' (0x5A) sorts above '.' (0x2E): a whole second ("...:00Z")
  // reads as LATER than any fraction within that same second
  // ("...:00.5Z"), which is backwards. These three tests are the three
  // widths Go's marshaler actually produces -- no fraction, a trimmed
  // fraction, and full nanosecond precision -- compared pairwise so the
  // property holds in general, not just within one width.

  it('a whole second (no fraction) is EARLIER than any fraction within that same second, not later', () => {
    // The exact end-to-end symptom: an ImagePullBackOff arising at
    // 09:01:00Z and resolving at 09:01:00.5Z left the row stuck forever,
    // because plain string comparison ranked "...:00Z" above "...:00.5Z".
    expect(compareAt('2026-08-15T09:01:00Z', '2026-08-15T09:01:00.5Z')).toBeLessThan(0)
    expect(compareAt('2026-08-15T09:01:00.5Z', '2026-08-15T09:01:00Z')).toBeGreaterThan(0)
  })

  it('a trimmed (short) fraction and a full 9-digit fraction compare correctly by true magnitude, not by string length', () => {
    // 500,000,000ns ("...5Z", trimmed) vs 500,000,001ns
    // ("...500000001Z", untrimmable) -- the latter is 1ns later, and "5" is
    // a strict PREFIX of "500000001". No padding needed for this to work
    // (M-2, Task 7 final fix wave -- the previous version of compareAt
    // right-padded both fractions before comparing, on a claim this test
    // now disproves rather than merely stops depending on): RFC3339Nano
    // trims trailing zeros, so "500000001" -- the longer string -- can
    // never be all zeros past "5"'s length; it is GUARANTEED to carry a
    // nonzero digit somewhere in there (its own final digit, at minimum),
    // which is exactly what makes the shorter prefix the smaller value.
    // JS's native string ordering already ranks a strict prefix as
    // smaller, so raw comparison gets this right on its own.
    expect(compareAt('2026-08-15T09:01:00.5Z', '2026-08-15T09:01:00.500000001Z')).toBeLessThan(0)
    expect(compareAt('2026-08-15T09:01:00.500000001Z', '2026-08-15T09:01:00.5Z')).toBeGreaterThan(0)
  })

  it('two full nanosecond-precision fractions order correctly down to the last digit', () => {
    expect(compareAt('2026-08-15T09:00:00.000000001Z', '2026-08-15T09:00:00.000000900Z')).toBeLessThan(0)
    expect(compareAt('2026-08-15T09:00:00.000000900Z', '2026-08-15T09:00:00.000000001Z')).toBeGreaterThan(0)
  })

  it('a differing whole-second prefix decides the order before any fraction is considered', () => {
    expect(compareAt('2026-08-15T09:00:00.999999999Z', '2026-08-15T09:00:01Z')).toBeLessThan(0)
  })

  it('returns 0 (not NaN, not a throw) for a value that does not match RFC3339 UTC', () => {
    expect(compareAt('not-a-real-timestamp', '2026-08-15T09:00:00Z')).toBe(0)
    expect(compareAt('2026-08-15T09:00:00Z', 'not-a-real-timestamp')).toBe(0)
  })
})

describe('activeCondition', () => {
  function condition(overrides: Partial<ClusterCondition>): ClusterCondition {
    return {
      kind: 'Pod', namespace: 'gpu-operator', name: 'a-pod', uid: 'uid-1', reason: 'Warning',
      severity: 0, resolved: false, at: '2026-08-15T09:00:00Z', message: 'm', ...overrides,
    }
  }

  it('picks the highest-severity unresolved condition', () => {
    const warn = condition({ uid: 'uid-1', reason: 'Warning', severity: 1 })
    const error = condition({ uid: 'uid-2', reason: 'FailedScheduling', severity: 2 })
    expect(activeCondition([warn, error])?.reason).toBe('FailedScheduling')
    expect(activeCondition([error, warn])?.reason).toBe('FailedScheduling')
  })

  it('returns undefined once every condition on the row is resolved', () => {
    const resolved = condition({ severity: 2, resolved: true })
    expect(activeCondition([resolved])).toBeUndefined()
  })

  // Minor 4 (Task 7 fix round 1): array order reflects Map insertion order
  // -- when a (UID, Reason) FIRST arose, not its most recent update -- so a
  // naive "first same-severity match wins" would pin the earliest-seen
  // condition forever, however stale, over one that just recurred.
  it('on an exact severity tie, prefers the later At, not the first-seen entry', () => {
    const older = condition({ uid: 'uid-old', reason: 'CrashLoopBackOff', severity: 2, at: '2026-08-15T09:01:00Z' })
    const newer = condition({ uid: 'uid-new', reason: 'ImagePullBackOff', severity: 2, at: '2026-08-15T09:09:00Z' })
    expect(activeCondition([older, newer])?.uid).toBe('uid-new')
    expect(activeCondition([newer, older])?.uid).toBe('uid-new')
  })
})
