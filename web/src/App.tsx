import { useCallback, useEffect, useState } from 'react'
import { ApiError, currentCluster, establishSession, probeSession, startRun } from './api'
import { Connect } from './components/Connect'
import { Wizard } from './components/Wizard'
import { useEvents } from './useEvents'

/**
 * launchTokenParam is the query parameter the binary prints in the URL it
 * opens. It is a one-shot credential: App exchanges it for a session cookie
 * and strips it from the address bar immediately, so it never survives into a
 * bookmark, a copied link, or the browser's history.
 */
const launchTokenParam = 't'

type Stage = 'authenticating' | 'connecting' | 'console'

export default function App() {
  const [stage, setStage] = useState<Stage>('authenticating')
  const [authError, setAuthError] = useState('')

  // Two ways in, and both end at the same cookie. A fresh launch arrives with
  // ?t= and exchanges it; a reload, a restored tab, or a retyped address
  // arrives with nothing and has only the cookie the first load set. The
  // second case is what an in-memory token could not serve, and it is the
  // ordinary one after the first minute.
  useEffect(() => {
    let canceled = false
    const url = new URL(window.location.href)
    const token = url.searchParams.get(launchTokenParam)

    async function bootstrap(): Promise<Stage> {
      if (token) {
        await establishSession(token)
        url.searchParams.delete(launchTokenParam)
        window.history.replaceState({}, '', url.pathname + url.search + url.hash)
      } else if (!(await probeSession())) {
        throw new Error('This console session has expired. Restart aicrme and open the URL it prints.')
      }
      // A reload after connecting lands here with a cluster already chosen.
      // Skipping Connect is not a convenience: the connection is
      // single-assignment, so a second attempt would answer 409 and leave the
      // operator on a screen that will not let them past.
      return (await currentCluster()) ? 'console' : 'connecting'
    }

    bootstrap()
      .then(next => { if (!canceled) setStage(next) })
      .catch(err => {
        if (!canceled) setAuthError(err instanceof Error ? err.message : 'Could not start a console session')
      })
    return () => { canceled = true }
  }, [])

  // The dark background and default text colour live here, once, so every
  // screen shares them — Connect's own heading colour class alone has nothing
  // dark to sit on top of (Tailwind's preflight sets no background), which
  // renders it near-white on the browser's default white. flow-root (CSS
  // display: flow-root) matters as much as the colours: without it, Connect's
  // mt-32 on its <form> collapses through this div — a plain block box with
  // no border/padding of its own doesn't contain a child's top margin — and
  // pushes the whole div (background included) down by 8rem, uncovering
  // that much of the page's default white above it. flow-root gives this
  // div its own block formatting context so the margin stays inside it.
  return (
    <div className="min-h-screen flow-root bg-slate-950 text-slate-100">
      {authError && <p className="mx-auto mt-32 w-[28rem] text-red-400 text-sm">{authError}</p>}
      {!authError && stage === 'authenticating' && (
        <p className="mx-auto mt-32 w-[28rem] text-slate-500 text-sm">Starting…</p>
      )}
      {stage === 'connecting' && <Connect onConnected={() => setStage('console')} />}
      {/* A 401 on the event stream means this cookie is not recognized, which
          in a process-lifetime session means the process that minted it is
          gone. There is nothing to retry and no token left in the URL to
          re-exchange, so the console says so rather than looping. */}
      {stage === 'console' && (
        <Console
          onUnauthorized={() => {
            setStage('authenticating')
            setAuthError('This console session has expired. Restart aicrme and open the URL it prints.')
          }}
        />
      )}
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
      {/* A discard clears the engine's recovery gate but publishes no bus
          event, so nothing in the stream tells this effect that POST
          /api/runs has stopped answering 409. Bumping the same token the
          error path uses re-runs it, which is what lets the console start a
          fresh run without the operator reloading the page.

          A stop is the same problem for a different gate: it does publish
          ("run done", via engine.finish), but this effect watches the token,
          not the stream, and the run it just ended was refusing new runs on
          two counts -- the live workload and, for a recovered run, the
          recovery gate Stop clears on success. */}
      <Wizard
        events={events}
        onDiscarded={() => setRetryToken(n => n + 1)}
        onStopped={() => setRetryToken(n => n + 1)}
      />
    </main>
  )
}
