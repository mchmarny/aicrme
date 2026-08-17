import { act, renderHook } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { GAP_RECONNECT_BASE_DELAY_MS, MAX_GAP_RECONNECT_ATTEMPTS, useEvents, type AicrEvent } from './useEvents'

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
  private listeners: Record<string, Array<(msg: MessageEvent<string>) => void>> = {}

  constructor(url: string) {
    this.url = url
    MockEventSource.instances.push(this)
  }

  addEventListener(type: string, handler: (msg: MessageEvent<string>) => void) {
    (this.listeners[type] ??= []).push(handler)
  }

  close() {
    this.closed = true
  }

  emit(data: unknown) {
    this.onmessage?.({ data: JSON.stringify(data) } as MessageEvent<string>)
  }

  // emitNamed simulates a named SSE frame (`event: <type>`), which
  // EventSource dispatches to addEventListener(type, ...) rather than
  // onmessage. The epoch control frame is the only named event the server
  // sends today.
  emitNamed(type: string, data: unknown) {
    for (const handler of this.listeners[type] ?? []) {
      handler({ data: JSON.stringify(data) } as MessageEvent<string>)
    }
  }
}

const ev = (id: number): AicrEvent => ({
  id, at: '2026-08-13T00:00:00Z', kind: 'log', level: 'info', message: `event ${id}`,
})

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
    vi.unstubAllGlobals()
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
    vi.useFakeTimers()
    renderHook(() => useEvents())
    const first = MockEventSource.instances[0]

    act(() => {
      first.emit(ev(5))
    })
    act(() => {
      // Skips 6: the bus dropped a live event for this subscriber.
      first.emit(ev(8))
    })
    expect(first.closed).toBe(true)

    // The reconnect is scheduled behind the first backoff delay, not fired
    // synchronously (see the runaway-reconnect tests below for why).
    act(() => {
      vi.advanceTimersByTime(GAP_RECONNECT_BASE_DELAY_MS)
    })

    expect(MockEventSource.instances.length).toBe(2)
    expect(MockEventSource.instances[1].url).toContain('since=5')
    vi.useRealTimers()
  })

  // Regression coverage for: a gap that never fills (the ring evicted past
  // lastId for good) reconnecting forever, each time asking the server to
  // serialize its whole backlog. Confirmed this test fails against the
  // pre-fix code, which had no attempt ceiling at all.
  it('stops reconnecting after MAX_GAP_RECONNECT_ATTEMPTS and resumes consuming events', () => {
    vi.useFakeTimers()
    const { result } = renderHook(() => useEvents())

    act(() => {
      MockEventSource.instances[0].emit(ev(5))
    })

    // Every reconnect sees the exact same unfillable gap (5 -> 8). Drive it
    // through MAX_GAP_RECONNECT_ATTEMPTS reconnects...
    for (let attempt = 1; attempt <= MAX_GAP_RECONNECT_ATTEMPTS; attempt++) {
      const current = MockEventSource.instances[MockEventSource.instances.length - 1]
      act(() => {
        current.emit(ev(8))
      })
      act(() => {
        vi.advanceTimersByTime(GAP_RECONNECT_BASE_DELAY_MS * 2 ** attempt)
      })
    }
    expect(MockEventSource.instances.length).toBe(1 + MAX_GAP_RECONNECT_ATTEMPTS)

    // ...then hit the cap: the same gap one more time must NOT trigger a
    // (MAX_GAP_RECONNECT_ATTEMPTS + 1)th reconnect, even after waiting well
    // past any conceivable backoff.
    const capped = MockEventSource.instances[MockEventSource.instances.length - 1]
    act(() => {
      capped.emit(ev(8))
    })
    act(() => {
      vi.advanceTimersByTime(60_000)
    })
    expect(MockEventSource.instances.length).toBe(1 + MAX_GAP_RECONNECT_ATTEMPTS)

    // The hole is accepted (surfaced as loss, not silently dropped) and
    // normal streaming resumes on the same, still-open connection.
    expect(result.current.events.map(e => e.id)).toEqual([5, 8])
    expect(result.current.eventsLost).toBe(2) // ids 6 and 7 never arrived
    expect(capped.closed).toBe(false)

    act(() => {
      capped.emit(ev(9))
    })
    expect(result.current.events.map(e => e.id)).toEqual([5, 8, 9])
    expect(MockEventSource.instances.length).toBe(1 + MAX_GAP_RECONNECT_ATTEMPTS)

    vi.useRealTimers()
  })

  it('resets the gap-attempt counter once a reconnect yields a contiguous stream', () => {
    vi.useFakeTimers()
    renderHook(() => useEvents())

    act(() => {
      MockEventSource.instances[0].emit(ev(5))
    })
    act(() => {
      MockEventSource.instances[0].emit(ev(8)) // gap #1: attempt 1 of MAX
    })
    act(() => {
      vi.advanceTimersByTime(GAP_RECONNECT_BASE_DELAY_MS)
    })
    expect(MockEventSource.instances.length).toBe(2)

    // This reconnect lands on a contiguous id: no further reconnect, and the
    // attempt counter must reset rather than staying at 1.
    act(() => {
      MockEventSource.instances[1].emit(ev(6))
    })
    expect(MockEventSource.instances.length).toBe(2)

    // A second, unrelated gap should get its own full MAX_GAP_RECONNECT_ATTEMPTS
    // budget. If the counter had not reset, this would cap out one reconnect
    // early (after MAX_GAP_RECONNECT_ATTEMPTS - 1 more, not MAX_GAP_RECONNECT_ATTEMPTS).
    for (let attempt = 1; attempt <= MAX_GAP_RECONNECT_ATTEMPTS; attempt++) {
      const current = MockEventSource.instances[MockEventSource.instances.length - 1]
      act(() => {
        current.emit(ev(9)) // lastId is 6, so 9 is a gap every time
      })
      act(() => {
        vi.advanceTimersByTime(GAP_RECONNECT_BASE_DELAY_MS * 2 ** attempt)
      })
    }
    // 2 (from the first gap's recovery) + MAX_GAP_RECONNECT_ATTEMPTS more.
    expect(MockEventSource.instances.length).toBe(2 + MAX_GAP_RECONNECT_ATTEMPTS)

    vi.useRealTimers()
  })

  // EventSource surfaces no HTTP status on error, so the hook cannot tell an
  // expired session from a network blip on its own -- it probes GET
  // /api/session (204 authenticated, 401 not) to tell them apart. Only a
  // genuine 401 may call onUnauthorized; EventSource's own retry already
  // handles everything else.
  it('calls onUnauthorized when the stream drops and the session probe reports a genuine 401', async () => {
    const fetchMock = vi.fn(async () => new Response(null, { status: 401 }))
    vi.stubGlobal('fetch', fetchMock)
    const onUnauthorized = vi.fn()

    renderHook(() => useEvents(onUnauthorized))
    const source = MockEventSource.instances[0]

    await act(async () => {
      source.onerror?.()
    })

    expect(fetchMock).toHaveBeenCalledWith('/api/session')
    expect(onUnauthorized).toHaveBeenCalledTimes(1)
  })

  it('does not call onUnauthorized when the stream drops but the session probe reports the session is still live', async () => {
    const fetchMock = vi.fn(async () => new Response(null, { status: 204 }))
    vi.stubGlobal('fetch', fetchMock)
    const onUnauthorized = vi.fn()

    renderHook(() => useEvents(onUnauthorized))
    const source = MockEventSource.instances[0]

    await act(async () => {
      source.onerror?.()
    })

    expect(fetchMock).toHaveBeenCalledWith('/api/session')
    expect(onUnauthorized).not.toHaveBeenCalled()
  })

  // The epoch is a named, id-less SSE control frame: nextID resets to 1 on
  // every process restart, so a lastId issued by a prior process looks like
  // a valid-but-stale cursor rather than an obviously wrong one, and the
  // server would otherwise filter out everything at or below it -- a live,
  // healthy-looking connection that delivers nothing. The epoch lets the
  // client tell "same process, ordinary reconnect" apart from "different
  // process, my cursor is meaningless" and react only to the latter.
  describe('epoch handling', () => {
    it('records the epoch from the very first connection without reconnecting', () => {
      renderHook(() => useEvents())
      const first = MockEventSource.instances[0]

      act(() => {
        first.emitNamed('epoch', { epoch: 'epoch-a' })
      })

      // Nothing to correct on a fresh connection -- lastId is already 0, so
      // treating the first epoch seen as a "change" would reconnect on every
      // single mount for no reason.
      expect(MockEventSource.instances.length).toBe(1)
      expect(first.closed).toBe(false)
    })

    // This is the property the brief calls out as the one most likely to be
    // implemented plausibly and wrongly: resetting lastId in place does NOT
    // work, because the server already chose its backlog from the original
    // (stale) cursor when the connection opened. Proven by bite-proofing in
    // task-9-report.md -- a reset-only implementation makes this test fail
    // while leaving the ordinary-streaming tests above green.
    it('tears down the connection and reopens at since=0 when the epoch changes, discarding accumulated state', () => {
      const { result } = renderHook(() => useEvents())
      const first = MockEventSource.instances[0]

      act(() => {
        first.emitNamed('epoch', { epoch: 'epoch-a' })
      })
      act(() => {
        first.emit(ev(50))
      })
      expect(result.current.events.map(e => e.id)).toEqual([50])

      // A different epoch on the same connection: the process behind it
      // restarted mid-stream (or the client mistook a fresh process for a
      // continuation of the one it last saw).
      act(() => {
        first.emitNamed('epoch', { epoch: 'epoch-b' })
      })

      expect(first.closed).toBe(true)
      expect(MockEventSource.instances.length).toBe(2)
      expect(MockEventSource.instances[1].url).toContain('since=0')
      // The event from the superseded process is gone, not merged with
      // whatever the new process goes on to replay.
      expect(result.current.events).toEqual([])
    })

    it('ignores frames still queued from the stale source after an epoch change', () => {
      const { result } = renderHook(() => useEvents())
      const first = MockEventSource.instances[0]

      act(() => {
        first.emitNamed('epoch', { epoch: 'epoch-a' })
      })
      act(() => {
        first.emitNamed('epoch', { epoch: 'epoch-b' })
      })
      const second = MockEventSource.instances[1]

      // The old source delivers a frame it had already selected under the
      // stale cursor before the epoch fired. It must not land in the
      // freshly cleared timeline.
      act(() => {
        first.emit(ev(50))
      })
      expect(result.current.events).toEqual([])

      // The new connection's own events still land normally.
      act(() => {
        second.emit(ev(1))
      })
      expect(result.current.events.map(e => e.id)).toEqual([1])
    })
  })
})
