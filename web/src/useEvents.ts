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

/**
 * Caps how many consecutive gap-triggered reconnects useEvents attempts
 * before giving up on filling a hole. A gap can be transient — the replay
 * ring is mid-eviction relative to when the subscriber registered — and
 * usually clears on the first retry. Past that, the gap is real: the ring
 * evicted past lastId for good, and reopening again just asks the server to
 * serialize its whole backlog (up to bus.replayCapacity events) for no gain.
 * A tab left open against a permanently unfillable hole would otherwise
 * reconnect forever — a self-inflicted denial of service against the same
 * console it's displaying. 3 attempts bounds that to 3 backlog
 * serializations per genuine hole, and is generous enough that a transient
 * eviction race (which should clear on attempt 1) has margin to spare.
 */
export const MAX_GAP_RECONNECT_ATTEMPTS = 3

/**
 * Base delay for the exponential backoff between gap-triggered reconnects:
 * 250ms, 500ms, 1000ms for attempts 1-3. Keeps a genuine, unfillable hole
 * from hammering the server with back-to-back full-backlog requests, while
 * keeping the worst-case total delay (1.75s) short enough that a transient
 * gap resolves without the user noticing a stall.
 */
export const GAP_RECONNECT_BASE_DELAY_MS = 250

/**
 * useEvents subscribes to /api/events and accumulates the ordered timeline.
 *
 * onUnauthorized, if given, must be a stable reference (wrap it in
 * useCallback at the call site). It is a dependency of the effect below, so
 * a new function identity on every render would tear down and reopen the
 * SSE connection continuously -- a bug that looks exactly like a flaky
 * backend.
 */
export function useEvents(onUnauthorized?: () => void) {
  const [events, setEvents] = useState<AicrEvent[]>([])
  const [connected, setConnected] = useState(false)
  const [eventsLost, setEventsLost] = useState(0)
  const lastId = useRef(0)
  const gapAttempts = useRef(0)

  useEffect(() => {
    let source: EventSource
    let torndown = false
    let backoffTimer: ReturnType<typeof setTimeout> | undefined

    // EventSource sends Last-Event-ID automatically on reconnect; ?since
    // seeds the very first connection after a full page reload, and reseeds
    // a manual reconnect triggered by detectGap below.
    function connect() {
      source = new EventSource(`/api/events?since=${lastId.current}`)
      source.onopen = () => setConnected(true)
      source.onerror = () => {
        setConnected(false)
        // EventSource surfaces no HTTP status, so a dropped stream is
        // indistinguishable here from an expired session. After the 8-hour
        // expiry the console previously sat on "reconnecting…" forever with
        // no path back to the login screen. Probe a cheap authenticated
        // route to tell the two apart; anything other than a 401 is a
        // genuine blip and EventSource's own retry handles it.
        fetch('/api/session')
          .then(res => {
            if (res.status === 401 && !torndown) onUnauthorized?.()
          })
          .catch(() => {})
      }
      source.onmessage = (msg: MessageEvent<string>) => {
        const parsed = JSON.parse(msg.data) as AicrEvent
        if (detectGap(lastId.current, parsed.id) && gapAttempts.current < MAX_GAP_RECONNECT_ATTEMPTS) {
          // The bus silently dropped events for this subscriber. Reopen from
          // lastId so the server replays the hole instead of leaving it
          // unfilled; parsed itself arrives again as part of that replay —
          // unless the ring already evicted past lastId too, in which case
          // this fires again and the attempt cap above eventually gives up
          // rather than reconnecting forever.
          gapAttempts.current++
          source.close()
          const delay = GAP_RECONNECT_BASE_DELAY_MS * 2 ** (gapAttempts.current - 1)
          backoffTimer = setTimeout(() => {
            if (!torndown) connect()
          }, delay)
          return
        }
        // Either contiguous, or a gap that survived MAX_GAP_RECONNECT_ATTEMPTS
        // reconnects: accept it, count what was lost so the UI can surface
        // it instead of hiding it, and resume streaming rather than retrying
        // a hole that isn't going to fill. `lost` is computed here, before
        // lastId.current is mutated below, and captured by value in the
        // updater closure — reading lastId.current lazily inside the
        // updater would see the post-mutation value once React gets around
        // to invoking it, silently corrupting the count.
        if (detectGap(lastId.current, parsed.id)) {
          const lost = parsed.id - lastId.current - 1
          setEventsLost(n => n + lost)
        }
        gapAttempts.current = 0
        lastId.current = Math.max(lastId.current, parsed.id)
        setEvents(prev => mergeEvents(prev, parsed))
      }
    }

    connect()
    // React 18 StrictMode runs this effect mount → cleanup → mount once in
    // dev. torndown stops a reconnect (from detectGap or a future retry)
    // scheduled by the first mount's EventSource from racing the second
    // mount's; closing here always tears down whichever EventSource is
    // current, and clearing backoffTimer stops a pending gap-reconnect from
    // firing after teardown, so no connection outlives the effect that owns
    // it.
    return () => {
      torndown = true
      clearTimeout(backoffTimer)
      source.close()
    }
    // onUnauthorized must be a stable (useCallback-wrapped) reference -- see
    // the function doc comment above.
  }, [onUnauthorized])

  return { events, connected, eventsLost }
}
