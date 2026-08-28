import { useEffect, useState } from 'react'
import { connect, fetchContexts, type ClusterInfo, type ContextInfo, type NodeComposition, type NodeGroup } from '../api'

/**
 * Connect is the screen that stands where the password prompt used to.
 *
 * It answers the one question the in-cluster console never had to ask: which
 * cluster. A pod inherited that from its ServiceAccount and could not be
 * pointed anywhere else; a binary on an operator's laptop can reach every
 * cluster in their kubeconfig, and picking the wrong one installs fourteen
 * components somewhere nobody meant.
 *
 * That is why it is two steps rather than one. Choosing a context is a guess
 * about a label in a file; the confirmation that follows reports what the
 * cluster actually said -- its server version, how many nodes it has, and the
 * bash/jq/helm/kubectl this machine resolved -- and only then offers to go on.
 */
export function Connect({ onConnected }: { onConnected: (info: ClusterInfo) => void }) {
  const [contexts, setContexts] = useState<ContextInfo[] | null>(null)
  const [selected, setSelected] = useState('')
  const [info, setInfo] = useState<ClusterInfo | null>(null)
  const [connecting, setConnecting] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    let canceled = false
    fetchContexts()
      .then(list => {
        if (canceled) return
        setContexts(list)
        // Preselected, not auto-connected. The kubeconfig's current-context is
        // the operator's best guess at what they meant, and it is still a
        // guess -- connecting on their behalf would make the first thing this
        // console does an unreviewed choice of cluster.
        setSelected(list.find(c => c.current)?.name ?? list[0]?.name ?? '')
      })
      .catch(err => {
        if (!canceled) setError(err instanceof Error ? err.message : 'Failed to read your kubeconfig')
      })
    return () => { canceled = true }
  }, [])

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    if (!selected) return
    setConnecting(true)
    setError('')
    try {
      setInfo(await connect(selected))
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to connect to that cluster')
    } finally {
      setConnecting(false)
    }
  }

  if (info) return <Confirm info={info} onContinue={() => onConnected(info)} />

  return (
    <form onSubmit={submit} className="mx-auto mt-32 w-[28rem] space-y-4">
      <h1 className="text-2xl font-semibold text-slate-100">Connect a cluster</h1>
      <p className="text-slate-400 text-sm">
        aicrme drives the cluster with your own credentials, for as long as it runs.
      </p>

      {contexts === null && !error && <p className="text-slate-500 text-sm">Reading your kubeconfig…</p>}
      {contexts?.length === 0 && (
        <p className="text-amber-400 text-sm">Your kubeconfig has no contexts.</p>
      )}

      {contexts && contexts.length > 0 && (
        <ul className="space-y-2">
          {contexts.map(c => (
            <li key={c.name}>
              <label className="flex cursor-pointer items-baseline gap-3 rounded border border-slate-700 bg-slate-900 px-3 py-2">
                <input
                  type="radio" name="context" value={c.name}
                  checked={selected === c.name}
                  onChange={() => setSelected(c.name)}
                />
                <span className="text-slate-100">{c.name}</span>
                <span className="text-slate-500 text-xs">{c.server}</span>
                {c.current && <span className="text-slate-500 text-xs">current</span>}
              </label>
            </li>
          ))}
        </ul>
      )}

      {error && <p className="text-red-400 text-sm">{error}</p>}

      <button
        type="submit"
        disabled={!selected || connecting}
        className="w-full rounded bg-emerald-600 py-2 text-white disabled:opacity-50"
      >
        {connecting ? 'Connecting…' : 'Connect'}
      </button>
    </form>
  )
}

/**
 * Confirm reports what the cluster and this machine actually answered.
 *
 * The versions are here rather than only in a log because this is the last
 * screen before something gets installed, and "am I pointed at the right
 * cluster" is a question a server version and a node count answer far better
 * than a context name does.
 */
