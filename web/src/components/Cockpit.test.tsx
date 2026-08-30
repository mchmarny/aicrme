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
  // Cockpit renders for both bundle and apply, and before the first component
  // reports there is nothing on screen but this one line. On EKS 2026-08-30 it
  // claimed the bundle was being generated for the ~40 seconds of apply's
  // pre-flight checks -- while the timeline beside it said `applying the
  // bundle` and `bundle phase complete` a minute earlier.
  it('names the phase it is actually waiting on before any component reports', () => {
    const props = { events: [], onDecide: vi.fn(), onRetry: vi.fn() }

    const { unmount } = render(<Cockpit {...props} run={baseRun({ phase: 'bundle' })} />)
    expect(screen.getByText(/generating the bundle/i)).toBeDefined()
    unmount()

    render(<Cockpit {...props} run={baseRun({ phase: 'apply' })} />)
    expect(screen.queryByText(/generating the bundle/i)).toBeNull()
    expect(screen.getByText(/pre-flight checks/i)).toBeDefined()
  })

  it('gate: renders the recipe components, a bundle download link, and sends {apply: yes} on Install', () => {
    const onDecide = vi.fn()
    const run = baseRun({ state: 'awaiting_decision', phase: 'apply' })
    render(<Cockpit events={[]} run={run} onDecide={onDecide} onRetry={vi.fn()} />)

    // By testid, not by text: several components share a name with the
    // namespace they land in (cert-manager, gpu-operator, kai-scheduler), so
    // once the list is grouped a bare text match finds both the heading and
    // the row and cannot tell which it wanted.
    for (const c of recipe.components) {
      expect(screen.getByTestId(`gate-component-${c.name}`).textContent).toMatch(c.name)
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

  // NAMESPACES ARE THE ONE GROUPING THE GATE ALREADY KNOWS.
  //
  // Fourteen components in a flat alphabetical list carried no information:
  // four of them land in `monitoring` and that was invisible with them
  // scattered across the list. Alphabetical is the one order that says
  // nothing about what is being approved.
  it('gate: groups the components by the namespace they land in', () => {
    const run = baseRun({ state: 'awaiting_decision', phase: 'apply' })
    render(<Cockpit events={[]} run={run} onDecide={vi.fn()} onRetry={vi.fn()} />)

    const group = screen.getByTestId('gate-namespace-kubeflow')
    expect(group.textContent).toMatch(/kubeflow-trainer/)
    expect(group.textContent).not.toMatch(/cert-manager/)
  })

  // WHY IS THIS COMPONENT IN MY CLUSTER.
  //
  // Discover names the gaps -- "No GPU-aware scheduler", "No GPU metrics" --
  // and the gate lists the components that close them, and nothing connected
  // the two. gap.Gap carries the component name already, so the join is
  // free; without it the gaps read as alarms and the list reads as a bill.
  it('gate: says which gap each component closes', () => {
    const run = baseRun({
      state: 'awaiting_decision',
      phase: 'apply',
      report: {
        headline: 'h', punchline: 'p', usableGpus: 16, totalGpus: 16, analyzed: true, simulated: false,
        gaps: [
          { id: 'sched', title: 'No GPU-aware scheduler', component: 'kai-scheduler' },
          { id: 'device-plugin', title: 'No device plugin', component: 'gpu-operator' },
          { id: 'gpu-metrics', title: 'No GPU metrics', component: 'gpu-operator' },
        ],
      },
    })
    render(<Cockpit events={[]} run={run} onDecide={vi.fn()} onRetry={vi.fn()} />)

    expect(screen.getByTestId('gate-component-kai-scheduler').textContent)
      .toMatch(/No GPU-aware scheduler/)
    // THREE of internal/gap's rules name gpu-operator -- gpu-driver,
    // device-plugin and gpu-metrics -- so a map keyed by component keeps only
    // the last and silently drops the other two. On a real cluster that hid
    // "No device plugin, Kubernetes cannot schedule nvidia.com/gpu", which is
    // the most consequential of the three, behind "No GPU metrics".
    const gpu = screen.getByTestId('gate-component-gpu-operator').textContent ?? ''
    expect(gpu).toMatch(/No device plugin/)
    expect(gpu).toMatch(/No GPU metrics/)
    // A component that closes no gap makes no claim about one.
    expect(screen.getByTestId('gate-component-cert-manager').textContent)
      .not.toMatch(/No GPU-aware scheduler/)
  })

  // "every version pinned" was contradicted two rows later on a real recipe:
  // gke-nccl-tcpxo and nodewright-customizations are AICR-generated local
  // charts with no upstream version to pin, and the screen made a blanket
  // claim and then showed the exceptions. On the one screen whose whole job
  // is honesty, that is the expensive kind of small wrong.
  it('gate: does not claim every version is pinned when some have none', () => {
    const mixed = {
      ...recipe,
      componentCount: 5,
      components: [
        ...recipe.components,
        { name: 'gke-nccl-tcpxo', kind: 'Helm', version: '', namespace: 'kube-system' },
      ],
    }
    const run = baseRun({ state: 'awaiting_decision', phase: 'apply', recipe: mixed })
    render(<Cockpit events={[]} run={run} onDecide={vi.fn()} onRetry={vi.fn()} />)

    expect(screen.queryByText(/every version pinned/)).toBeNull()
    expect(screen.getByText(/4 of 5 pinned/)).toBeDefined()
    // and the two without one are explained rather than left blank.
    expect(screen.getByTestId('gate-component-gke-nccl-tcpxo').textContent).toMatch(/generated/i)
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
