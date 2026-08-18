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
    // copy must read as a temporal correlation ("while installing"), never
    // as a claim that gpu-operator owns or caused the condition. deploy.sh
    // itself warns that cluster convergence continues asynchronously past
    // --wait returning (deploy.sh.tmpl:488-492), so "caused by" would be a
    // false claim this component has no basis to make.
    expect(el.textContent).toMatch(/while installing/i)
    expect(el.textContent).not.toMatch(/caused by|owns|responsible for|because of/i)
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
