import { useCallback, useEffect, useState } from 'react'
import { ApiError, currentCluster, establishSession, probeSession, startRun, type ClusterInfo } from './api'
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
  // Held for the header. Connect answers "which cluster" and then every later
  // screen forgot it, including the gate that grants cluster-admin -- see the
  // ClusterBadge doc comment.
  const [cluster, setCluster] = useState<ClusterInfo | null>(null)

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
        throw new Error('This console session has expired. Re-open the tokenized URL aicrme printed at startup — the one ending in ?t=…')
      }
      // A reload after connecting lands here with a cluster already chosen.
      // Skipping Connect is not a convenience: the connection is
      // single-assignment, so a second attempt would answer 409 and leave the
      // operator on a screen that will not let them past.
      // The response, not just its truthiness: this is the reload path, and
      // the header needs the same cluster identity a fresh connect provides.
      const connected = await currentCluster()
      if (!connected) return 'connecting'
      setCluster(connected)
      return 'console'
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
    <div className="min-h-screen flow-root bg-bg text-ink-strong">
      {authError && <p className="mx-auto mt-32 w-[28rem] text-fail text-sm">{authError}</p>}
      {!authError && stage === 'authenticating' && (
        <p className="mx-auto mt-32 w-[28rem] text-ink-faint text-sm">Starting…</p>
      )}
      {stage === 'connecting' && (
        <Connect onConnected={info => { setCluster(info); setStage('console') }} />
      )}
      {/* A 401 on the event stream means this cookie is not recognized, which
          in a process-lifetime session means the process that minted it is
          gone. There is nothing to retry and no token left in the URL to
          re-exchange, so the console says so rather than looping. */}
      {stage === 'console' && (
        <Console
          cluster={cluster}
          onUnauthorized={() => {
            setStage('authenticating')
            setAuthError('This console session has expired. Re-open the tokenized URL aicrme printed at startup — the one ending in ?t=…')
          }}
        />
      )}
    </div>
  )
}

/**
 * ClusterBadge names the cluster every screen after Connect is acting on.
 *
 * Connect exists to answer "which cluster", and the answer used to stop being
 * visible the moment it was given: from there the header said only
 * "connected", including on the confirm gate where the operator grants
 * cluster-admin to install fourteen components. The kubeconfig this was built
 * against holds 144 contexts, so "am I pointed at the right one" stays a live
 * question long past the screen that asked it.
 *
 * Context name and cluster-wide GPU count, because those are the two facts
 * that distinguish the intended cluster from its neighbours -- and both are
 * already computed at connect. Truncated with the full value in `title`: a GKE
 * context name runs to sixty characters and the header is not where it earns
 * its space.
 */
function ClusterBadge({ cluster }: { cluster: ClusterInfo | null }) {
  if (!cluster) return null
  const gpus = cluster.nodes?.totalGPUs
  return (
    <span className="flex min-w-0 items-baseline gap-2 text-xs text-ink-faint">
      <span className="truncate font-mono text-ink-soft" title={cluster.context}>
        {cluster.context}
      </span>
      {gpus ? <span className="shrink-0">{gpus} GPUs</span> : null}
    </span>
  )
}

function Console({ cluster, onUnauthorized }: { cluster: ClusterInfo | null; onUnauthorized: () => void }) {
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
        {/* The AICR mark is matted onto its own dark backdrop rather than
            being transparent, so it is rendered as a small rounded tile --
            treating it as a free-floating glyph would show its square
            edge against a page background it does not match. */}
        <img src="/aicr-mark.png" alt="" className="h-7 w-7 rounded" />
        <h1 className="text-xl font-semibold text-ink-strong">aicrme</h1>
        <span className={connected ? 'text-pass text-xs' : 'text-ink-faint text-xs'}>
          {connected ? 'connected' : 'reconnecting…'}
        </span>
        <ClusterBadge cluster={cluster} />
        {/* Pushed right, away from the cluster identity: this describes the
            binary, not the cluster, and sitting them together invites reading
            it as something discovered from the cluster. Links to the release
            notes for the exact version whose decisions are on screen. */}
        {cluster?.aicrVersion && (
          <a
            href={`https://github.com/NVIDIA/aicr/releases/tag/${cluster.aicrVersion}`}
            target="_blank"
            rel="noreferrer"
            title="The AICR release this console is built against"
            data-testid="aicr-version"
            className="text-ink-faint hover:text-accent ml-auto font-mono text-xs underline underline-offset-4"
          >
            AICR {cluster.aicrVersion}
          </a>
        )}
      </header>
      {eventsLost > 0 && (
        <p className="mb-4 text-warn text-xs">
          {eventsLost} event{eventsLost === 1 ? '' : 's'} could not be recovered after a connection gap.
        </p>
      )}
      {startError && (
        <div className="mb-4 space-y-2">
          <p className="text-fail text-sm">{startError}</p>
          <button
            onClick={() => setRetryToken(n => n + 1)}
            className="rounded border border-line px-3 py-1 text-sm text-ink"
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
        // NOT wired to Stop. A successful Stop used to bump retryToken, which
        // re-ran the effect above and started a new run immediately -- so the
        // finished run lost the `current` slot, and with it the only path to
        // Reset (engine.Reset refuses any run that is not current). Observed on
        // real H100s 2026-08-29: Stop left 16 releases installed and no way in
        // the console to remove them. Starting a new run is now an explicit
        // action on the stopped screen.
        onStartNewRun={() => setRetryToken(n => n + 1)}
      />
    </main>
  )
}
