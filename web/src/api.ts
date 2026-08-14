export async function login(username: string, password: string): Promise<void> {
  const res = await fetch('/api/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password }),
  })
  if (!res.ok) throw new Error(res.status === 429 ? 'Too many attempts' : 'Invalid credentials')
}

export async function startRun(): Promise<{ id: string }> {
  const res = await fetch('/api/runs', { method: 'POST' })
  if (!res.ok) throw new Error('Failed to start run')
  return res.json()
}
