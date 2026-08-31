import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Connect, contextLabel } from './Connect'
import type { ClusterInfo, NodeComposition } from '../api'

const contexts = [
  { name: 'alpha', server: 'https://alpha.example:6443', current: false },
  { name: 'beta', server: 'https://beta.example:6443', current: true },
]

/**
 * manyContexts is the shape of a working engineer's kubeconfig rather than a
 * test's: the laptop this screen was built against holds 144 contexts, and
 * the current one sorted to row 89 of them. Alphabetical order put the one
 * answer the operator wanted below eighty-eight they did not.
 */
const manyContexts = [
  ...Array.from({ length: 40 }, (_, i) => ({
    name: `aicr-test${String(i).padStart(2, '0')}`,
    server: `https://aicr-test${i}.example:443`,
    current: false,
  })),
  // Shaped like a real GKE context on purpose -- long, underscore-separated,
  // and sorting nowhere near the front. That is the case this fixture exists
  // to cover: the current context is selected because it is current, not
  // because of where it lands in the list.
  { name: 'gke_example-project_us-central1_aicr-demo-0-00000000000', server: 'https://gke.example:443', current: true },
  { name: 'zzz-last', server: 'https://zzz.example:443', current: false },
]

/**
 * taintedNodes is the layout of the GKE cluster this feature was written for:
 * two H100 nodes behind a taint of the platform team's own choosing, beside
 * four ordinary ones.
 *
 * The group is no longer `blocked` and there is no `remedy`: Connect derives
 * that taint from the nodes and adopts it, reporting what it adopted in
 * `tolerating`. This fixture used to carry the opposite, which is what the
 * operator had to fix by hand.
 */
const taintedNodes: NodeComposition = {
  total: 6,
  gpuNodes: 2,
  untainted: 3,
  groups: [
    {
      count: 2,
      instanceType: 'a3-megagpu-8g',
      accelerator: 'nvidia-h100-mega-80gb',
      gpusPerNode: 8,
      taints: ['dedicated=gpu-workload:NoSchedule'],
    },
    { count: 3, instanceType: 'e2-standard-4' },
    { count: 1, instanceType: 'n2-standard-8' },
  ],
  tolerating: 'dedicated=gpu-workload:NoSchedule',
}

const clusterInfo: ClusterInfo = {
  context: 'beta',
  server: 'https://beta.example:6443',
  version: 'v1.31.4',
  nodeCount: 6,
  nodes: taintedNodes,
  uid: '1111-2222',
  toolchain: { helm: 'v3.19.0', kubectl: 'v1.31.0', bash: '5.2.15', jq: '1.7' },
}

/** withNodes swaps the composition, leaving the rest of the response alone. */
function withNodes(nodes: NodeComposition): ClusterInfo {
  return { ...clusterInfo, nodes }
}

/**
 * mockFetch answers the two routes Connect uses. Deliberately a fetch stub
 * rather than a mocked api module: the request shape POST /api/connect sends
 * is part of what this screen has to get right, and mocking the client would
 * make it untestable.
 */
function mockFetch(overrides: {
  connect?: () => Response
  contexts?: () => Response
  survey?: () => Response
} = {}) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = typeof input === 'string' ? input : input.toString()
    if (url === '/api/contexts') {
      return overrides.contexts?.() ?? new Response(JSON.stringify(contexts), { status: 200 })
    }
    if (url === '/api/connect') {
      return overrides.connect?.() ?? new Response(JSON.stringify(clusterInfo), { status: 200 })
    }
    // Defaults to 'unavailable' (surveyCluster treats 503 that way) so every
    // test written before the survey panel existed is unaffected by it.
    if (url === '/api/cluster/survey') {
      return overrides.survey?.() ?? new Response(null, { status: 503 })
    }
    throw new Error(`unexpected fetch: ${url}`)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

