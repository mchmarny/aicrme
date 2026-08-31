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
/**
 * ordered puts the current-context first and leaves the rest alphabetical.
 *
 * The server sorts by name, deliberately, so two consecutive loads of the
 * same kubeconfig cannot list it differently -- that ordering is kept here
 * for everything except the one row the operator is overwhelmingly likely to
 * want. On the laptop this was built against, 144 contexts sorted the
 * preselected one to row 89: the screen opened on eighty-eight unselected
 * radios with the actual selection off-screen, which reads as "nothing is
 * selected, pick one from this wall".
 */
/**
 * filterThreshold is where a list stops being readable and starts needing a
 * search box. Below it the input is pure chrome; above it the screen is a
 * wall. Six is roughly where a glance stops working.
 */
const filterThreshold = 6

export function ordered(contexts: ContextInfo[]): ContextInfo[] {
  const current = contexts.filter(c => c.current)
  return [...current, ...contexts.filter(c => !c.current)]
}

/**
 * matches is the filter predicate: substring, case-insensitive, over the two
 * fields on screen. Deliberately not fuzzy -- an operator filtering 144
 * clusters is usually typing a fragment they already know ("uat", "us-east"),
 * and a fuzzy match that surfaces near-misses above the exact one would make
 * the wrong cluster easier to hit.
 */
export function matches(c: ContextInfo, query: string): boolean {
  const q = query.trim().toLowerCase()
  if (!q) return true
  return c.name.toLowerCase().includes(q) || (c.server ?? '').toLowerCase().includes(q)
}

/**
 * contextLabel splits a context name into the part that identifies the cluster
 * and the boilerplate in front of it.
 *
 * `aws eks update-kubeconfig` writes ARNs, so every row on an AWS engineer's
 * screen begins with the same ~40 characters — `arn:aws:eks:us-east-1:
 * 615299774277:cluster/` — and the only distinguishing part is last, where a
 * wrap puts it on a second line. Observed on real EKS 2026-08-30.
 *
 * The full name is never hidden: it stays in the title and is what the filter
 * matches, so nothing an operator might search for disappears.
 */
export function contextLabel(name: string): { lead: string; prefix?: string } {
  const arn = /^(arn:aws:[^:]*:[^:]*:[^:]*:cluster\/)(.+)$/.exec(name)
  if (arn) return { lead: arn[2], prefix: arn[1] }
  return { lead: name }
}

