import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { deriveComponents } from '../pipeline'
import type { AicrEvent } from '../useEvents'
import { Reset, ResetGate } from './Reset'
import { deriveRunState, type ResidueItem, type RunState } from './Wizard'

const RUN_ID = '00112233445566ff'

let nextId = 1

/**
 * installEvent reproduces what internal/applier/parse.go's header and
 * terminal markers publish, via internal/steps/apply.go's trackComponents.
 */
function installEvent(name: string, namespace: string, index: number, status: string): AicrEvent {
  return {
    id: nextId++, runId: RUN_ID, at: '2026-08-21T00:00:00Z', kind: 'component',
    level: 'info', phase: 'apply', component: name,
    message: `${name} ${status}`,
    data: { name, namespace, index, total: 3, status },
  }
}

/**
 * teardownEvent reproduces engine.publishResidueItem's payload field for
 * field (internal/engine/reset.go's residueData) -- including the operation
 * discriminator, which is what tells this module it is looking at a removal
 * rather than an install running backwards.
 */
function teardownEvent(
  name: string, namespace: string, kind: 'release' | 'namespace', status: string, reason?: string,
): AicrEvent {
  return {
    id: nextId++, runId: RUN_ID, at: '2026-08-21T00:10:00Z', kind: 'component',
    level: status === 'removed' ? 'info' : 'warn', phase: 'apply', component: name,
    message: `${kind} ${name} ${status}`,
    data: { name, namespace, kind, status, operation: 'teardown', reason },
  }
}

/** installed is a three-component run, in deploy.sh's own order. */
function installed(): AicrEvent[] {
  return [
    installEvent('cert-manager', 'cert-manager', 1, 'started'),
    installEvent('cert-manager', 'cert-manager', 1, 'installed'),
    installEvent('nfd', 'node-feature-discovery', 2, 'started'),
    installEvent('nfd', 'node-feature-discovery', 2, 'installed'),
    installEvent('gpu-operator', 'gpu-operator', 3, 'started'),
    installEvent('gpu-operator', 'gpu-operator', 3, 'installed'),
  ]
}

function runState(overrides: Partial<RunState> = {}): RunState {
  return { runId: RUN_ID, state: 'done', report: null, recipe: null, ...overrides }
}

function residue(items: ResidueItem[], incomplete = false) {
  return { incomplete, summary: 'reset: 2 of 3 releases removed, 0 of 0 namespaces removed', items }
}

describe('teardown rendering', () => {
  it('labels rows removing and removed during a teardown, not installing', () => {
    // Only gpu-operator has been reported on; the other two are still ahead
    // of the uninstall in flight.
    const events = [...installed(), teardownEvent('gpu-operator', 'gpu-operator', 'release', 'removed')]

    const rows = deriveComponents(events, undefined)

    const byName = Object.fromEntries(rows.map(r => [r.name, r.status]))
    expect(byName['gpu-operator']).toBe('removed')
    // Not 'installed'. internal/teardown emits per release only after helm
    // returns, so a row it has not reached yet would otherwise sit reading
    // as a successful install for minutes while the cluster is being
    // dismantled underneath it.
    expect(byName['nfd']).toBe('removing')
    expect(byName['cert-manager']).toBe('removing')
  })

  it('renders teardown rows in reverse install order', () => {
    const events = [...installed(), teardownEvent('gpu-operator', 'gpu-operator', 'release', 'removed')]

    const rows = deriveComponents(events, undefined)

    // The order the teardown actually works in: install order encodes
    // dependency, so cert-manager -- which issues the certificates
    // gpu-operator's webhooks present -- goes last.
    expect(rows.map(r => r.name)).toEqual(['gpu-operator', 'nfd', 'cert-manager'])
  })

  it('leaves an install rendering in install order', () => {
    // The bite-proof's other half: without a teardown event the ordering
    // must be untouched, or every install would render backwards.
    const rows = deriveComponents(installed(), undefined)
    expect(rows.map(r => r.name)).toEqual(['cert-manager', 'nfd', 'gpu-operator'])
  })

  it('keeps namespaces out of the component pipeline', () => {
    // A namespace was never a deploy.sh step. Rendering one as a component
    // row puts a phantom entry in a numbered pipeline.
    const events = [
      ...installed(),
      teardownEvent('gpu-operator', 'gpu-operator', 'release', 'removed'),
      teardownEvent('gpu-operator', '', 'namespace', 'removed'),
    ]

    const rows = deriveComponents(events, undefined)
    expect(rows).toHaveLength(3)
  })
})

