import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { Discover, type CapabilityReport } from './Discover'

const report: CapabilityReport = {
  headline: 'This is an EKS cluster with 64 H100 GPUs.',
  detail: '8 x p5.48xlarge, H100 SXM 80GB, EFA fabric, Kubernetes 1.33, Ubuntu 22.04',
  punchline: '0 of 64 GPUs are usable by a workload today.',
  usableGpus: 0,
  totalGpus: 64,
  gaps: [
    { id: 'gpu-driver', title: 'No GPU driver installed, the kernel does not see the devices', component: 'gpu-operator' },
    { id: 'device-plugin', title: 'No device plugin, Kubernetes cannot schedule nvidia.com/gpu', component: 'gpu-operator' },
  ],
}

describe('Discover', () => {
  it('opens with the capability statement, not an inventory', () => {
    render(<Discover report={report} />)
    expect(screen.getByRole('heading', { name: /EKS cluster with 64 H100 GPUs/ })).toBeDefined()
  })

  it('lists every gap', () => {
    render(<Discover report={report} />)
    expect(screen.getAllByTestId(/^gap-/)).toHaveLength(2)
  })

  it('names the component that closes each gap so this screen pre-explains the next', () => {
    render(<Discover report={report} />)
    expect(screen.getByTestId('gap-gpu-driver').textContent).toContain('gpu-operator')
  })

  it('lands on the number the finale pays off', () => {
    render(<Discover report={report} />)
    expect(screen.getByTestId('punchline').textContent).toBe('0 of 64 GPUs are usable by a workload today.')
  })
})