export function Connect({ onConnected }: { onConnected: (info: ClusterInfo) => void }) {
  const [contexts, setContexts] = useState<ContextInfo[] | null>(null)
  const [selected, setSelected] = useState('')
  const [query, setQuery] = useState('')
  const [info, setInfo] = useState<ClusterInfo | null>(null)
  const [connecting, setConnecting] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    let canceled = false
    fetchContexts()
      .then(list => {
        if (canceled) return
        setContexts(ordered(list))
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

  const visible = (contexts ?? []).filter(c => matches(c, query))

  if (info) return <Confirm info={info} onContinue={() => onConnected(info)} />

  return (
    <form onSubmit={submit} className="mx-auto mt-24 w-full max-w-3xl px-8 space-y-4">
      <div className="flex items-center gap-3">
        <img src="/aicr-mark.png" alt="" className="h-9 w-9 rounded" />
        <h1 className="text-2xl font-semibold text-ink-strong">Connect a cluster</h1>
      </div>
      <p className="text-ink-soft text-sm">
        aicrme drives the cluster with your own credentials, for as long as it runs.
      </p>

      {contexts === null && !error && <p className="text-ink-faint text-sm">Reading your kubeconfig…</p>}
      {contexts?.length === 0 && (
        <p className="text-warn text-sm">Your kubeconfig has no contexts.</p>
      )}

      {contexts && contexts.length > filterThreshold && (
        <input
          type="text"
          aria-label="Filter contexts"
          placeholder={`Filter ${contexts.length} contexts…`}
          value={query}
          onChange={e => setQuery(e.target.value)}
          className="w-full rounded border border-line bg-panel px-3 py-2 text-sm text-ink placeholder:text-ink-faint"
        />
      )}

      {contexts && contexts.length > 0 && visible.length === 0 && (
        <p className="text-ink-soft text-sm">No context matches “{query}”.</p>
      )}

      {visible.length > 0 && (
        /* Capped and scrollable rather than infinite: the Connect button has
           to stay reachable without paging past every cluster the operator
           has ever touched. */
        <ul className="max-h-[26rem] space-y-2 overflow-y-auto pr-1">
          {visible.map(c => (
            <li key={c.name}>
              {/* Name over server, not beside it. Side by side, a full EKS
                  ARN wrapped to three lines while short names wrapped to two
                  and the server URL -- the lower-value field -- won the
                  horizontal argument. */}
              <label className="flex cursor-pointer items-start gap-3 rounded border border-line bg-panel px-3 py-2">
                <input
                  type="radio" name="context" value={c.name}
                  checked={selected === c.name}
                  onChange={() => setSelected(c.name)}
                  className="mt-1 shrink-0"
                />
                <span className="min-w-0 flex-1">
                  <span className="flex items-baseline gap-2">
                    <span className="min-w-0 font-mono text-sm" title={c.name}>
                      {contextLabel(c.name).prefix && (
                        <span className="text-ink-faint">{contextLabel(c.name).prefix}</span>
                      )}
                      <span className="break-all text-ink-strong">{contextLabel(c.name).lead}</span>
                    </span>
                    {c.current && (
                      <span className="shrink-0 rounded bg-accent/15 px-1.5 text-xs text-accent">current</span>
                    )}
                  </span>
                  <span className="block truncate text-xs text-ink-faint">{c.server}</span>
                </span>
              </label>
            </li>
          ))}
        </ul>
      )}

      {error && <p className="text-fail text-sm">{error}</p>}

      <button
        type="submit"
        disabled={!selected || connecting}
        className="w-full rounded bg-accent py-2 font-medium text-bg disabled:opacity-50"
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
 *
 * Exported: App renders this directly for a reload's 'connected' stage,
 * where a cluster is already chosen and there is no context list to walk
 * back through -- Connect's own fresh-connect path is not the only way to
 * reach it.
 */
export function Confirm({ info, onContinue }: { info: ClusterInfo; onContinue: () => void }) {
  const tools = Object.entries(info.toolchain ?? {}).sort(([a], [b]) => a.localeCompare(b))
  return (
    <div className="mx-auto mt-24 w-full max-w-3xl px-8 space-y-4">
      <h1 className="text-2xl font-semibold text-ink-strong">Connected</h1>
      {/* Four kinds of fact were rendered at one weight, separated only by
          whitespace: what the cluster is, what it is made of, what this run
          will tolerate, and what is installed on the operator's own laptop.
          Nothing said which was which. */}
      <SectionLabel>Cluster</SectionLabel>
      <dl className="space-y-1 text-sm">
        <Row label="context" value={info.context} />
        <Row label="server" value={info.server} />
        <Row label="version" value={info.version} />
        <Row label="cluster" value={info.uid} />
      </dl>
      <Composition nodes={info.nodes} />
      {tools.length > 0 && (
        <SectionLabel>This machine</SectionLabel>
      )}
      {tools.length > 0 && (
        <dl className="space-y-1 text-sm">
          {tools.map(([name, version]) => <Row key={name} label={name} value={version} />)}
        </dl>
      )}
      {info.registryWarning && (
        <div className="rounded border border-warn/40 bg-warn/10 px-3 py-2 text-xs">
          <p className="text-warn">Helm cannot resolve registry credentials</p>
          <p className="mt-1 text-warn">{info.registryWarning}</p>
        </div>
      )}
      {info.recoveredRun && (
        <p className="text-warn text-sm">
          A previous run on this cluster was interrupted and has been recovered.
        </p>
      )}
      <button onClick={onContinue} className="w-full rounded bg-accent py-2 font-medium text-bg">
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
        <span className="w-20 shrink-0 text-ink-faint">nodes</span>
        <span className="text-ink">
          {`${nodes.total} total · ${nodes.gpuNodes} advertising GPUs`}
        </span>
      </div>

      {groups.length > 0 && (
        <ul className="divide-y divide-line rounded border border-line bg-panel">
          {groups.map((g, i) => <GroupRow key={i} group={g} />)}
        </ul>
      )}

      {nodes.more ? (
        <p className="text-ink-faint text-xs">{`and ${nodes.more} more shapes`}</p>
      ) : null}

      {nodes.tolerating && (
        <div className="rounded border border-line bg-panel px-3 py-2 text-xs">
          <p className="text-ink-soft">
            Your GPU nodes are tainted. This run will tolerate:
          </p>
          <code className="mt-1 block break-all text-ink">{nodes.tolerating}</code>
        </div>
      )}

      {/* The install-cannot-start condition, stated before the button rather
          than discovered component by component. On EKS 2026-08-30 every node
          carried an untolerated taint, the first chart sat Unschedulable for
          3m23s, and nothing had said so. Distinct from the GPU remedy above:
          that one aicrme can fix with its own tolerations, this one it cannot
          — the charts are somebody else's. */}
      {nodes.total > 0 && nodes.untainted === 0 && (
        <div data-testid="nothing-schedulable" className="rounded border border-fail/40 bg-fail/10 px-3 py-2 text-xs">
          <p className="text-fail">
            No node accepts a workload without tolerations, so the recipe's components
            will not schedule and the install will stall on the first one.
          </p>
          <p className="mt-1 text-ink-soft">
            Every one of the {nodes.total} nodes is tainted. Remove the taint from the nodes
            that should run platform components, or the install cannot proceed.
          </p>
        </div>
      )}

      {nodes.remedy && (
        <div className="rounded border border-warn/40 bg-warn/10 px-3 py-2 text-xs">
          <p className="text-warn">
            Quit and relaunch to reach them:
          </p>
          <code className="mt-1 block break-all text-warn">
            {`AICRME_GPU_TOLERATIONS=${nodes.remedy}`}
          </code>
        </div>
      )}
    </div>
  )
}

/** SectionLabel groups the Confirm screen's four kinds of fact. */
function SectionLabel({ children }: { children: React.ReactNode }) {
  return (
    <h2 className="pt-2 text-xs uppercase tracking-wide text-ink-faint">{children}</h2>
  )
}

function GroupRow({ group }: { group: NodeGroup }) {
  const hasGPUs = (group.gpusPerNode ?? 0) > 0
  return (
    /* The GPU row decides whether this cluster can do the job at all, and it
       read exactly like the two "no GPUs" rows around it. Accent on the count
       is the cheapest way to make the answer findable in one glance. */
    <li className={`px-3 py-2 text-xs ${hasGPUs ? 'bg-accent/5' : ''}`}>
      <div className="flex items-baseline justify-between gap-2">
        <span className={hasGPUs ? 'text-ink-strong' : 'text-ink-soft'}>
          {`${group.count} × ${group.instanceType || 'unlabelled'}`}
        </span>
        <span className={`shrink-0 ${hasGPUs ? 'text-accent' : 'text-ink-faint'}`}>
          {/* "none advertised", not "no GPUs". This reads node capacity, and
              a p5.48xlarge with eight H100s advertises nothing until a device
              plugin lands -- so the screen told an operator on real EKS
              hardware that their GPU nodes had no GPUs. The new wording is
              true of a CPU node and of a GPU node that is not yet configured,
              which is precisely the state this console exists to fix. */}
          {hasGPUs ? `${group.gpusPerNode} GPU each` : 'none advertised'}
        </span>
      </div>

      {group.accelerator && <p className="text-ink-faint">{group.accelerator}</p>}

      {/* Simulated is stated rather than warned about: a KWOK fake node is
          unreachable on purpose, and treating it as a fault would put a
          warning on every demo run. */}
      {group.simulated && <p className="text-ink-faint">simulated (KWOK)</p>}

      {group.blocked && (
        <p className="mt-1 text-warn">
          {group.taints?.join(', ')} — not tolerated, the agent cannot land here
        </p>
      )}
    </li>
  )
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex gap-3">
      <dt className="w-20 shrink-0 text-ink-faint">{label}</dt>
      <dd className="text-ink">{value}</dd>
    </div>
  )
}
