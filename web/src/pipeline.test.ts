import { describe, expect, it } from 'vitest'
import { deriveComponents, deriveFailure, deploymentActionsTotal } from './pipeline'
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