describe('Connect', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('lists every context in the kubeconfig with its server', async () => {
    mockFetch()
    render(<Connect onConnected={() => {}} />)

    expect(await screen.findByText('alpha')).toBeDefined()
    expect(screen.getByText('beta')).toBeDefined()
    expect(screen.getByText('https://alpha.example:6443')).toBeDefined()
  })

  // THE ONE THE OPERATOR ALMOST CERTAINLY WANTS GOES FIRST.
  //
  // The list was sorted by name alone, so on a real kubeconfig the
  // preselected current-context sat at row 89 of 144: the screen opened on
  // eighty-eight unselected radios, the selection was off-screen, and the
  // Connect button was below all of them. Selection was correct the whole
  // time and invisible, which is the same as broken.
  it('puts the current context at the top of the list', async () => {
    mockFetch({ contexts: () => new Response(JSON.stringify(manyContexts), { status: 200 }) })
    render(<Connect onConnected={() => {}} />)

    const radios = await screen.findAllByRole('radio')
    expect(radios[0].getAttribute('value')).toBe('gke_example-project_us-central1_aicr-demo-0-00000000000')
    // and it is still the selected one.
    expect((radios[0] as HTMLInputElement).checked).toBe(true)
  })

  // 144 rows is not a list, it is a wall. Filtering is what makes the screen
  // independent of how many clusters the operator has ever touched.
  it('filters the list by name', async () => {
    mockFetch({ contexts: () => new Response(JSON.stringify(manyContexts), { status: 200 }) })
    render(<Connect onConnected={() => {}} />)

    await screen.findAllByRole('radio')
    fireEvent.change(screen.getByRole('textbox', { name: /filter/i }), { target: { value: 'zzz' } })

    const radios = screen.getAllByRole('radio')
    expect(radios).toHaveLength(1)
    expect(radios[0].getAttribute('value')).toBe('zzz-last')
  })

  // Filtering must not silently strand the operator on a selection they can
  // no longer see -- and must say so rather than showing an empty box.
  it('says when a filter matches nothing', async () => {
    mockFetch({ contexts: () => new Response(JSON.stringify(manyContexts), { status: 200 }) })
    render(<Connect onConnected={() => {}} />)

    await screen.findAllByRole('radio')
    fireEvent.change(screen.getByRole('textbox', { name: /filter/i }), { target: { value: 'nothing-matches-this' } })

    expect(screen.queryAllByRole('radio')).toHaveLength(0)
    expect(screen.getByText(/no context matches/i)).toBeDefined()
  })

  // Preselected, never auto-connected: the current-context is the operator's
  // best guess at what they meant, and connecting on their behalf would make
  // the first thing this console does an unreviewed choice of cluster.
  it('preselects the current-context without connecting to it', async () => {
    const fetchMock = mockFetch()
    render(<Connect onConnected={() => {}} />)

    await waitFor(() =>
      expect((screen.getByRole('radio', { name: /beta/ }) as HTMLInputElement).checked).toBe(true))
    expect(fetchMock).not.toHaveBeenCalledWith('/api/connect', expect.anything())
  })

  it('connects to the context the operator picked, not the preselected one', async () => {
    const fetchMock = mockFetch()
    render(<Connect onConnected={() => {}} />)

    fireEvent.click(await screen.findByRole('radio', { name: /alpha/ }))
    fireEvent.click(screen.getByRole('button', { name: /connect/i }))

    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith('/api/connect', expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({ context: 'alpha' }),
      })))
  })

  // The confirmation is what makes "am I pointed at the right cluster" an
  // answerable question. A context name is a label in a file the operator
  // edits; a server version and a node count came from the cluster itself.
  it('shows the cluster and toolchain the operator is about to install into', async () => {
    mockFetch()
    render(<Connect onConnected={() => {}} />)

    fireEvent.click(await screen.findByRole('button', { name: /connect/i }))

    expect(await screen.findByText('v1.31.4')).toBeDefined()
    expect(screen.getByText(/6 total/)).toBeDefined()
    expect(screen.getByText('v3.19.0')).toBeDefined()
    expect(screen.getByText('1111-2222')).toBeDefined()
  })

  // A node count is a scalar an operator cannot act on. What they are deciding
  // is whether this is the cluster they meant, and the shapes answer that:
  // "two H100 nodes and four small ones" is recognisable in a way "6" is not.
  it('shows what the cluster is made of, folded into shapes', async () => {
    mockFetch()
    render(<Connect onConnected={() => {}} />)

    fireEvent.click(await screen.findByRole('button', { name: /connect/i }))

    expect(await screen.findByText(/2 × a3-megagpu-8g/)).toBeDefined()
    expect(screen.getByText(/nvidia-h100-mega-80gb/)).toBeDefined()
    expect(screen.getByText(/8 GPU each/)).toBeDefined()
    expect(screen.getByText(/3 × e2-standard-4/)).toBeDefined()
    expect(screen.getByText(/2 advertising GPUs/)).toBeDefined()
  })

  // The Phase 4 failure, now handled rather than delegated. The screen used to
  // print `AICRME_GPU_TOLERATIONS=<taint>` and ask the operator to quit and
  // relaunch; it names the taint it adopted instead. Both halves are asserted:
  // saying what it did, and no longer asking for anything.
  it('names the GPU taint it adopted, and asks the operator for nothing', async () => {
    mockFetch()
    render(<Connect onConnected={() => {}} />)

    fireEvent.click(await screen.findByRole('button', { name: /connect/i }))

    expect(await screen.findByText(/will tolerate/i)).toBeDefined()
    expect(screen.getAllByText(/dedicated=gpu-workload:NoSchedule/).length).toBeGreaterThan(0)
    expect(screen.queryByText(/AICRME_GPU_TOLERATIONS/)).toBeNull()
    expect(screen.queryByText(/relaunch/i)).toBeNull()
  })

  // The counterpart, and the more important of the two. A warning that shows
  // on every healthy cluster is a warning nobody reads on the broken one.
  it('says nothing about tolerations when every GPU node is reachable', async () => {
    const healthy: NodeComposition = {
      total: 6,
      gpuNodes: 2,
      untainted: 3,
      groups: [
        {
          count: 2,
          instanceType: 'a3-megagpu-8g',
          accelerator: 'nvidia-h100-mega-80gb',
          gpusPerNode: 8,
          taints: ['nvidia.com/gpu=present:NoSchedule'],
        },
        { count: 4, instanceType: 'e2-standard-4' },
      ],
    }
    mockFetch({ connect: () => new Response(JSON.stringify(withNodes(healthy)), { status: 200 }) })
    render(<Connect onConnected={() => {}} />)

    fireEvent.click(await screen.findByRole('button', { name: /connect/i }))

    await screen.findByText(/2 × a3-megagpu-8g/)
    expect(screen.queryByText(/AICRME_GPU_TOLERATIONS/)).toBeNull()
    // Nor the adoption notice: nvidia.com/gpu is covered by the built-in
    // toleration, so nothing was derived and there is nothing to report.
    expect(screen.queryByText(/will tolerate/i)).toBeNull()
  })

  // The 2026-08-28 install failure, moved to second zero. helm consults its
  // credential helper before it will try an anonymous pull, so a config naming
  // a helper this machine lacks fails every oci:// chart -- public ones
  // included. It surfaced as component 3 of 16 dying five minutes into Apply,
  // naming a Docker binary the operator never asked for.
  it('warns when helm names a credential helper this machine does not have', async () => {
    const warned: ClusterInfo = {
      ...clusterInfo,
      registryWarning: 'helm\'s registry config (/x/config.json) names a credential helper this machine does not have: docker-credential-osxkeychain (every registry).',
    }
    mockFetch({ connect: () => new Response(JSON.stringify(warned), { status: 200 }) })
    render(<Connect onConnected={() => {}} />)

    fireEvent.click(await screen.findByRole('button', { name: /connect/i }))

    expect(await screen.findByText(/docker-credential-osxkeychain/)).toBeDefined()
  })

  // The counterpart: a healthy machine says nothing, or the warning stops
  // being read on the machine that has the problem.
  it('says nothing about registry credentials on a healthy machine', async () => {
    mockFetch()
    render(<Connect onConnected={() => {}} />)

    fireEvent.click(await screen.findByRole('button', { name: /connect/i }))

    await screen.findByText(/2 × a3-megagpu-8g/)
    expect(screen.queryByText(/credential helper/i)).toBeNull()
  })

  // Capping is honest only if it says it capped.
  it('counts the shapes it did not have room to show', async () => {
    const many: NodeComposition = {
      total: 90,
      gpuNodes: 0,
      untainted: 3,
      groups: [{ count: 10, instanceType: 'e2-standard-4' }],
      more: 3,
    }
    mockFetch({ connect: () => new Response(JSON.stringify(withNodes(many)), { status: 200 }) })
    render(<Connect onConnected={() => {}} />)

    fireEvent.click(await screen.findByRole('button', { name: /connect/i }))

    expect(await screen.findByText(/3 more shapes/)).toBeDefined()
  })

  // Nothing installs until the operator says so. Confirming is the whole
  // reason the screen has two steps rather than one.
  it('does not report a connection until the operator continues past the confirmation', async () => {
    mockFetch()
    const onConnected = vi.fn()
    render(<Connect onConnected={onConnected} />)

    fireEvent.click(await screen.findByRole('button', { name: /connect/i }))
    await screen.findByRole('heading', { name: /connected/i })
    expect(onConnected).not.toHaveBeenCalled()

    fireEvent.click(screen.getByRole('button', { name: /continue/i }))
    expect(onConnected).toHaveBeenCalledWith(clusterInfo)
  })

  it('says a run was recovered so the operator is not surprised by one', async () => {
    mockFetch({
      connect: () => new Response(JSON.stringify({
        ...clusterInfo,
        recoveredRun: { id: 'abcdef0123456789', state: 'failed' },
      }), { status: 200 }),
    })
    render(<Connect onConnected={() => {}} />)

    fireEvent.click(await screen.findByRole('button', { name: /connect/i }))

    expect(await screen.findByText(/interrupted and has been recovered/i)).toBeDefined()
  })

  // A wrong context or a sleeping VPN is the ordinary case, so the screen has
  // to stay usable: the connector returns to disconnected on failure and the
  // operator must be able to pick again without restarting the binary.
  it('leaves the operator able to pick again after a failed connect', async () => {
    mockFetch({ connect: () => new Response('nope', { status: 504 }) })
    render(<Connect onConnected={() => {}} />)

    fireEvent.click(await screen.findByRole('button', { name: /connect/i }))

    expect(await screen.findByText(/failed to connect/i)).toBeDefined()
    expect(screen.getByRole('radio', { name: /alpha/ })).toBeDefined()
    expect((screen.getByRole('button', { name: /connect/i }) as HTMLButtonElement).disabled).toBe(false)
  })

  it('reports an unreadable kubeconfig instead of an empty list', async () => {
    mockFetch({ contexts: () => new Response('boom', { status: 500 }) })
    render(<Connect onConnected={() => {}} />)

    expect(await screen.findByText(/failed to read your kubeconfig/i)).toBeDefined()
  })

  // Fetched on the Connected screen rather than at connect time: it costs a
  // helm list plus one helm history per matched release, and paying that
  // during Connect would delay the screen for a panel most clusters do not
  // need.
  it('shows already-installed components on the connected screen', async () => {
    mockFetch({
      survey: () => new Response(JSON.stringify({
        clusterUid: '1111-2222', driverMode: 'host', complete: true,
        releases: [{
          name: 'gpu-operator', namespace: 'gpu-operator', chart: 'gpu-operator',
          chartVersion: 'v26.3.3', component: 'gpu-operator', revision: 1,
          firstDeployed: '2026-08-30T20:29:00Z', lastUpdated: '2026-08-30T20:40:00Z',
          nodeLevel: false, recommended: true,
        }],
      }), { status: 200 }),
    })

    render(<Connect onConnected={() => {}} />)
    fireEvent.click(await screen.findByRole('button', { name: /connect/i }))

    expect(await screen.findByTestId('clear-panel')).toBeDefined()
  })

  // The survey is advisory. Blocking an install because an advisory panel
  // could not load would let a helm timeout stop the console.
  it('keeps Continue enabled while the survey is still running', async () => {
    mockFetch({ survey: () => new Promise(() => {}) as unknown as Response })

    render(<Connect onConnected={() => {}} />)
    fireEvent.click(await screen.findByRole('button', { name: /connect/i }))

    const cont = await screen.findByRole('button', { name: /continue/i })
    expect(cont.hasAttribute('disabled')).toBe(false)
    expect(screen.getByTestId('clear-loading')).toBeDefined()
  })
})

