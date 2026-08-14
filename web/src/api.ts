export async function login(username: string, password: string): Promise<void> {
  const res = await fetch('/api/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password }),
  })
  if (!res.ok) throw new Error(res.status === 429 ? 'Too many attempts' : 'Invalid credentials')
}

/** Run mirrors Go's engine.Run field-for-field (internal/engine/run.go). Artifacts is Go json:"-" and never appears here. */
export interface Run {
  id: string
  state: 'idle' | 'running' | 'awaiting_decision' | 'failed' | 'active' | 'done'
  phase: string
  decisions: Record<string, string>
  pending?: string[]
  error?: string
  startedAt: string
  updatedAt: string
}

export async function startRun(): Promise<Run> {
  const res = await fetch('/api/runs', { method: 'POST' })
  if (!res.ok) throw new Error('Failed to start run')
  return res.json()
}

/**
 * Options mirrors Go's aicrclient.Options (internal/aicrclient/options.go).
 * See internal/api/options.go's handleOptions doc comment for the client
 * contract this response carries: PlatformsByIntent is what a caller must
 * render from, and a caller must re-fetch once the run reaches
 * awaiting_decision rather than caching a mount-time (likely provisional)
 * answer.
 */
export interface Options {
  intents: string[]
  platforms: string[]
  platformsByIntent: Record<string, string[]>
  provisional: boolean
}

export async function fetchOptions(): Promise<Options> {
  const res = await fetch('/api/options')
  if (!res.ok) throw new Error('Failed to fetch options')
  return res.json()
}

export async function decide(runId: string, decisions: Record<string, string>): Promise<Run> {
  const res = await fetch(`/api/runs/${encodeURIComponent(runId)}/decide`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(decisions),
  })
  if (!res.ok) throw new Error('Failed to submit decision')
  return res.json()
}
