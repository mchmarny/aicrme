import { cleanup, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'

const contexts = [{ name: 'alpha', server: 'https://alpha.example:6443', current: true }]

const clusterInfo = {
  context: 'alpha',
  server: 'https://alpha.example:6443',
  version: 'v1.31.4',
  nodeCount: 6,
  nodes: { total: 6, gpuNodes: 2, totalGPUs: 16, usableGPUs: 16 },
  uid: '1111-2222',
  toolchain: {},
}

type Routes = {
  session?: () => Response
  sessionProbe?: () => Response
  cluster?: () => Response
}

/**
 * mockFetch answers the bootstrap routes. Every default is the happy path for
 * a FRESH launch -- token accepted, no cluster yet -- so each test overrides
 * only the one answer it is about.
 */
function mockFetch(routes: Routes = {}) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input.toString()
    if (url === '/api/session') {
      return init?.method === 'POST'
        ? routes.session?.() ?? new Response(null, { status: 204 })
        : routes.sessionProbe?.() ?? new Response(null, { status: 204 })
    }
    if (url === '/api/cluster') {
      return routes.cluster?.() ?? new Response('not connected', { status: 409 })
    }
    if (url === '/api/contexts') return new Response(JSON.stringify(contexts), { status: 200 })
    if (url === '/api/runs') return new Response(JSON.stringify({ id: 'run', state: 'running' }), { status: 200 })
    if (url === '/api/options') return new Response(JSON.stringify({
      intents: [], platforms: [], platformsByIntent: {}, provisional: false,
    }), { status: 200 })
    throw new Error(`unexpected fetch: ${url}`)
  })
  vi.stubGlobal('fetch', fetchMock)
  // App's console half opens an EventSource, which jsdom does not implement.
  // Bootstrap is what these tests are about; the stream has its own coverage
  // in useEvents.lifecycle.test.tsx.
  vi.stubGlobal('EventSource', class {
    close() {}
    addEventListener() {}
    removeEventListener() {}
  })
  return fetchMock
}

describe('App bootstrap', () => {
  beforeEach(() => window.history.replaceState({}, '', '/'))
  // Unmount BEFORE unstubbing, not after. App's console half opens its
  // EventSource from an effect that only fires once the session probe and
  // cluster fetch resolve -- after the assertions above have already passed.
  // Leaving the tree mounted let that effect land after unstubAllGlobals had
  // removed the EventSource stub, which is a race that loses only under load:
  // green on every local run, and it failed CI on a docs-only commit.
  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
  })

  // The token is a one-shot credential printed in a URL. Exchanging it for a
  // cookie and stripping it is what keeps it out of bookmarks, copied links
  // and browser history.
  it('exchanges a ?t= token for a session and strips it from the URL', async () => {
    window.history.replaceState({}, '', '/?t=launch-token-value')
    const fetchMock = mockFetch()

    render(<App />)

    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/api/session', expect.objectContaining({
      method: 'POST',
      body: JSON.stringify({ token: 'launch-token-value' }),
    })))
    expect(window.location.search).toBe('')
    expect(await screen.findByRole('heading', { name: /connect a cluster/i })).toBeDefined()
  })

  // The reload path, and the ordinary one after the first minute: a restored
  // tab has no ?t= and only the cookie the first load set. This is the case an
  // in-memory token could not serve.
  it('authenticates from the cookie on a reload with no token in the URL', async () => {
    const fetchMock = mockFetch()

    render(<App />)

    expect(await screen.findByRole('heading', { name: /connect a cluster/i })).toBeDefined()
    expect(fetchMock).not.toHaveBeenCalledWith('/api/session', expect.objectContaining({ method: 'POST' }))
    expect(fetchMock).toHaveBeenCalledWith('/api/session')
  })

  // The connection is single-assignment, so a reload that went back to Connect
  // would ask again and be refused with a 409 the operator cannot get past.
  it('skips Connect when this console is already connected', async () => {
    const fetchMock = mockFetch({
      cluster: () => new Response(JSON.stringify(clusterInfo), { status: 200 }),
    })

    render(<App />)

    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/api/runs', expect.anything()))
    expect(screen.queryByRole('heading', { name: /connect a cluster/i })).toBeNull()
  })

  // A cookie the server does not recognize means the process that minted it is
  // gone. There is nothing to retry and no token left in the URL, so saying so
  // is the only useful thing left.
  // WHICH CLUSTER AM I ABOUT TO INSTALL INTO.
  //
  // Connect answers it and then throws the answer away: past that screen the
  // header said only "connected", so every later screen -- including the gate
  // where the operator grants cluster-admin to install fourteen components --
  // named no cluster at all. On the laptop this was built against, the
  // kubeconfig holds 144 contexts.
  it('keeps the connected cluster named in the header', async () => {
    mockFetch({ cluster: () => new Response(JSON.stringify(clusterInfo), { status: 200 }) })

    render(<App />)

    // The context is the name the operator chose it by, so it is the name the
    // header has to carry.
    expect(await screen.findByText('alpha')).toBeDefined()
    // And the cluster-wide GPU count, which is the other half of "is this the
    // right one" and is already computed at connect.
    expect(screen.getByText(/16 GPUs/)).toBeDefined()
  })

  it('says the session is gone rather than looping when the cookie is not recognized', async () => {
    mockFetch({ sessionProbe: () => new Response(null, { status: 401 }) })

    render(<App />)

    expect(await screen.findByText(/session has expired/i)).toBeDefined()
    expect(screen.queryByRole('heading', { name: /connect a cluster/i })).toBeNull()
  })

  it('reports a launch token the server refused', async () => {
    window.history.replaceState({}, '', '/?t=stale-token')
    mockFetch({ session: () => new Response('nope', { status: 401 }) })

    render(<App />)

    expect(await screen.findByText(/launch token was not accepted/i)).toBeDefined()
  })
})
