import { act, renderHook } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { useEvents } from './useEvents'

/**
 * A minimal EventSource double. jsdom does not implement EventSource, and
 * these tests care about how many the hook creates and closes — not about
 * exercising a real network stack.
 */
class MockEventSource {
  static instances: MockEventSource[] = []
  url: string
  closed = false
  onopen: (() => void) | null = null
  onerror: (() => void) | null = null
  onmessage: ((msg: MessageEvent<string>) => void) | null = null

  constructor(url: string) {
    this.url = url
    MockEventSource.instances.push(this)
  }

  close() {
    this.closed = true
  }

  emit(data: unknown) {
    this.onmessage?.({ data: JSON.stringify(data) } as MessageEvent<string>)
  }
}

// React 18+ StrictMode runs a component's effects mount -> cleanup -> mount
// once in dev, on the same instance, so state (the lastId ref, the events
// list) survives across the pair while the effect body itself re-executes.
// That exact sequence — an effect's cleanup firing and then its setup
// re-running, without the component actually unmounting from the user's
// perspective — could not be reproduced through @testing-library/react's
// renderHook + <StrictMode> in this stack (Vitest 4.1.10, React 19.2.8,
// jsdom 30): the effect ran exactly once regardless of NODE_ENV, so there
// was nothing here to assert against without asserting a tautology.
//
// The property that matters is structural, not StrictMode-specific: the
// effect keeps its EventSource in a single `source` variable, and the
// cleanup function unconditionally closes whatever `source` currently holds.
// That holds regardless of why the effect's cleanup+setup cycle runs — a
// StrictMode double-invoke, a route change unmounting the console, or a
// literal unmount/remount — so these tests exercise the same code path
// through the sequence they can drive deterministically: consecutive mount
// and unmount cycles.
describe('useEvents connection lifecycle', () => {
  beforeEach(() => {
    MockEventSource.instances = []
    // @ts-expect-error -- test double, not a full EventSource implementation
    global.EventSource = MockEventSource
  })

  afterEach(() => {
    // @ts-expect-error -- undo the test double
    delete global.EventSource
  })

  it('unsubscribes on unmount, leaving no open connection behind', () => {
    const { unmount } = renderHook(() => useEvents())
    expect(MockEventSource.instances.length).toBe(1)

    unmount()

    expect(MockEventSource.instances.every(s => s.closed)).toBe(true)
  })

  it('does not leak a prior mount’s connection across a mount -> unmount -> mount cycle', () => {
    const first = renderHook(() => useEvents())
    first.unmount()
    renderHook(() => useEvents())

    expect(MockEventSource.instances.length).toBe(2)
    const open = MockEventSource.instances.filter(s => !s.closed)
    expect(open.length).toBe(1)
    expect(open[0]).toBe(MockEventSource.instances[1])
  })

  it('accumulates events delivered on the live connection without loss or duplication', () => {
    const { result } = renderHook(() => useEvents())
    const source = MockEventSource.instances[0]

    act(() => {
      source.emit({ id: 1, at: '2026-08-13T00:00:00Z', kind: 'log', level: 'info', message: 'one' })
    })
    act(() => {
      source.emit({ id: 2, at: '2026-08-13T00:00:01Z', kind: 'log', level: 'info', message: 'two' })
    })
    // A reconnect replay can redeliver an id the client already has.
    act(() => {
      source.emit({ id: 2, at: '2026-08-13T00:00:01Z', kind: 'log', level: 'info', message: 'two' })
    })

    expect(result.current.events.map(e => e.id)).toEqual([1, 2])
  })

  it('reopens the connection when an id arrives with a gap, replaying from the last seen id', () => {
    renderHook(() => useEvents())
    const first = MockEventSource.instances[0]

    act(() => {
      first.emit({ id: 5, at: '2026-08-13T00:00:00Z', kind: 'log', level: 'info', message: 'five' })
    })
    act(() => {
      // Skips 6: the bus dropped a live event for this subscriber.
      first.emit({ id: 8, at: '2026-08-13T00:00:03Z', kind: 'log', level: 'info', message: 'eight' })
    })

    expect(first.closed).toBe(true)
    expect(MockEventSource.instances.length).toBe(2)
    expect(MockEventSource.instances[1].url).toContain('since=5')
  })
})