function Confirm({ info, onContinue }: { info: ClusterInfo; onContinue: () => void }) {
  const tools = Object.entries(info.toolchain ?? {}).sort(([a], [b]) => a.localeCompare(b))
  return (
    <div className="mx-auto mt-32 w-[28rem] space-y-4">
      <h1 className="text-2xl font-semibold text-slate-100">Connected</h1>
      <dl className="space-y-1 text-sm">
        <Row label="context" value={info.context} />
        <Row label="server" value={info.server} />
        <Row label="version" value={info.version} />
        <Row label="cluster" value={info.uid} />
      </dl>
      <Composition nodes={info.nodes} />
      {tools.length > 0 && (
        <dl className="space-y-1 text-sm">
          {tools.map(([name, version]) => <Row key={name} label={name} value={version} />)}
        </dl>
      )}
      {info.recoveredRun && (
        <p className="text-amber-400 text-sm">
          A previous run on this cluster was interrupted and has been recovered.
        </p>
      )}
      <button onClick={onContinue} className="w-full rounded bg-emerald-600 py-2 text-white">
        Continue
      </button>
    </div>
  )
}

/**
 * Composition shows what the cluster is made of, and whether the snapshot
 * agent can reach the GPU part of it.
 *
 * The inventory is always visible; the warning appears only when something is
 * genuinely unreachable. That asymmetry is the point. The first real cluster
 * this console met tainted its GPU pool `dedicated=gpu-workload:NoSchedule`,
 * which the built-in toleration does not match, and the only symptom was
 * Discover sitting Pending for ten minutes before returning a snapshot with no
 * accelerator in it. Nothing named the taint.
 *
 * Connect now derives that taint and adopts it, so the amber remedy block is a
 * last resort rather than the normal path, and the ordinary case is the quiet
 * grey one above it: "this run will tolerate X". Naming it is not decoration.
 * Tolerating a taint is a scheduling decision taken on the operator's behalf,
 * and a taint can exist precisely to keep other people's workloads off a pool
 * -- so the screen says which ones this run will ignore, while nothing has
 * been installed yet and the answer is still cheap to act on.
 */
function Composition({ nodes }: { nodes: NodeComposition }) {
  const groups = nodes.groups ?? []
  return (
    <div className="space-y-2">
      <div className="flex gap-3 text-sm">
        <span className="w-20 shrink-0 text-slate-500">nodes</span>
        <span className="text-slate-200">
          {`${nodes.total} total · ${nodes.gpuNodes} with GPUs`}
        </span>
      </div>

      {groups.length > 0 && (
        <ul className="divide-y divide-slate-800 rounded border border-slate-700 bg-slate-900">
          {groups.map((g, i) => <GroupRow key={i} group={g} />)}
        </ul>
      )}

      {nodes.more ? (
        <p className="text-slate-500 text-xs">{`and ${nodes.more} more shapes`}</p>
      ) : null}

      {nodes.tolerating && (
        <div className="rounded border border-slate-700 bg-slate-900 px-3 py-2 text-xs">
          <p className="text-slate-400">
            Your GPU nodes are tainted. This run will tolerate:
          </p>
          <code className="mt-1 block break-all text-slate-200">{nodes.tolerating}</code>
        </div>
      )}

      {nodes.remedy && (
        <div className="rounded border border-amber-700/50 bg-amber-950/30 px-3 py-2 text-xs">
          <p className="text-amber-400">
            Quit and relaunch to reach them:
          </p>
          <code className="mt-1 block break-all text-amber-200">
            {`AICRME_GPU_TOLERATIONS=${nodes.remedy}`}
          </code>
        </div>
      )}
    </div>
  )
}

function GroupRow({ group }: { group: NodeGroup }) {
  const hasGPUs = (group.gpusPerNode ?? 0) > 0
  return (
    <li className="px-3 py-2 text-xs">
      <div className="flex items-baseline justify-between gap-2">
        <span className={hasGPUs ? 'text-slate-100' : 'text-slate-400'}>
          {`${group.count} × ${group.instanceType || 'unlabelled'}`}
        </span>
        <span className="shrink-0 text-slate-500">
          {hasGPUs ? `${group.gpusPerNode} GPU each` : 'no GPUs'}
        </span>
      </div>

      {group.accelerator && <p className="text-slate-500">{group.accelerator}</p>}

      {/* Simulated is stated rather than warned about: a KWOK fake node is
          unreachable on purpose, and treating it as a fault would put a
          warning on every demo run. */}
      {group.simulated && <p className="text-slate-500">simulated (KWOK)</p>}

      {group.blocked && (
        <p className="mt-1 text-amber-400">
          {group.taints?.join(', ')} — not tolerated, the agent cannot land here
        </p>
      )}
    </li>
  )
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex gap-3">
      <dt className="w-20 shrink-0 text-slate-500">{label}</dt>
      <dd className="text-slate-200">{value}</dd>
    </div>
  )
}