// Observed on EKS 2026-08-30: every node carried dedicated=worker-workload, so
// the first chart sat Unschedulable for 3m23s and the install stalled. Nothing
// said so at Connect, where it was already knowable.
it('warns before connecting when no node can take an untolerated pod', async () => {
  mockFetch({
    contexts: () => new Response(JSON.stringify([{ name: 'eks', server: 'https://x', current: true }]), { status: 200 }),
    connect: () => new Response(
      JSON.stringify(withNodes({ ...taintedNodes, untainted: 0 })), { status: 200 }),
  })

  render(<Connect onConnected={() => {}} />)
  fireEvent.click(await screen.findByRole('button', { name: /connect/i }))

  const warning = await screen.findByTestId('nothing-schedulable')
  expect(warning.textContent).toMatch(/will not schedule/i)
  expect(warning.textContent).toMatch(/install will stall/i)
})

// The ordinary case must not show it: a cluster with any untainted node is fine.
it('does not warn when some node can take an untolerated pod', async () => {
  mockFetch({
    contexts: () => new Response(JSON.stringify([{ name: 'gke', server: 'https://x', current: true }]), { status: 200 }),
    connect: () => new Response(JSON.stringify(clusterInfo), { status: 200 }),
  })

  render(<Connect onConnected={() => {}} />)
  fireEvent.click(await screen.findByRole('button', { name: /connect/i }))

  await screen.findByText(/Connected/i)
  expect(screen.queryByTestId('nothing-schedulable')).toBeNull()
})

