import { render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'

const contexts = [{ name: 'alpha', server: 'https://alpha.example:6443', current: true }]

const clusterInfo = {
  context: 'alpha',
  server: 'https://alpha.example:6443',
  version: 'v1.31.4',
  nodeCount: 6,
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
  afterEach(() => vi.unstubAllGlobals())

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
