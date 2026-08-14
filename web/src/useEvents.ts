import { useEffect, useRef, useState } from 'react'

/** AicrEvent mirrors Go's bus.Event field-for-field. */
export interface AicrEvent {
  id: number
  runId?: string
  at: string
  kind: 'phase' | 'log' | 'component' | 'cluster' | 'decision' | 'error'
  phase?: string
  level: 'info' | 'warn' | 'error'
  component?: string
  message: string
  data?: unknown
}

/**
 * mergeEvents inserts e into the ordered list, ignoring duplicates. The SSE
 * replay-on-reconnect contract means the same id can arrive twice.
 *
 * Live events almost always arrive one past the current tail, so the common
 * case appends in O(1) with no scan or sort. The list only reaches thousands
 * of entries over a long run, and re-sorting all of it on every insert would
 * make that path O(n log n) per event; the fallback below — a duplicate scan
 * plus a full sort — is reserved for the rare backlog replay or out-of-order
 * delivery, where the caller trades a slower merge for correctness.
 */
export function mergeEvents(existing: AicrEvent[], e: AicrEvent): AicrEvent[] {
  const last = existing[existing.length - 1]
  if (last && e.id > last.id) return [...existing, e]
  if (existing.some(x => x.id === e.id)) return existing
  const next = [...existing, e]
  next.sort((a, b) => a.id - b.id)
  return next
}

/**
 * detectGap reports whether id skips over at least one event since lastId.
 * The bus drops live events for a subscriber that falls more than 256 behind
 * (see bus.subscriberBuffer) without ever closing the connection or erroring
 * — the stream looks healthy while quietly missing messages. lastId === 0 is
 * the pre-connection baseline, not a gap: the very first event delivered can
 * legitimately start above 1 if older events already aged out of the replay
 * ring.
 */
export function detectGap(lastId: number, id: number): boolean {
  return lastId > 0 && id > lastId + 1
}

/** useEvents subscribes to /api/events and accumulates the ordered timeline. */
export function useEvents() {
  const [events, setEvents] = useState<AicrEvent[]>([])
  const [connected, setConnected] = useState(false)
  const lastId = useRef(0)

  useEffect(() => {
    let source: EventSource
    let torndown = false

    // EventSource sends Last-Event-ID automatically on reconnect; ?since
    // seeds the very first connection after a full page reload, and reseeds
    // a manual reconnect triggered by detectGap below.
    function connect() {
      source = new EventSource(`/api/events?since=${lastId.current}`)
      source.onopen = () => setConnected(true)
      source.onerror = () => setConnected(false)
      source.onmessage = (msg: MessageEvent<string>) => {
        const parsed = JSON.parse(msg.data) as AicrEvent
        if (detectGap(lastId.current, parsed.id)) {
          // The bus silently dropped events for this subscriber. Reopen from
          // lastId so the server replays the hole instead of leaving it
          // unfilled; parsed itself arrives again as part of that replay.
          source.close()
          if (!torndown) connect()
          return
        }
        lastId.current = Math.max(lastId.current, parsed.id)
        setEvents(prev => mergeEvents(prev, parsed))
      }
    }

    connect()
    // React 18 StrictMode runs this effect mount → cleanup → mount once in
    // dev. torndown stops a reconnect (from detectGap or a future retry)
    // scheduled by the first mount's EventSource from racing the second
    // mount's; closing here always tears down whichever EventSource is
    // current, so no connection outlives the effect that owns it.
    return () => {
      torndown = true
      source.close()
    }
  }, [])

  return { events, connected }
}
