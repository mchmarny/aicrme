import { render, screen } from '@testing-library/react'
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
