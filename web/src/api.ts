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
  state: 'idle' | 'running' | 'awaiting_decision' | 'failed' | 'active' | 'done' | 'resetting'
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
 * stopRun is the only exit from an active run (POST /api/runs/{id}/stop ->
 * engine.Stop). Nothing else in this module can end one: Discard is rejected
 * outright while a workload is running, and Retry requires a failed run. The
 * engine deletes the workload, waits until it is actually gone, and only then
 * finishes the run -- so a resolved promise here means the pods, and the GPUs
 * they held, are released.
 *
 * The request can outlast the tab that issued it: the handler detaches its
 * context (internal/api/prove.go) because tearing a GPU workload down on a
 * real cluster takes long enough that a closed tab would otherwise leave the
 * operator with no idea whether it happened.
 */
export async function stopRun(runId: string): Promise<Run> {
  const res = await fetch(`/api/runs/${encodeURIComponent(runId)}/stop`, { method: 'POST' })
  if (!res.ok) throw new ApiError(res.status, 'Failed to stop the workload')
  return res.json()
}

/**
 * bundleUrl is a plain href rather than a fetch: the browser's own download
 * handling gets the filename from Content-Disposition, and the session
 * cookie rides along on the navigation.
 */
export function bundleUrl(runId: string): string {
  return `/api/runs/${encodeURIComponent(runId)}/bundle`
}

/**
 * resetRun tears down what one run installed: its workload, the helm
 * releases it created, and the namespaces it created and left empty
 * (POST /api/runs/{id}/reset -> engine.Reset).
 *
 * The confirmation body is required by the server, not decorative here: it
 * is what stops a URL alone -- a stray retry, a pasted address -- starting
 * a teardown of someone's cluster. See internal/api/reset.go.
 *
 * Resolves as soon as the teardown is accepted and its state persisted, NOT
 * when it finishes: a helm uninstall per component with --wait runs for
 * minutes, so the engine backgrounds the work and the console follows it on
 * the event stream. The request outlives the tab that issued it for the
 * same reason stopRun's does.
 */
export async function resetRun(runId: string): Promise<Run> {
  const res = await fetch(`/api/runs/${encodeURIComponent(runId)}/reset`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ confirm: 'reset' }),
  })
  if (!res.ok) throw new ApiError(res.status, 'Failed to reset the run')
  return res.json()
}
