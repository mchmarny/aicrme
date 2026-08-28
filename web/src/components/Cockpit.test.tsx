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

  // The confirm gate is the one screen whose entire job is honesty: it is
  // where the operator approves a cluster-wide install. It used to say
  // "every version pinned and signed", and the second half was not backed by
  // anything. aicr.ComponentRef -- and recipe.ComponentRef beneath it --
  // carry Name/Kind/Version/Source/Chart/Namespace and no digest or
  // signature; steps.Bundle passes no Attester, so the bundle is not
  // attested either; and AICR's only attestation path
  // (Client.EmitRecipeEvidence) needs a completed ValidateState run, which
  // this console deliberately does not do. "Pinned" is true and stays.
  it('gate: claims versions are pinned, and does not claim they are signed', () => {
    const run = baseRun({ state: 'awaiting_decision', phase: 'apply' })
    render(<Cockpit events={[]} run={run} onDecide={vi.fn()} onRetry={vi.fn()} />)

    expect(screen.getByText(/every version pinned/)).toBeDefined()
    expect(screen.queryByText(/signed/i)).toBeNull()
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

  // AGGREGATE PROGRESS. The screen listed every component's status and never
  // summed them, so sixteen minutes into an install there was no way to tell
  // minute 3 from minute 13. The counts it did show -- "14 components, 16
  // deployment actions" -- are both denominators with no numerator.
  it('running: says how many of the deployment actions are done', () => {
    const events: AicrEvent[] = [
      componentEvent(1, 'cert-manager', { index: 1, total: 4, status: 'started' }),
      componentEvent(2, 'cert-manager', { status: 'installed' }),
      componentEvent(3, 'nfd', { index: 2, total: 4, status: 'started' }),
      componentEvent(4, 'nfd', { status: 'installed' }),
      componentEvent(5, 'gpu-operator', { index: 3, total: 4, status: 'started' }),
    ]
    render(<Cockpit events={events} run={baseRun({ state: 'running' })} onDecide={vi.fn()} onRetry={vi.fn()} />)

    expect(screen.getByTestId('cockpit-progress').textContent).toMatch(/2 of 4/)
  })

  // The in-flight row has to be findable at a glance. Every finished row
  // carried the identical word INSTALLED, so the one row that differed was
  // styled exactly like the eleven that did not.
  it('running: marks the component actually in flight', () => {
    const events: AicrEvent[] = [
      componentEvent(1, 'cert-manager', { index: 1, total: 2, status: 'started' }),
      componentEvent(2, 'cert-manager', { status: 'installed' }),
      componentEvent(3, 'gpu-operator', { index: 2, total: 2, status: 'started' }),
    ]
    render(<Cockpit events={events} run={baseRun({ state: 'running' })} onDecide={vi.fn()} onRetry={vi.fn()} />)

    expect(screen.getByTestId('active-gpu-operator')).toBeDefined()
    expect(screen.queryByTestId('active-cert-manager')).toBeNull()
    // The finished row still says so for a screen reader, even though sighted
    // users get a glyph instead of an eleventh copy of the word.
    expect(screen.getByTestId('component-cert-manager').textContent).toMatch(/installed/i)
  })

  // Durations were already on every row -- ComponentState carries startedAt
  // and endedAt, with a comment saying they exist "so the UI can show elapsed
  // time" -- and nothing rendered them.
  it('running: shows how long each component took', () => {
    const events: AicrEvent[] = [
      { ...componentEvent(1, 'cert-manager', { index: 1, total: 2, status: 'started' }), at: '2026-08-28T13:00:00Z' },
      { ...componentEvent(2, 'cert-manager', { status: 'installed' }), at: '2026-08-28T13:02:09Z' },
    ]
    render(<Cockpit events={events} run={baseRun({ state: 'running' })} onDecide={vi.fn()} onRetry={vi.fn()} />)

    expect(screen.getByTestId('component-cert-manager').textContent).toMatch(/2m 9s/)
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

  // Ruling 38 (Task 7 final fix wave, replacing fix round 2's tense-only
  // fix). Ruling 27's stated intent is that a still-open condition survives
  // to the Done screen -- an operator seeing "installed successfully" needs
  // to still see the one row that isn't actually clean. But teardown means
  // the console can no longer claim this is CURRENT, only that it was the
  // last thing observed -- so the caption must say "last observed", not
  // just switch to past tense.
  it('done: a still-open condition survives to the Done screen, labeled as last observed rather than current', () => {
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
    expect(gpuRow.textContent).toMatch(/last observed while gpu-operator installed\)/i)
    expect(gpuRow.textContent).not.toMatch(/while gpu-operator installs\)/i)
  })

  // Teardown happens at EITHER terminal state, not just StateDone -- Failed
  // must carry the same "last observed" discipline, not the live present
  // tense a mid-run screen would use. Pre-merge fix wave: "installed" is
  // wrong here specifically -- the Failed screen's own heading says
  // "Install failed", and "(last observed while gpu-operator installed)"
  // sitting on a failed run reads as "it installed successfully", the
  // opposite claim. "was installing" keeps the same discipline without it.
  it('failed: a still-open condition on the Failed screen is labeled last observed, past continuous -- never a success claim', () => {
    const events: AicrEvent[] = [
      componentEvent(1, 'gpu-operator', { index: 1, total: 2, status: 'started' }),
      componentEvent(2, 'gpu-operator', { status: 'installed' }),
      {
        id: 3, runId: 'run1', at: '2026-08-15T09:03:00Z', kind: 'cluster', level: 'info', phase: 'apply',
        component: 'gpu-operator', message: 'gpu-operator/nvidia-driver-daemonset-abc ImagePullBackOff',
        data: {
          kind: 'Pod', namespace: 'gpu-operator', name: 'nvidia-driver-daemonset-abc', uid: 'uid-1',
          reason: 'ImagePullBackOff', severity: 2, resolved: false, at: '2026-08-15T09:03:00Z',
        },
      },
      componentEvent(4, 'kai-scheduler', { index: 2, total: 2, status: 'failed', attempt: 1 }),
    ]
    const run = baseRun({ state: 'failed', error: 'bundle apply failed: exit status 1' })
    render(<Cockpit events={events} run={run} onDecide={vi.fn()} onRetry={vi.fn()} />)

    const gpuRow = screen.getByTestId('component-gpu-operator')
    expect(gpuRow.textContent).toMatch(/last observed while gpu-operator was installing\)/i)
    expect(gpuRow.textContent).not.toMatch(/gpu-operator installed\)/i)
  })
})
