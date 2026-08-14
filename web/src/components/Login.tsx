import { useState } from 'react'
import { login } from '../api'

export function Login({ onSuccess }: { onSuccess: () => void }) {
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    try {
      await login('admin', password)
      onSuccess()
    } catch (err) {
      setError((err as Error).message)
    }
  }

  return (
    <form onSubmit={submit} className="mx-auto mt-32 w-80 space-y-4">
      <h1 className="text-2xl font-semibold text-slate-100">aicrme</h1>
      <input
        type="password" value={password} onChange={e => setPassword(e.target.value)}
        placeholder="Password" aria-label="Password"
        className="w-full rounded border border-slate-700 bg-slate-900 px-3 py-2 text-slate-100"
      />
      {error && <p className="text-red-400 text-sm">{error}</p>}
      <button type="submit" className="w-full rounded bg-emerald-600 py-2 text-white">Sign in</button>
    </form>
  )
}
