import { useState } from 'react'
import { Login } from './components/Login'
import { Timeline } from './components/Timeline'
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
      {authed ? <Console /> : <Login onSuccess={() => setAuthed(true)} />}
    </div>
  )
}

function Console() {
  const { events, connected, eventsLost } = useEvents()
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
      <Timeline events={events} />
    </main>
  )
}
