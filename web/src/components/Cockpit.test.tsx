import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { Cockpit } from './Cockpit'
import type { RunState } from './Wizard'
import type { AicrEvent } from '../useEvents'

const recipe = {
  name: 'criteria(...)',
  version: 'dev',
  componentCount: 4,
  components: [
    { name: 'cert-manager', kind: 'Helm', version: 'v1.20.2', namespace: 'cert-manager' },
    { name: 'gpu-operator', kind: 'Helm', version: 'v26.3.3', namespace: 'gpu-operator' },
    { name: 'kai-scheduler', kind: 'Helm', version: 'v0.14.1', namespace: 'kai-scheduler' },
    { name: 'kubeflow-trainer', kind: 'Helm', version: '2.2.0', namespace: 'kubeflow' },
  ],
}

function componentEvent(id: number, name: string, data: Record<string, unknown>): AicrEvent {
  return {
    id, runId: 'run1', at: `2026-08-15T09:0${id}:00Z`, kind: 'component', level: 'info',
    phase: 'apply', component: name, message: `${name} ${data.status}`, data: { name, ...data },
  }
}

function baseRun(overrides: Partial<RunState>): RunState {
  return { runId: 'run1', phase: 'apply', state: 'running', report: null, recipe, ...overrides }
}

describe('Cockpit', () => {
  it('gate: renders the recipe components, a bundle download link, and sends {apply: yes} on Install', () => {
    const onDecide = vi.fn()
    const run = baseRun({ state: 'awaiting_decision', phase: 'apply' })
    render(<Cockpit events={[]} run={run} onDecide={onDecide} onRetry={vi.fn()} />)

    for (const c of recipe.components) {
      expect(screen.getByText(new RegExp(c.name))).toBeDefined()
    }

    const link = screen.getByRole('link', { name: /download bundle/i })
    expect(link.getAttribute('href')).toBe('/api/runs/run1/bundle')

    fireEvent.click(screen.getByRole('button', { name: /install/i }))
    expect(onDecide).toHaveBeenCalledWith({ apply: 'yes' })
  })

  it('running: renders component rows with status, and a retrying component shows its attempt count', () => {
    const events: AicrEvent[] = [
      componentEvent(1, 'cert-manager', { index: 1, total: 4, status: 'started' }),
      componentEvent(2, 'cert-manager', { status: 'installed' }),
      componentEvent(3, 'kai-scheduler', { index: 2, total: 4, status: 'started' }),
      componentEvent(4, 'kai-scheduler', { status: 'retrying', attempt: 1, maxAttempts: 2, retryInSeconds: 5 }),
    ]
    const run = baseRun({ state: 'running' })
    render(<Cockpit events={events} run={run} onDecide={vi.fn()} onRetry={vi.fn()} />)

    expect(screen.getByTestId('component-cert-manager').textContent).toMatch(/installed/i)
    const kaiRow = screen.getByTestId('component-kai-scheduler')
    expect(kaiRow.textContent).toMatch(/retrying/i)
    expect(kaiRow.textContent).toContain('1/2')
  })

  it('slow-step callout: an active gpu-operator renders its note; an active cert-manager renders none', () => {
    const events: AicrEvent[] = [
      componentEvent(1, 'cert-manager', { index: 1, total: 4, status: 'started' }),
      componentEvent(2, 'gpu-operator', { index: 2, total: 4, status: 'started' }),
    ]
    const run = baseRun({ state: 'running' })
    render(<Cockpit events={events} run={run} onDecide={vi.fn()} onRetry={vi.fn()} />)

    expect(screen.getByTestId('component-gpu-operator').textContent).toMatch(/driver DaemonSet compiles/)
    expect(screen.getByTestId('component-cert-manager').textContent).not.toMatch(/driver DaemonSet compiles/)
  })

  it('renders an attributed cluster condition inside its own row and nowhere else', () => {
    const events: AicrEvent[] = [
      componentEvent(1, 'cert-manager', { index: 1, total: 4, status: 'started' }),
      componentEvent(2, 'gpu-operator', { index: 2, total: 4, status: 'started' }),
      {
        id: 3, runId: 'run1', at: '2026-08-15T09:03:00Z', kind: 'cluster', level: 'info', phase: 'apply',
        component: 'gpu-operator', message: 'gpu-operator/nvidia-driver-daemonset-abc ImagePullBackOff',
        data: {
          kind: 'Pod', namespace: 'gpu-operator', name: 'nvidia-driver-daemonset-abc', uid: 'uid-1',
          reason: 'ImagePullBackOff', severity: 2, resolved: false, at: '2026-08-15T09:03:00Z',
        },
      },
    ]
    const run = baseRun({ state: 'running' })
    render(<Cockpit events={events} run={run} onDecide={vi.fn()} onRetry={vi.fn()} />)

    const gpuRow = screen.getByTestId('component-gpu-operator')
    expect(gpuRow.textContent).toMatch(/ImagePullBackOff/)
    expect(gpuRow.textContent).toMatch(/while gpu-operator installs/i)
    expect(screen.getByTestId('component-cert-manager').textContent).not.toMatch(/ImagePullBackOff/)
  })

  it('failed: renders the failing component, exit error, and tail, and Retry calls onRetry', () => {
    const events: AicrEvent[] = [
      componentEvent(1, 'cert-manager', { index: 1, total: 4, status: 'started' }),
      componentEvent(2, 'cert-manager', { status: 'installed' }),
      componentEvent(3, 'kai-scheduler', { index: 2, total: 4, status: 'started' }),
      componentEvent(4, 'kai-scheduler', { status: 'retrying', attempt: 1, maxAttempts: 2, retryInSeconds: 5 }),
      componentEvent(5, 'kai-scheduler', { status: 'failed', attempt: 2 }),
      {
        id: 6, runId: 'run1', at: '2026-08-15T09:06:00Z', kind: 'error', level: 'error', phase: 'apply',
        component: 'kai-scheduler', message: 'deploy.sh failed: exit status 1',
        data: {
          component: 'kai-scheduler',
          exitError: 'exit status 1',
          tail: ['└─ ✗ kai-scheduler FAILED (after 2 attempts)', 'Error: INSTALLATION FAILED: timed out'],
        },
      },
    ]
    const onRetry = vi.fn()
    const run = baseRun({ state: 'failed', error: 'bundle apply failed: exit status 1' })
    render(<Cockpit events={events} run={run} onDecide={vi.fn()} onRetry={onRetry} />)

    expect(screen.getAllByText(/kai-scheduler/).length).toBeGreaterThan(0)
    expect(screen.getByText('exit status 1')).toBeDefined()
    const tail = screen.getByTestId('failure-tail')
    expect(tail.textContent).toContain('kai-scheduler FAILED')
    expect(tail.textContent).toContain('timed out')

    fireEvent.click(screen.getByRole('button', { name: /retry/i }))
    expect(onRetry).toHaveBeenCalled()
  })

  it('done: renders a success line and no Retry button', () => {
    const events: AicrEvent[] = [
      componentEvent(1, 'cert-manager', { index: 1, total: 1, status: 'started' }),
      componentEvent(2, 'cert-manager', { status: 'installed' }),
    ]
    const run = baseRun({ state: 'done' })
    render(<Cockpit events={events} run={run} onDecide={vi.fn()} onRetry={vi.fn()} />)

    expect(screen.getByTestId('cockpit-success')).toBeDefined()
    expect(screen.queryByRole('button', { name: /retry/i })).toBeNull()
  })

  // Minor 2 (Task 7 fix round 2, new): Ruling 27's stated intent is that a
  // still-open condition survives to the Done screen -- an operator seeing
  // "installed successfully" needs to still see the one row that isn't
  // actually clean. The caption's tense must match: the run is over, so
  // "installed", not "installs".
  it('done: a still-open condition survives to the Done screen in past tense', () => {
    const events: AicrEvent[] = [
      componentEvent(1, 'gpu-operator', { index: 1, total: 1, status: 'started' }),
      componentEvent(2, 'gpu-operator', { status: 'installed' }),
      {
        id: 3, runId: 'run1', at: '2026-08-15T09:03:00Z', kind: 'cluster', level: 'info', phase: 'apply',
        component: 'gpu-operator', message: 'gpu-operator/nvidia-driver-daemonset-abc ImagePullBackOff',
        data: {
          kind: 'Pod', namespace: 'gpu-operator', name: 'nvidia-driver-daemonset-abc', uid: 'uid-1',
          reason: 'ImagePullBackOff', severity: 2, resolved: false, at: '2026-08-15T09:03:00Z',
        },
      },
    ]
    const run = baseRun({ state: 'done' })
    render(<Cockpit events={events} run={run} onDecide={vi.fn()} onRetry={vi.fn()} />)

    const gpuRow = screen.getByTestId('component-gpu-operator')
    expect(gpuRow.textContent).toMatch(/ImagePullBackOff/)
    expect(gpuRow.textContent).toMatch(/while gpu-operator installed\)/i)
    expect(gpuRow.textContent).not.toMatch(/while gpu-operator installs/i)
  })
})
