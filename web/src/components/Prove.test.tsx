import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { Prove } from './Prove'
import type { RunState } from './Wizard'
import type { AicrEvent } from '../useEvents'

const RUN_ID = '00112233445566ff'

/**
 * placementEvents reproduces what internal/steps/prove.go's awaitGang emits,
 * field for field: KindCluster, the prove phase the engine stamps on every
 * step emit, and one event per gang member the instant its pod is bound.
 * internal/steps/prove_test.go pins the producing side.
 */
function placementEvents(): AicrEvent[] {
  return [
    {
      id: 20, runId: RUN_ID, at: '2026-08-21T00:00:01Z', kind: 'cluster', level: 'info',
      phase: 'prove', message: `gang member prove-${RUN_ID}-0 placed on node kwok-gpu-0`,
    },
    {
      id: 21, runId: RUN_ID, at: '2026-08-21T00:00:02Z', kind: 'cluster', level: 'info',
      phase: 'prove', message: `gang member prove-${RUN_ID}-1 placed on node kwok-gpu-1`,
    },
  ]
}

/**
 * report mirrors internal/gap.Report as the discover phase publishes it.
 * totalGpus is the field that decides what this screen may claim: gap.go's
 * punchline() calls zero "a simulated cluster" in as many words, and the
 * recorded KWOK stream carries exactly that.
 */
function report(totalGpus: number, usableGpus = 0) {
  return {
    headline: 'This is a kind cluster with 7 node(s).',
    punchline: 'punchline',
    usableGpus,
    totalGpus,
    analyzed: true,
    gaps: null,
  }
}

function runState(overrides: Partial<RunState> = {}): RunState {
  return {
    runId: RUN_ID,
    phase: 'prove',
    state: 'active',
    report: report(0),
    recipe: null,
    ...overrides,
  }
}

