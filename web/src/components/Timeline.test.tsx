import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { Timeline } from './Timeline'
import type { AicrEvent } from '../useEvents'

const events: AicrEvent[] = [
  { id: 1, at: '2026-08-13T00:00:00Z', kind: 'phase', level: 'info', message: 'phase started', phase: 'discover' },
  { id: 2, at: '2026-08-13T00:00:01Z', kind: 'log', level: 'warn', message: 'FailedScheduling' },
]

describe('Timeline', () => {
  it('renders every event message', () => {
    render(<Timeline events={events} />)
    expect(screen.getByText('phase started')).toBeDefined()
    expect(screen.getByText('FailedScheduling')).toBeDefined()
  })

  it('marks warnings so they are surfaced, not buried', () => {
    render(<Timeline events={events} />)
    expect(screen.getByTestId('event-2').className).toContain('text-amber')
  })

  it('renders an empty state with no events', () => {
    render(<Timeline events={[]} />)
    expect(screen.getByText(/waiting for events/i)).toBeDefined()
  })

  // docs/ux-feedback.md item 1, observed during a real demo run: the stream
  // appended, so during a 14-action install it grew past the viewport and the
  // operator had to scroll to see what was happening NOW -- during precisely
  // the five minutes the demo exists to be watched.
  it('puts the newest event first, so a live run needs no scrolling', () => {
    render(<Timeline events={events} />)
    const order = screen.getAllByRole('listitem').map(li => li.getAttribute('data-testid'))
    expect(order).toEqual(['event-2', 'event-1'])
  })

  // Ordered by the bus's own monotonic id rather than by arrival. A tab that
  // joins late replays from the ring buffer before live events resume, and
  // nothing guarantees the two interleave in id order -- so "newest last in
  // the array" is not the same claim as "newest".
  it('orders by event id, not by the order they arrived', () => {
    render(<Timeline events={[events[1], events[0]]} />)
    const order = screen.getAllByRole('listitem').map(li => li.getAttribute('data-testid'))
    expect(order).toEqual(['event-2', 'event-1'])
  })

  // Array.prototype.reverse sorts in place. The same events array is held by
  // Wizard and passed to other views, so reversing it here would silently
  // reorder theirs.
  it('does not reorder the array it was given', () => {
    const input = [...events]
    render(<Timeline events={input} />)
    expect(input.map(e => e.id)).toEqual([1, 2])
  })
})

const ev = (o: Partial<AicrEvent> & { id: number }): AicrEvent => ({
  at: '2026-08-13T00:00:00Z', kind: 'log', level: 'info', message: `event ${o.id}`, ...o,
})

describe('Timeline run scoping', () => {
  // A failed run from an hour ago rendered flush against the run you just
  // started, in identical styling, with no boundary between them. On the real
  // GKE cluster that put a wall of red timeout text from a dead run directly
  // beneath the live run's progress, which reads as though the thing happening
  // now is on fire.
  it('shows only the current run when it knows which one that is', () => {
    render(<Timeline runId="now" events={[
      ev({ id: 1, runId: 'before', level: 'error', message: 'the previous run timed out' }),
      ev({ id: 2, runId: 'before', message: 'previous run detail' }),
      ev({ id: 3, runId: 'now', message: 'deploying cluster snapshot agent' }),
    ]} />)

    expect(screen.getByText('deploying cluster snapshot agent')).toBeDefined()
    expect(screen.queryByText('the previous run timed out')).toBeNull()
  })

  // Collapsed, never dropped: the earlier run is still the evidence for what
  // the cluster looks like now, and a timeline that silently discards it is
  // lying by omission.
  it('says how much earlier history it is holding back, and can reveal it', () => {
    render(<Timeline runId="now" events={[
      ev({ id: 1, runId: 'before', level: 'error', message: 'the previous run timed out' }),
      ev({ id: 2, runId: 'before', message: 'previous run detail' }),
      ev({ id: 3, runId: 'now', message: 'deploying cluster snapshot agent' }),
    ]} />)

    fireEvent.click(screen.getByText(/2 events from earlier runs/))
    expect(screen.getByText('the previous run timed out')).toBeDefined()
  })
})

describe('Timeline noise', () => {
  // 289 of 397 events on a real run were cluster-kind pod chatter -- 73% --
  // and the DNSConfigForming lines repeat per pod while saying nothing about
  // the install. The component and phase events that describe progress were
  // outnumbered three to one.
  it('hides routine cluster chatter by default', () => {
    render(<Timeline events={[
      ev({ id: 1, kind: 'cluster', message: 'monitoring/prometheus-node-exporter: DNSConfigForming resolved' }),
      ev({ id: 2, kind: 'component', message: 'cert-manager installed' }),
    ]} />)

    expect(screen.getByText('cert-manager installed')).toBeDefined()
    expect(screen.queryByText(/DNSConfigForming/)).toBeNull()
  })

  // The filter is on kind AND level together. A cluster event that is warning
  // or worse is the pod that would not schedule -- exactly the thing worth
  // reading -- so it survives the filter that removes its routine siblings.
  it('never hides a cluster event that is a warning or an error', () => {
    render(<Timeline events={[
      ev({ id: 1, kind: 'cluster', level: 'warn', message: 'FailedScheduling: untolerated taint' }),
    ]} />)

    expect(screen.getByText('FailedScheduling: untolerated taint')).toBeDefined()
  })

  it('can show the whole stream on request', () => {
    render(<Timeline events={[
      ev({ id: 1, kind: 'cluster', message: 'DNSConfigForming resolved' }),
      ev({ id: 2, kind: 'component', message: 'cert-manager installed' }),
    ]} />)

    fireEvent.click(screen.getByText(/cluster activity/i))
    expect(screen.getByText(/DNSConfigForming/)).toBeDefined()
  })
})

describe('Timeline remedies', () => {
  // The kai-scheduler timeout embeds two shell commands inside a wrapped red
  // paragraph. An operator cannot tell where the prose stops and the command
  // starts, let alone select one cleanly.
  it('renders embedded commands as code rather than prose', () => {
    render(<Timeline events={[
      ev({
        id: 1, kind: 'error', level: 'error',
        message: 'try `kubectl delete schedulingshard default` and `kubectl rollout restart deploy -n kai-scheduler`, then retry',
      }),
    ]} />)

    const cmd = screen.getByText('kubectl delete schedulingshard default')
    expect(cmd.tagName).toBe('CODE')
    expect(screen.getByText('kubectl rollout restart deploy -n kai-scheduler').tagName).toBe('CODE')
  })
})
