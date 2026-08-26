import { useState } from 'react'
import type { AicrEvent } from '../useEvents'

const levelClass: Record<AicrEvent['level'], string> = {
  info: 'text-slate-300',
  warn: 'text-amber-400',
  error: 'text-red-400',
}

/**
 * isRoutineCluster marks the pod chatter that crowds out the install.
 *
 * Measured on a real GKE run: 289 of 397 events were cluster-kind, 73% of the
 * stream, against 90 component and phase events that actually described what
 * was being installed. The bulk is DNSConfigForming repeating per pod.
 *
 * Filtered on kind AND level together, never kind alone. A cluster event at
 * warn or above is the pod that would not schedule -- the single most useful
 * line on the screen when a gang fails to place -- so it survives the filter
 * that removes its routine siblings.
 */
function isRoutineCluster(e: AicrEvent): boolean {
  return e.kind === 'cluster' && e.level === 'info'
}

/**
 * renderMessage promotes backtick-delimited spans to <code>.
 *
 * Remedies arrive embedded in prose -- the gang-placement timeout carries two
 * kubectl commands inside a wrapped red paragraph -- and rendered as flat text
 * an operator cannot tell where the sentence stops and the command starts.
 */
function renderMessage(message: string) {
  return message.split(/`([^`]+)`/g).map((part, i) =>
    i % 2 === 1
      ? <code key={i} className="rounded bg-slate-800 px-1 text-slate-100">{part}</code>
      : <span key={i}>{part}</span>,
  )
}

/**
 * Timeline renders the event stream newest first.
 *
 * It appended until 2026-08-23, which meant that during a 14-action install
 * the line describing what was happening NOW sat below the fold, and the
 * operator had to scroll to follow a live run (docs/ux-feedback.md item 1,
 * observed during a real demo). Newest first costs the chronological reading
 * -- "phase started" now sits below "phase complete" -- and buys never having
 * to scroll during the five minutes the demo exists to be watched.
 *
 * Sorted by the bus's own monotonic id rather than trusting array order: a
 * tab joining late replays from the ring buffer before live events resume,
 * and nothing guarantees the two interleave in id order. On a copy, because
 * sort mutates and this array is Wizard's, shared with other views.
 *
 * Scoped to one run when the caller knows which. The SPA subscribes at
 * ?since=0 and the bus replays its whole ring, so without scoping a run that
 * failed an hour ago renders flush against the run in progress, in identical
 * styling and with no boundary. On real hardware that put a wall of red
 * timeout text from a dead run directly beneath the live run's progress.
 * Earlier runs are collapsed rather than dropped -- they are still the
 * evidence for the state the cluster is in.
 */
export function Timeline({ events, runId }: { events: AicrEvent[]; runId?: string }) {
  const [showEarlier, setShowEarlier] = useState(false)
  const [showCluster, setShowCluster] = useState(false)

  if (events.length === 0) {
    return <p className="text-slate-500 text-sm">Waiting for events…</p>
  }

  // An event with no runId belongs to the session rather than to a run --
  // connect and recovery both emit them -- so it is never "earlier history".
  const earlier = runId ? events.filter(e => e.runId && e.runId !== runId) : []
  const scoped = runId ? events.filter(e => !e.runId || e.runId === runId) : events

  const shown = (showEarlier ? [...scoped, ...earlier] : scoped)
    .filter(e => showCluster || !isRoutineCluster(e))
  const hiddenCluster = events.some(isRoutineCluster)

  const newestFirst = [...shown].sort((a, b) => b.id - a.id)
  return (
    <div className="space-y-2">
      <ol className="font-mono text-sm space-y-1">
        {newestFirst.map(e => (
          <li key={e.id} data-testid={`event-${e.id}`} className={levelClass[e.level]}>
            <span className="text-slate-600 mr-2">{new Date(e.at).toLocaleTimeString()}</span>
            {e.component && <span className="text-slate-400 mr-2">[{e.component}]</span>}
            {renderMessage(e.message)}
          </li>
        ))}
      </ol>

      <div className="flex flex-wrap gap-3 text-xs">
        {hiddenCluster && (
          <button
            type="button"
            onClick={() => setShowCluster(v => !v)}
            className="text-slate-500 underline decoration-dotted"
          >
            {showCluster ? 'hide cluster activity' : 'show cluster activity'}
          </button>
        )}
        {earlier.length > 0 && !showEarlier && (
          <button
            type="button"
            onClick={() => setShowEarlier(true)}
            className="text-slate-500 underline decoration-dotted"
          >
            {`${earlier.length} events from earlier runs`}
          </button>
        )}
      </div>
    </div>
  )
}
