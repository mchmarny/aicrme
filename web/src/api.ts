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

/**
 * establishSession exchanges the launch token for a session cookie.
 *
 * Called once on load with the ?t= value, which App then strips from the
 * visible URL. Everything afterwards -- including the EventSource timeline,
 * which cannot attach request headers -- authenticates by the cookie this
 * sets. See internal/api/token.go.
 */
export async function establishSession(token: string): Promise<void> {
  const res = await fetch('/api/session', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ token }),
  })
  if (!res.ok) throw new ApiError(res.status, 'This launch token was not accepted')
}

/**
 * probeSession reports whether the cookie from a previous load is still good.
 *
 * This is the reload path: a restored tab has no ?t= in its URL, and the
 * cookie is the only thing that can authenticate it. It also still does its
 * original job of telling "server gone" from "reconnecting", because
 * EventSource surfaces no HTTP status on error.
 */
export async function probeSession(): Promise<boolean> {
  const res = await fetch('/api/session')
  return res.status === 204
}

/** ContextInfo mirrors Go's console.ContextInfo (internal/console/connect.go). */
export interface ContextInfo {
  name: string
  server: string
  current: boolean
}

/**
 * fetchContexts lists what the operator can connect to. The server reads the
 * kubeconfig and contacts no cluster, so this returns promptly even when most
 * of the listed contexts are unreachable from wherever the operator is
 * sitting.
 */
export async function fetchContexts(): Promise<ContextInfo[]> {
  const res = await fetch('/api/contexts')
  if (!res.ok) throw new ApiError(res.status, 'Failed to read your kubeconfig')
  return res.json()
}

/**
 * NodeGroup mirrors Go's console.NodeGroup: a fold of every node sharing a
 * shape and a scheduling constraint. The server folds, deliberately — a
 * cluster can have hundreds of nodes and this screen must not grow with it.
 */
export interface NodeGroup {
  count: number
  instanceType?: string
  accelerator?: string
  gpusPerNode?: number
  taints?: string[]
  /** blocked: has GPUs, and carries a taint the snapshot agent cannot tolerate. */
  blocked?: boolean
  /** simulated: a KWOK fake node, unreachable by design rather than by mistake. */
  simulated?: boolean
}

/** NodeComposition mirrors Go's console.NodeComposition. */
export interface NodeComposition {
  total: number
  gpuNodes: number
  groups?: NodeGroup[]
  /** more: shapes beyond the display cap, counted rather than dropped. */
  more?: number
  /** remedy: the AICRME_GPU_TOLERATIONS value that would clear every block. */
  remedy?: string
}

/** ClusterInfo mirrors Go's console.ClusterInfo (internal/console/connect.go). */
export interface ClusterInfo {
  context: string
  server: string
  version: string
  nodeCount: number
  nodes: NodeComposition
  uid: string
  toolchain?: Record<string, string>
  /**
   * recoveredRun is the run this cluster's store was holding, recovered
   * during the connect that produced this response. In-cluster the pod
   * restart triggered recovery and the console simply found a run waiting;
   * locally there is no restart, so it arrives here or not at all.
   */
  recoveredRun?: Run
}

/**
 * connect establishes this process's one cluster connection. A second call
 * conflicts: switching clusters means restarting the binary, because the run
 * directory, the frozen kubeconfig and every cluster client are all keyed on
 * the first answer.
 */
/**
 * currentCluster reports the connection this console already has, or null.
 *
 * This is the reload path's other half. A restored tab has the session cookie
 * and no memory of which cluster it was on, and the connection is
 * single-assignment -- so asking to connect again answers 409 and strands the
 * operator on a screen that will not let them past. Asking first is what
 * skips it.
 */
export async function currentCluster(): Promise<ClusterInfo | null> {
  const res = await fetch('/api/cluster')
  if (res.status === 409) return null
  if (!res.ok) throw new ApiError(res.status, 'Failed to read this console’s cluster connection')
  return res.json()
}

export async function connect(contextName: string): Promise<ClusterInfo> {
  const res = await fetch('/api/connect', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ context: contextName }),
  })
  if (!res.ok) throw new ApiError(res.status, 'Failed to connect to that cluster')
  return res.json()
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
