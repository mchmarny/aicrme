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

  it('disables Stop while a stop is already in flight', () => {
    render(<Prove events={placementEvents()} run={runState()} busy={true} onStop={vi.fn()} />)

    expect(screen.getByRole('button', { name: /stop workload/i }).hasAttribute('disabled')).toBe(true)
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
