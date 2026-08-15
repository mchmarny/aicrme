import { useCallback, useEffect, useState } from 'react'
import { ApiError, startRun } from './api'
import { Login } from './components/Login'
import { Wizard } from './components/Wizard'
import { useEvents } from './useEvents'

export default function App() {
  const [authed, setAuthed] = useState(false)
  // The dark background and default text colour live here, once, so both
  // screens share them — Login's own heading colour class alone had nothing
  // dark to sit on top of (Tailwind's preflight sets no background), which
  // rendered it near-white on the browser's default white. flow-root (CSS
  // display: flow-root) matters as much as the colours: without it, Login's
  // mt-32 on its <form> collapses through this div — a plain block box with
  // no border/padding of its own doesn't contain a child's top margin — and
  // pushes the whole div (background included) down by 8rem, uncovering
  // that much of the page's default white above it. flow-root gives this
  // div its own block formatting context so the margin stays inside it.
  return (
    <div className="min-h-screen flow-root bg-slate-950 text-slate-100">
      {authed ? <Console onUnauthorized={() => setAuthed(false)} /> : <Login onSuccess={() => setAuthed(true)} />}
    </div>
  )
}

function Console({ onUnauthorized }: { onUnauthorized: () => void }) {
  // useEvents depends on this callback's identity to decide whether to
  // re-run its connection effect (see useEvents.ts's doc comment): wrapping
  // it in useCallback keeps it stable across Console's own re-renders, even
  // though the onUnauthorized prop itself is currently a fresh closure per
  // App render. Without this, the SSE stream would tear down and reopen on
  // every render.
  const handleUnauthorized = useCallback(() => onUnauthorized(), [onUnauthorized])
  const { events, connected, eventsLost } = useEvents(handleUnauthorized)
  const [startError, setStartError] = useState('')
  const [retryToken, setRetryToken] = useState(0)

  // Discover "runs automatically on first load -- no decisions gate it"
  // (see internal/steps/discover.go's doc comment), so the console starts a
  // run itself rather than waiting on a button. A 409 here means a run is
  // already in progress -- e.g. a page reload -- and the SSE stream (backed
  // by the bus's replay ring) already carries its state, so that specific
  // failure is expected and silent. Any other failure (network error, 401
  // from an expired session, a 5xx) previously hit the same
  // `.catch(() => {})` and left the console showing nothing forever with no
  // way to recover short of a manual page reload -- surfaced here instead,
  // with a retry the user can act on.
  useEffect(() => {
    let canceled = false
    setStartError('')
    startRun().catch(err => {
      if (canceled) return
      if (err instanceof ApiError && err.status === 409) return
      setStartError(err instanceof Error ? err.message : 'Failed to start a run')
    })
    return () => { canceled = true }
  }, [retryToken])

  return (
    <main className="p-8">
      <header className="mb-6 flex items-center gap-3">
        <h1 className="text-xl font-semibold">aicrme</h1>
        <span className={connected ? 'text-emerald-400 text-xs' : 'text-slate-500 text-xs'}>
          {connected ? 'connected' : 'reconnecting…'}
        </span>
      </header>
      {eventsLost > 0 && (
        <p className="mb-4 text-amber-400 text-xs">
          {eventsLost} event{eventsLost === 1 ? '' : 's'} could not be recovered after a connection gap.
        </p>
      )}
      {startError && (
        <div className="mb-4 space-y-2">
          <p className="text-red-400 text-sm">{startError}</p>
          <button
            onClick={() => setRetryToken(n => n + 1)}
            className="rounded border border-slate-700 px-3 py-1 text-sm text-slate-200"
          >
            Retry
          </button>
        </div>
      )}
      <Wizard events={events} />
    </main>
  )
}
