import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { Discover, type CapabilityReport } from './Discover'

const report: CapabilityReport = {
  headline: 'This is an EKS cluster with 64 H100 GPUs.',
  detail: '8 x p5.48xlarge, H100 SXM 80GB, EFA fabric, Kubernetes 1.33, Ubuntu 22.04',
  punchline: '0 of 64 GPUs are usable by a workload today.',
  usableGpus: 0,
  totalGpus: 64,
  analyzed: true,
  simulated: false,
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

  // gap.Report.Gaps carries no `omitempty` (internal/gap/gap.go:26), so Go
  // marshals it as JSON null whenever no rule fires -- confirmed for real by
  // internal/gap.TestAnalyzeFullyCapableClusterHasNoGaps -- which is exactly
  // the product's own end state: a cluster with everything already
  // installed. This must render a positive capability statement, not crash.
  it('renders a positive statement instead of crashing when the cluster has no gaps to close', () => {
    const fullyCapable: CapabilityReport = {
      headline: 'This is an eks cluster with 8 GPUs.',
      punchline: '8 of 8 GPUs are usable by a workload today.',
      usableGpus: 8,
      totalGpus: 8,
      analyzed: true,
      simulated: false,
      gaps: null as unknown as CapabilityReport['gaps'],
    }
    render(<Discover report={fullyCapable} />)
    expect(screen.queryAllByTestId(/^gap-/)).toHaveLength(0)
    expect(screen.getByTestId('no-gaps').textContent).toMatch(/already|nothing left|no gaps/i)
    expect(screen.getByTestId('punchline').textContent).toBe('8 of 8 GPUs are usable by a workload today.')
  })

  it('renders a positive statement for an empty (non-null) gaps array too', () => {
    render(<Discover report={{ ...report, gaps: [] }} />)
    expect(screen.getByTestId('no-gaps')).toBeDefined()
  })

  // gap.Report.Analyzed distinguishes "every capability already present"
  // from "nothing measured at all" -- both produce zero gaps, but only the
  // first earns the green already-capable copy.
  it('renders a caveat, not a congratulation, when no snapshot was ever analyzed', () => {
    const unmeasured: CapabilityReport = {
      headline: 'No cluster snapshot available.',
      punchline: "Run Discover to capture the cluster's current state.",
      usableGpus: 0,
      totalGpus: 0,
      analyzed: false,
      simulated: false,
      gaps: null as unknown as CapabilityReport['gaps'],
    }
    render(<Discover report={unmeasured} />)
    expect(screen.getByTestId('no-snapshot').textContent).toMatch(/not.*clean bill of health|nothing has been measured/i)
    expect(screen.queryByTestId('no-gaps')).toBeNull()
    expect(screen.queryAllByTestId(/^gap-/)).toHaveLength(0)
  })
})
