import type { AicrEvent } from '../useEvents'

const levelClass: Record<AicrEvent['level'], string> = {
  info: 'text-slate-300',
  warn: 'text-amber-400',
  error: 'text-red-400',
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
 */
export function Timeline({ events }: { events: AicrEvent[] }) {
  if (events.length === 0) {
    return <p className="text-slate-500 text-sm">Waiting for events…</p>
  }
  const newestFirst = [...events].sort((a, b) => b.id - a.id)
  return (
    <ol className="font-mono text-sm space-y-1">
      {newestFirst.map(e => (
        <li key={e.id} data-testid={`event-${e.id}`} className={levelClass[e.level]}>
          <span className="text-slate-600 mr-2">{new Date(e.at).toLocaleTimeString()}</span>
          {e.component && <span className="text-slate-400 mr-2">[{e.component}]</span>}
          {e.message}
        </li>
      ))}
    </ol>
  )
}
