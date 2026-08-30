import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { ComponentConditions } from './ComponentConditions'
import type { ClusterCondition } from '../pipeline'

function condition(overrides: Partial<ClusterCondition>): ClusterCondition {
  return {
    kind: 'Pod', namespace: 'gpu-operator', name: 'nvidia-driver-daemonset-abc', uid: 'uid-1',
    reason: 'ImagePullBackOff', severity: 2, resolved: false, at: '2026-08-15T09:00:00Z',
    message: 'gpu-operator/nvidia-driver-daemonset-abc ImagePullBackOff', ...overrides,
  }
}

describe('ComponentConditions', () => {
  it("shows the highest-severity condition's reason and a temporal caption, never an ownership claim", () => {
    const warn = condition({ uid: 'uid-warn', reason: 'FailedScheduling', severity: 1 })
    const error = condition({ uid: 'uid-error', reason: 'ImagePullBackOff', severity: 2 })
    render(<ComponentConditions name="gpu-operator" conditions={[warn, error]} installing />)

    const el = screen.getByTestId('condition-gpu-operator')
    expect(el.textContent).toContain('ImagePullBackOff')
    expect(el.textContent).not.toContain('FailedScheduling')

    // The one property a reviewer cannot verify by reading behavior: the
    // copy must read as a temporal correlation ("while <action> installs"),
    // never as a claim that gpu-operator owns or caused the condition.
    // deploy.sh itself warns that cluster convergence continues
    // asynchronously past --wait returning (deploy.sh.tmpl:488-492), so
    // "caused by" would be a false claim this component has no basis to
    // make. The subject (the row's own action) must be named -- see the
    // next test -- so this asserts the exact phrase, not just "while
    // installing" in isolation.
    expect(el.textContent).toMatch(/while gpu-operator installs/i)
    expect(el.textContent).not.toMatch(/caused by|owns|responsible for|because of/i)
  })

  it('names the row itself, not just "installing" in the abstract (Minor 2, fix round 1)', () => {
    const error = condition({ reason: 'FailedScheduling' })
    render(<ComponentConditions name="kai-scheduler" conditions={[error]} installing />)
    const el = screen.getByTestId('condition-kai-scheduler')
    expect(el.textContent).toMatch(/while kai-scheduler installs/i)
  })

  it('does not print the reason twice when the message already carries it (Minor 3, fix round 1)', () => {
    // pods.go's podMessage format: "<namespace>/<name>: <raw reason>".
    const podCondition = condition({ reason: 'ImagePullBackOff', message: 'gpu-operator/nvidia-driver-daemonset-abc: ImagePullBackOff' })
    render(<ComponentConditions name="gpu-operator" conditions={[podCondition]} />)
    const el = screen.getByTestId('condition-gpu-operator')
    expect(el.textContent?.match(/ImagePullBackOff/g)).toHaveLength(1)
  })

  it('still shows the reason when the message does not carry it, e.g. a rollout readiness count', () => {
    const rollout = condition({ reason: 'RolloutProgress', message: 'gpu-operator/nvidia-driver-daemonset 3/8 ready' })
    render(<ComponentConditions name="gpu-operator" conditions={[rollout]} />)
    const el = screen.getByTestId('condition-gpu-operator')
    expect(el.textContent).toContain('RolloutProgress')
    expect(el.textContent).toContain('3/8 ready')
  })

  // Ruling 38 (Task 7 final fix wave): after a terminal state, the observer
  // has torn down and nothing can ever publish a resolution again -- the
  // console knows "this was true when we stopped watching," not "this is
  // true." The label must say so, not just switch verb tense: this is the
  // one property a reviewer cannot verify by reading behavior, the same
  // class as the temporal-not-ownership assertion above, so it's pinned as
  // a literal string here too.
  //
  // Pre-merge fix wave: Done and Failed are asserted SEPARATELY, not as one
  // "terminal" case, because the wording differs by which one -- see
  // ComponentConditions.tsx's doc comment on `terminalState`.
  it('labels a Done-state condition "last observed ... installed" -- a true, completed claim', () => {
    const error = condition({ reason: 'ImagePullBackOff' })
    render(<ComponentConditions name="gpu-operator" conditions={[error]} terminalState="done" />)
    const el = screen.getByTestId('condition-gpu-operator')
    expect(el.textContent).toMatch(/last observed while gpu-operator installed\)/i)
    expect(el.textContent).not.toMatch(/while gpu-operator installs\)/i)
    expect(el.textContent).not.toMatch(/cluster activity/i)
  })

  // "installed" on the Failed screen would read as "it installed
  // successfully" -- the opposite of what that screen means -- so Failed
  // gets the past-continuous "was installing" instead of Done's past-simple
  // "installed". Same "last observed, not current" discipline, no success
  // claim.
  it('labels a Failed-state condition "last observed ... was installing" -- no success claim', () => {
    const error = condition({ reason: 'ImagePullBackOff' })
    render(<ComponentConditions name="kai-scheduler" conditions={[error]} terminalState="failed" />)
    const el = screen.getByTestId('condition-kai-scheduler')
    expect(el.textContent).toMatch(/last observed while kai-scheduler was installing\)/i)
    expect(el.textContent).not.toMatch(/kai-scheduler installed\)/i)
    expect(el.textContent).not.toMatch(/cluster activity/i)
  })

  it('defaults to the live, present-tense caption when terminalState is omitted', () => {
    const error = condition({ reason: 'ImagePullBackOff' })
    render(<ComponentConditions name="gpu-operator" conditions={[error]} installing />)
    const el = screen.getByTestId('condition-gpu-operator')
    expect(el.textContent).toMatch(/cluster activity while gpu-operator installs\)/i)
    expect(el.textContent).not.toMatch(/last observed/i)
  })

  it('renders nothing once every condition on the row has resolved', () => {
    const resolved = condition({ resolved: true })
    render(<ComponentConditions name="gpu-operator" conditions={[resolved]} />)
    expect(screen.queryByTestId('condition-gpu-operator')).toBeNull()
  })

  it('renders nothing for a row with no conditions at all', () => {
    render(<ComponentConditions name="cert-manager" conditions={[]} />)
    expect(screen.queryByTestId('condition-cert-manager')).toBeNull()
  })
})

// The bug this distinction exists for. A condition that recurs after its
// component has finished -- during Validate, which is still a "running" run --
// used to be captioned "cluster activity while nodewright-operator installs",
// naming an install that ended minutes earlier. Observed on real H100s
// 2026-08-30. The tense follows the COMPONENT's own action, not the run's.
it('does not claim a finished component is still installing', () => {
  const error = condition({
    uid: 'u-9', reason: 'Unhealthy', severity: 2, at: '2026-08-30T13:55:18Z',
    message: 'skyhook/skyhook-operator-controller-manager: Readiness probe failed',
  })

  render(<ComponentConditions name="nodewright-operator" conditions={[error]} />)

  const text = screen.getByTestId('condition-nodewright-operator').textContent ?? ''
  expect(text).not.toMatch(/installs/)
  expect(text).toMatch(/while nodewright-operator installed/)
})
