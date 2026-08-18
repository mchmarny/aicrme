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
    render(<ComponentConditions name="gpu-operator" conditions={[warn, error]} />)

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
    render(<ComponentConditions name="kai-scheduler" conditions={[error]} />)
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

  it('renders past tense when tense="past" (Minor 2, fix round 2 -- the Done screen)', () => {
    const error = condition({ reason: 'ImagePullBackOff' })
    render(<ComponentConditions name="gpu-operator" conditions={[error]} tense="past" />)
    const el = screen.getByTestId('condition-gpu-operator')
    expect(el.textContent).toMatch(/while gpu-operator installed\)/i)
    expect(el.textContent).not.toMatch(/while gpu-operator installs/i)
  })

  it('defaults to present tense when tense is omitted', () => {
    const error = condition({ reason: 'ImagePullBackOff' })
    render(<ComponentConditions name="gpu-operator" conditions={[error]} />)
    const el = screen.getByTestId('condition-gpu-operator')
    expect(el.textContent).toMatch(/while gpu-operator installs\)/i)
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
