/**
 * ApiError carries the HTTP status alongside the message, so a caller can
 * tell an expected failure (e.g. 409 "a run is already in progress" on
 * startRun, which a page reload triggers routinely) apart from one worth
 * surfacing to the user.
 */
export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

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
  if (!res.ok) throw new ApiError(res.status, 'Failed to start run')
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
  if (!res.ok) throw new ApiError(res.status, 'Failed to fetch options')
  return res.json()
}

export async function decide(runId: string, decisions: Record<string, string>): Promise<Run> {
  const res = await fetch(`/api/runs/${encodeURIComponent(runId)}/decide`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(decisions),
  })
  if (!res.ok) throw new ApiError(res.status, 'Failed to submit decision')
  return res.json()
}

export async function retryRun(runId: string): Promise<Run> {
  const res = await fetch(`/api/runs/${encodeURIComponent(runId)}/retry`, { method: 'POST' })
  if (!res.ok) throw new ApiError(res.status, 'Failed to retry the run')
  return res.json()
}

/**
 * discardRun drops a recovered run and its persisted ConfigMap record
 * (DELETE /api/runs/{id} -> engine.Discard), which is the only thing that
 * clears the recovery gate for a run Retry refuses: Retry requires
 * StateFailed, so a run recovered in a terminal state -- notably the `done`
 * one every `helm upgrade` of a release that has completed a demo recovers --
 * has no other way out, and POST /api/runs answers 409 until it is gone.
 *
 * 204 No Content on success, so there is no body to decode.
 */
export async function discardRun(runId: string): Promise<void> {
  const res = await fetch(`/api/runs/${encodeURIComponent(runId)}`, { method: 'DELETE' })
  if (!res.ok) throw new ApiError(res.status, 'Failed to discard the run')
}

/**
 * bundleUrl is a plain href rather than a fetch: the browser's own download
 * handling gets the filename from Content-Disposition, and the session
 * cookie rides along on the navigation.
 */
export function bundleUrl(runId: string): string {
  return `/api/runs/${encodeURIComponent(runId)}/bundle`
}