describe('the confirm gate', () => {
  it('lists every release and namespace before the second confirm click', () => {
    const onReset = vi.fn()
    const components = deriveComponents(installed(), undefined)
    render(<ResetGate run={runState()} components={components} busy={false} onReset={onReset} />)

    // One click is not enough: the first only opens the inventory.
    fireEvent.click(screen.getByTestId('reset'))
    expect(onReset).not.toHaveBeenCalled()

    const listed = screen.getByTestId('reset-removals').textContent ?? ''
    for (const name of ['cert-manager', 'nfd', 'gpu-operator']) {
      expect(listed).toContain(name)
    }
    // Addressed as (name, namespace), the way a helm release actually is.
    expect(listed).toContain('node-feature-discovery')

    fireEvent.click(screen.getByTestId('reset-confirmed'))
    expect(onReset).toHaveBeenCalledTimes(1)
  })

  it('lists separately what will be skipped for want of ownership', () => {
    const run = runState({
      residue: residue([
        { kind: 'release', name: 'gpu-operator', namespace: 'gpu-operator', removed: true },
        {
          kind: 'release', name: 'cert-manager', namespace: 'cert-manager',
          skip: 'this release already existed before the install',
        },
        { kind: 'namespace', name: 'monitoring', skip: 'still holds ConfigMap somebody-elses-config' },
      ]),
    })
    render(<ResetGate run={run} components={deriveComponents(installed(), undefined)} busy={false} onReset={vi.fn()} />)

    fireEvent.click(screen.getByTestId('reset'))

    const skipped = screen.getByTestId('reset-skipped').textContent ?? ''
    expect(skipped).toContain('cert-manager')
    expect(skipped).toContain('already existed before the install')
    expect(skipped).toContain('monitoring')
    // A removed item is not a skipped one: the section names what the
    // operator now has to deal with by hand, and padding it with successes
    // would bury that.
    expect(skipped).not.toContain('gpu-operator')
  })
})

describe('which actions the console offers', () => {
  it('offers only Reset for a run with an incomplete teardown', () => {
    // engine.hasIncompleteTeardown blocks Start, Retry and Discard, so a
    // console that offered them would hand the operator three buttons that
    // all answer 409.
    const run = runState({
      residue: residue([
        { kind: 'release', name: 'cert-manager', namespace: 'cert-manager', error: 'release is in a failed state' },
      ], true),
    })
    render(<ResetGate run={run} components={deriveComponents(installed(), undefined)} busy={false} onReset={vi.fn()} />)

    expect(screen.getByTestId('reset')).toBeTruthy()
    expect(screen.queryByTestId('retry')).toBeNull()
    expect(screen.queryByTestId('discard')).toBeNull()
    // And it says why, rather than presenting a bare button.
    expect(screen.getByTestId('residue-warning').textContent).toContain('reset')
  })

  it('does not offer Reset mid-run', () => {
    // deriveRunState is what the Wizard switches its body on. A run in
    // flight resolves to a state whose screen carries no teardown, and this
    // pins the derivation rather than the markup: engine.Reset would 409 a
    // live run anyway, but an offered-then-rejected button is a bug the
    // operator sees.
    const running = deriveRunState([
      { id: 1, runId: RUN_ID, at: '2026-08-21T00:00:00Z', kind: 'phase', level: 'info', message: 'run running' },
    ])
    expect(running.state).toBe('running')
    expect(running.residue).toBeUndefined()
  })

  it('renders a teardown in flight as its own screen', () => {
    // "run resetting" is engine.Reset's own KindPhase message, chosen to
    // match finish's "run " + state shape precisely so this branch exists.
    const resetting = deriveRunState([
      { id: 1, runId: RUN_ID, at: '2026-08-21T00:00:00Z', kind: 'phase', level: 'warn', message: 'run resetting' },
    ])
    expect(resetting.state).toBe('resetting')
  })

  it('carries the incomplete guard across a restart', () => {
    // After a restart the recovery marker is the only event in the stream
    // that knows a teardown was interrupted.
    const recovered = deriveRunState([
      {
        id: 1, runId: RUN_ID, at: '2026-08-21T00:00:00Z', kind: 'recovered', level: 'warn',
        message: 'recovered a previous run', data: { residueIncomplete: true },
      },
      { id: 2, runId: RUN_ID, at: '2026-08-21T00:00:01Z', kind: 'phase', level: 'error', message: 'run failed' },
    ])
    expect(recovered.residue?.incomplete).toBe(true)
  })
})

describe('the teardown screen', () => {
  it('reports the counts rather than a bare verdict', () => {
    const run = runState({
      state: 'resetting',
      residue: residue([{ kind: 'release', name: 'gpu-operator', namespace: 'gpu-operator', removed: true }]),
    })
    render(<Reset events={[...installed(), teardownEvent('gpu-operator', 'gpu-operator', 'release', 'removed')]} run={run} />)

    // An operator must be able to tell a clean teardown from a partial one
    // without reading the timeline (design section 6).
    expect(screen.getByTestId('reset-summary').textContent).toContain('2 of 3 releases removed')
  })
})
