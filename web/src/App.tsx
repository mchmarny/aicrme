import { useState } from 'react'
import { Login } from './components/Login'
import { Timeline } from './components/Timeline'
import { useEvents } from './useEvents'

export default function App() {
  const [authed, setAuthed] = useState(false)
  if (!authed) return <Login onSuccess={() => setAuthed(true)} />
  return <Console />
}

function Console() {
  const { events, connected } = useEvents()
  return (
    <main className="min-h-screen bg-slate-950 p-8 text-slate-100">
      <header className="mb-6 flex items-center gap-3">
        <h1 className="text-xl font-semibold">aicrme</h1>
        <span className={connected ? 'text-emerald-400 text-xs' : 'text-slate-500 text-xs'}>
          {connected ? 'connected' : 'reconnecting…'}
        </span>
      </header>
      <Timeline events={events} />
    </main>
  )
}
