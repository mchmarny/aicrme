import type { AicrEvent } from '../useEvents'

const levelClass: Record<AicrEvent['level'], string> = {
  info: 'text-slate-300',
  warn: 'text-amber-400',
  error: 'text-red-400',
}

export function Timeline({ events }: { events: AicrEvent[] }) {
  if (events.length === 0) {
    return <p className="text-slate-500 text-sm">Waiting for events…</p>
  }
  return (
    <ol className="font-mono text-sm space-y-1">
      {events.map(e => (
        <li key={e.id} data-testid={`event-${e.id}`} className={levelClass[e.level]}>
          <span className="text-slate-600 mr-2">{new Date(e.at).toLocaleTimeString()}</span>
          {e.component && <span className="text-slate-400 mr-2">[{e.component}]</span>}
          {e.message}
        </li>
      ))}
    </ol>
  )
}