// A p5.48xlarge has eight H100s and advertises none of them until a device
// plugin lands, so "no GPUs" was a false claim about the hardware on real EKS
// nodes. The screen reports what the scheduler can see, and now says so.
it('says GPUs are unadvertised rather than absent', async () => {
  mockFetch({
    connect: () => new Response(JSON.stringify(withNodes({
      total: 2,
      gpuNodes: 0,
      untainted: 2,
      groups: [{ count: 2, instanceType: 'p5.48xlarge' }],
    })), { status: 200 }),
  })

  render(<Connect onConnected={() => {}} />)
  fireEvent.click(await screen.findByRole('button', { name: /connect/i }))

  expect(await screen.findByText(/none advertised/)).toBeDefined()
  expect(screen.queryByText(/no GPUs/)).toBeNull()
})

describe('contextLabel', () => {
  // Every AWS row shares the same ~40-character ARN prefix, so the only part
  // that identifies the cluster is last -- and wraps to a second line.
  // Observed on real EKS 2026-08-30.
  it('leads with the cluster name for an EKS ARN', () => {
    const got = contextLabel('arn:aws:eks:us-east-1:615299774277:cluster/aicr-uat-day-ah1-0-33316327543')
    expect(got.lead).toBe('aicr-uat-day-ah1-0-33316327543')
    expect(got.prefix).toBe('arn:aws:eks:us-east-1:615299774277:cluster/')
  })

  // GKE and plain names front-load what matters already; splitting them would
  // invent structure that is not there.
  it('leaves a non-ARN name whole', () => {
    expect(contextLabel('gke_proj_us-central1_cluster-1')).toEqual({ lead: 'gke_proj_us-central1_cluster-1' })
    expect(contextLabel('minikube')).toEqual({ lead: 'minikube' })
  })
})
