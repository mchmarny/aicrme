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
})
