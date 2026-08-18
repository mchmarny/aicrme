import { describe, expect, it } from 'vitest'
import { activeCondition, deriveComponents, deriveFailure, deploymentActionsTotal, type ClusterCondition } from './pipeline'
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
})