describe('Prove', () => {
  it('shows the allocation decision and a Stop control while active', () => {
    const onStop = vi.fn()
    render(<Prove events={placementEvents()} run={runState()} busy={false} onStop={onStop} />)

    const placements = screen.getByTestId('prove-placements')
    expect(placements.textContent).toMatch(/placed on node kwok-gpu-0/)
    expect(placements.textContent).toMatch(/placed on node kwok-gpu-1/)
    expect(screen.getByRole('button', { name: /stop workload/i })).toBeDefined()
  })

  // THE SCREENSHOT ANYONE WOULD PUT IN A SLIDE, and the numbers for it were
  // scattered across forty timeline lines. This is the moment the run is
  // proven; it should state what it achieved without the operator counting
  // rows.
  it('summarises what the run achieved', () => {
    const events: AicrEvent[] = [
      { id: 1, runId: RUN_ID, at: '2026-08-28T13:00:00Z', kind: 'component', level: 'info', phase: 'apply',
        component: 'cert-manager', message: 'installing cert-manager',
        data: { name: 'cert-manager', index: 1, total: 2, status: 'started' } },
      { id: 2, runId: RUN_ID, at: '2026-08-28T13:02:00Z', kind: 'component', level: 'info', phase: 'apply',
        component: 'cert-manager', message: 'cert-manager installed',
        data: { name: 'cert-manager', status: 'installed' } },
      { id: 3, runId: RUN_ID, at: '2026-08-28T13:02:00Z', kind: 'component', level: 'info', phase: 'apply',
        component: 'nfd', message: 'installing nfd',
        data: { name: 'nfd', index: 2, total: 2, status: 'started' } },
      { id: 4, runId: RUN_ID, at: '2026-08-28T13:14:30Z', kind: 'component', level: 'info', phase: 'apply',
        component: 'nfd', message: 'nfd installed', data: { name: 'nfd', status: 'installed' } },
      ...placementEvents(),
    ]
    render(<Prove events={events} run={runState({ report: report(16, 16) })} busy={false} onStop={vi.fn()} />)

    const summary = screen.getByTestId('prove-summary').textContent ?? ''
    expect(summary).toMatch(/2 of 2/)        // actions installed
    expect(summary).toMatch(/14m 30s/)       // wall clock across the install
    expect(summary).toMatch(/2/)             // gang members placed
    expect(summary).toMatch(/16 of 16 GPUs/) // what the cluster offered
  })

  // A simulated cluster has no GPU figure worth printing, and "0 of 0 GPUs"
  // is worse than silence -- the separate simulated caveat already says what
  // is true there.
  it('omits the GPU figure on a cluster that reported none', () => {
    render(<Prove events={placementEvents()} run={runState()} busy={false} onStop={vi.fn()} />)

    expect(screen.getByTestId('prove-summary').textContent).not.toMatch(/GPUs/)
  })

  // The claim this screen must NOT make on a cluster with no GPUs in it. The
  // design is explicit that the simulated finale is labeled "without apology"
  // -- what it may not do is imply a throughput number nothing measured.
  it('labels a simulated cluster and makes no throughput claim', () => {
    render(<Prove events={placementEvents()} run={runState()} busy={false} onStop={vi.fn()} />)

    expect(screen.getByTestId('prove-simulated').textContent)
      .toMatch(/simulated cluster, no GPU hardware/i)
    expect(screen.queryByText(/GB\/s/)).toBeNull()
    expect(screen.queryByTestId('prove-real')).toBeNull()
  })

  // The other half of the same branch: on hardware, the simulated label is a
  // lie, and asserting only its presence above would pass just as happily if
  // the screen printed it unconditionally.
  it('does not call a cluster with GPUs simulated', () => {
    render(
      <Prove events={placementEvents()} run={runState({ report: report(64) })} busy={false} onStop={vi.fn()} />)

    expect(screen.queryByTestId('prove-simulated')).toBeNull()
    expect(screen.getByTestId('prove-real').textContent).toMatch(/0 of 64 GPUs/)
    expect(screen.queryByText(/GB\/s/)).toBeNull()
  })

  // A run adopted at startup (internal/engine/reconcile.go) publishes no
  // capability report at all -- there was no record for recovery to bootstrap
  // from. Claiming either answer here would be the console inventing a fact
  // about hardware it never measured.
  it('claims nothing about hardware for a run with no capability report', () => {
    render(
      <Prove events={placementEvents()} run={runState({ report: null })} busy={false} onStop={vi.fn()} />)

    expect(screen.queryByTestId('prove-simulated')).toBeNull()
    expect(screen.queryByTestId('prove-real')).toBeNull()
    expect(screen.getByRole('button', { name: /stop workload/i })).toBeDefined()
  })

  // Wizard renders this screen for the whole prove phase, so the in-progress
  // state is a real one -- and telling an operator watching the gang be placed
  // that it "has stopped" would be the screen contradicting the events next
  // to it.
  it('says the workload is being placed while the run is still running', () => {
    render(
      <Prove events={[]} run={runState({ state: 'running' })} busy={false} onStop={vi.fn()} />)

    expect(screen.getByRole('heading', { name: /placing the reference workload/i })).toBeDefined()
    expect(screen.queryByTestId('prove-stopped')).toBeNull()
    expect(screen.queryByRole('button', { name: /stop workload/i })).toBeNull()
  })

  // The claim this screen must never make: a run that failed at Prove may have
  // failed its own cleanup too, and the console cannot tell from the event
  // stream. "Nothing is holding your accelerators" would be an assertion about
  // something it did not observe.
  it('claims nothing about what a failed run left behind', () => {
    render(
      <Prove events={[]} run={runState({ state: 'failed' })} busy={false} onStop={vi.fn()} />)

    expect(screen.getByTestId('prove-failed')).toBeDefined()
    expect(screen.queryByTestId('prove-stopped')).toBeNull()
    expect(screen.queryByText(/still holding the cluster's accelerators/i)).toBeNull()
    expect(screen.queryByRole('button', { name: /stop workload/i })).toBeNull()
    // No allocation claim either: nothing was placed, so there is nothing for
    // the simulated/real branch to be about.
    expect(screen.queryByTestId('prove-simulated')).toBeNull()
  })

  // A stop deletes the workload and then waits for its pods to ACTUALLY be
  // gone, which is minutes on a real cluster. Disabling the button was the
  // whole of the feedback, so the screen looked identical to a dead click for
  // the entire operation -- observed on real hardware, where the operator
  // reasonably concluded nothing had happened.
  it('says it is stopping, and why it takes a while, while a stop is in flight', () => {
    render(<Prove events={placementEvents()} run={runState()} busy={true} onStop={vi.fn()} />)

    const button = screen.getByRole('button', { name: /stopping/i })
    expect(button.hasAttribute('disabled')).toBe(true)
    // The wait has a stated reason, so a slow stop reads as work rather than
    // as a hang.
    expect(screen.getByTestId('prove-stopping').textContent).toMatch(/pods/i)
  })

  // THE SCREEN THAT LOOKED LIKE A FAILURE.
  //
  // StateActive is Prove's terminal SUCCESS state: the gang placed, and the
  // reference workload is `sleep infinity` holding that placement by design,
  // so no later state ever arrives. Before this, the only coloured things on
  // the screen were a red Stop and a red Reset, with no success signal at
  // all -- and it was read as an error by the person who built it.
  it('marks an active run as the successful end state', () => {
    render(<Prove events={placementEvents()} run={runState()} busy={false} onStop={vi.fn()} />)

    const success = screen.getByTestId('prove-success')
    expect(success.textContent).toMatch(/succeeded/i)
    // Says the run ENDS here. Waiting for something further is the specific
    // mistake this state invites, and nothing else on the screen rules it out.
    expect(success.textContent).toMatch(/last step|ends here|nothing further/i)
  })

  // The success line is about the run, not the hardware, so it holds on a
  // simulated cluster too -- where the placement is exactly as real. The
  // separate simulated caveat is what keeps the hardware claim honest.
  it('marks success on a simulated cluster without claiming hardware', () => {
    render(<Prove events={placementEvents()} run={runState()} busy={false} onStop={vi.fn()} />)

    expect(screen.getByTestId('prove-success')).toBeDefined()
    expect(screen.getByTestId('prove-simulated')).toBeDefined()
  })

  // The counterpart: a stopped or failed run must not claim success. A signal
  // that shows in every state is not a signal.
  it('claims no success once the run is no longer active', () => {
    for (const state of ['done', 'failed'] as const) {
      const { unmount } = render(
        <Prove events={placementEvents()} run={runState({ state })} busy={false} onStop={vi.fn()} />)
      expect(screen.queryByTestId('prove-success'), state).toBeNull()
      unmount()
    }
  })

  // Once the workload is gone the button must go with it: engine.Stop answers
  // a run that is no longer active with a 409, so leaving it on screen would
  // be offering an action guaranteed to fail.
  it('offers no Stop control once the run is no longer active', () => {
    render(
      <Prove events={placementEvents()} run={runState({ state: 'done' })} busy={false} onStop={vi.fn()} />)

    expect(screen.queryByRole('button', { name: /stop workload/i })).toBeNull()
    expect(screen.getByTestId('prove-stopped')).toBeDefined()
    // The placement history stays: it is the record of what the cluster did.
    expect(screen.getByTestId('prove-placements').textContent).toMatch(/placed on node/)
  })

  // The verdict has to be on the screen the operator ends on, beside the
  // placement claim rather than instead of it.
  it('shows what validation found', () => {
    const run = runState({
      validation: {
        phases: [{ phase: 'deployment', status: 'passed', seconds: 92, tests: 14, passed: 14, failed: 0, skipped: 0 }],
      },
    })
    render(<Prove events={placementEvents()} run={run} busy={false} onStop={vi.fn()} />)

    const panel = screen.getByTestId('prove-validation')
    expect(panel.textContent).toMatch(/deployment/)
    expect(panel.textContent).toMatch(/14/)
  })

  // A skip is not a pass, and the screen must not let it read as one.
  it('says why validation was skipped rather than showing a verdict', () => {
    const run = runState({ validation: { skipped: 'simulated cluster -- AICR’s validator lands on fake nodes' } })
    render(<Prove events={placementEvents()} run={run} busy={false} onStop={vi.fn()} />)

    const panel = screen.getByTestId('prove-validation')
    expect(panel.textContent).toMatch(/skipped/i)
    expect(panel.textContent).toMatch(/simulated/i)
    expect(panel.textContent).not.toMatch(/passed/i)
  })

  // No validation at all is a third state, distinct from both.
  it('shows no validation panel when the step has not run', () => {
    render(<Prove events={placementEvents()} run={runState()} busy={false} onStop={vi.fn()} />)

    expect(screen.queryByTestId('prove-validation')).toBeNull()
  })

  // internal/observer publishes KindCluster telemetry of its own with no
  // phase at all, and the discover phase publishes gap lines under the same
  // kind. Neither is a placement decision, and rendering either here would
  // put words in the scheduler's mouth.
  it('shows only placement events from the prove phase', () => {
    const noise: AicrEvent[] = [
      {
        id: 1, runId: RUN_ID, at: '2026-08-21T00:00:00Z', kind: 'cluster', level: 'warn',
        phase: 'discover', message: 'No GPU driver installed',
      },
      {
        id: 2, runId: RUN_ID, at: '2026-08-21T00:00:00Z', kind: 'cluster', level: 'info',
        component: 'gpu-operator', message: 'nvidia-driver-daemonset 2/8 nodes ready',
      },
    ]
    render(
      <Prove events={[...noise, ...placementEvents()]} run={runState()} busy={false} onStop={vi.fn()} />)

    const placements = screen.getByTestId('prove-placements')
    expect(placements.textContent).not.toMatch(/No GPU driver installed/)
    expect(placements.textContent).not.toMatch(/nvidia-driver-daemonset/)
    expect(placements.querySelectorAll('li').length).toBe(2)
  })
})
