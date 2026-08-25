import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Connect } from './Connect'
import type { ClusterInfo } from '../api'

const contexts = [
  { name: 'alpha', server: 'https://alpha.example:6443', current: false },
  { name: 'beta', server: 'https://beta.example:6443', current: true },
]

const clusterInfo: ClusterInfo = {
  context: 'beta',
  server: 'https://beta.example:6443',
  version: 'v1.31.4',
  nodeCount: 6,
  uid: '1111-2222',
  toolchain: { helm: 'v3.19.0', kubectl: 'v1.31.0', bash: '5.2.15', jq: '1.7' },
}

/**
 * mockFetch answers the two routes Connect uses. Deliberately a fetch stub
 * rather than a mocked api module: the request shape POST /api/connect sends
 * is part of what this screen has to get right, and mocking the client would
 * make it untestable.
 */
function mockFetch(overrides: { connect?: () => Response; contexts?: () => Response } = {}) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = typeof input === 'string' ? input : input.toString()
    if (url === '/api/contexts') {
      return overrides.contexts?.() ?? new Response(JSON.stringify(contexts), { status: 200 })
    }
    if (url === '/api/connect') {
      return overrides.connect?.() ?? new Response(JSON.stringify(clusterInfo), { status: 200 })
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
    expect(screen.getByText('6 nodes')).toBeDefined()
    expect(screen.getByText('v3.19.0')).toBeDefined()
    expect(screen.getByText('1111-2222')).toBeDefined()
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
})
