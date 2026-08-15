import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { MAX_PROVISIONAL_OPTIONS_RETRIES, Wizard } from './Wizard'
import type { AicrEvent } from '../useEvents'
import kwokRun from '../fixtures/kwok-run.json'

// kwok-run.json is a real, recorded stream from the production bus + engine
// + steps pipeline (internal/steps.NewDiscover / NewRecommend), not
// hand-typed JSON: it was captured by driving a real run against a KWOK
// cluster carrying AICR's simulated H100 nodes (test/e2e/discover-recommend.sh
// stands up the same cluster) and saving GET /api/events verbatim.
// Slicing it at known offsets exercises Wizard against the exact event
// shapes the server emits at each stage of a real run: events[0..8] cover
// the discover phase ending with the capability report on event id 3;
// event id 10 is the KindDecision "awaiting decision" that parks the run on
// recommend; the full 15-event array carries the resolved recipe on event
// id 13 through to "run done".
const events = kwokRun as AicrEvent[]
const discoverOnly = events.slice(0, 9)
const awaitingRecommendDecision = events.slice(0, 10)

const optionsResponse = {
  intents: ['training', 'inference'],
  platforms: ['kubeflow', 'slurm', 'dynamo'],
  platformsByIntent: { training: ['kubeflow', 'slurm'], inference: ['dynamo'] },
  provisional: false,
}

function mockFetch(handler?: (url: string) => Response | Promise<Response>) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, _init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input.toString()
    if (handler) return handler(url)
    if (url === '/api/options') {
      return new Response(JSON.stringify(optionsResponse), { status: 200 })
    }
    if (url.includes('/decide')) {
      return new Response(JSON.stringify({ id: 'run', state: 'running' }), { status: 200 })
    }
    throw new Error(`unexpected fetch: ${url}`)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

describe('Wizard', () => {
  beforeEach(() => {
    mockFetch()
  })
  afterEach(() => {
    vi.unstubAllGlobals()
    vi.useRealTimers()
  })

  it('shows the capability statement while the run is still in the discover phase', () => {
    render(<Wizard events={discoverOnly} />)
    expect(screen.getByRole('heading', { name: /kind cluster with 7 node/i })).toBeDefined()
    expect(screen.getByTestId('punchline').textContent).toContain('No GPU hardware detected')
  })

  it('does not fetch /api/options while still on the discover phase', () => {
    const fetchMock = mockFetch()
    render(<Wizard events={discoverOnly} />)
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('asks the two questions once the run parks awaiting the recommend decision', async () => {
    const fetchMock = mockFetch()
    render(<Wizard events={awaitingRecommendDecision} />)
    await waitFor(() => expect(screen.getAllByRole('radiogroup')).toHaveLength(2))
    expect(fetchMock).toHaveBeenCalledWith('/api/options')
  })

  it('submits the decision to the run the events named', async () => {
    const fetchMock = mockFetch()
    render(<Wizard events={awaitingRecommendDecision} />)
    await waitFor(() => expect(screen.getAllByRole('radiogroup')).toHaveLength(2))

    fireEvent.click(screen.getByLabelText('training'))
    fireEvent.click(screen.getByLabelText('kubeflow'))
    fireEvent.click(screen.getByRole('button', { name: /continue/i }))

    await waitFor(() => {
      const call = fetchMock.mock.calls.find(([url]) => String(url).includes('/decide'))
      expect(call).toBeDefined()
    })
    const [url, init] = fetchMock.mock.calls.find(([u]) => String(u).includes('/decide'))!
    expect(url).toBe('/api/runs/553a423750e2797b/decide')
    expect(JSON.parse(init!.body as string)).toEqual({ intent: 'training', platform: 'kubeflow' })
  })

  // Regression guard: this test used to render the full, completed stream
  // and assert the two radiogroups were still present --
  // locking in a defect where a finished run kept an enabled Continue button
  // that would 409 against Decide's "run is not awaiting a decision" guard.
  // The correct behaviour is the opposite: once the run has moved past
  // awaiting_decision, the ask-form must not reappear.
  it('does not re-ask the two questions once the run has already decided and resolved', async () => {
    render(<Wizard events={events} />)
    await waitFor(() => expect(screen.getByText(/every version pinned and signed/)).toBeDefined())
    expect(screen.queryAllByRole('radiogroup')).toHaveLength(0)
    expect(screen.queryByRole('button', { name: /continue/i })).toBeNull()
    expect(screen.getByText(/gpu-operator v26.3.3/)).toBeDefined()
  })

  it('keeps the event timeline visible as a right rail', () => {
    render(<Wizard events={discoverOnly} />)
    expect(screen.getByText('deploying cluster snapshot agent')).toBeDefined()
  })

  // Regression guard: the bus's replay ring is global across every run
  // this process has started, and a page reload starts a
  // fresh run whenever the previous one is no longer live. Without
  // partitioning by runId, a reload that replays both runs' events rendered
  // the finished run A's capability report as though it belonged to the
  // brand-new run B.
  it('does not show a previous run\'s report or recipe as the current run\'s', () => {
    const runA: AicrEvent[] = [
      { id: 1, runId: 'runA', at: '2026-08-14T00:00:00Z', kind: 'phase', level: 'info', phase: 'discover', message: 'phase started' },
      {
        id: 2, runId: 'runA', at: '2026-08-14T00:00:01Z', kind: 'log', level: 'info', phase: 'discover',
        message: 'RUN A HEADLINE',
        data: { headline: 'RUN A HEADLINE', punchline: 'RUN A PUNCHLINE', usableGpus: 0, totalGpus: 0, gaps: [] },
      },
      { id: 3, runId: 'runA', at: '2026-08-14T00:00:02Z', kind: 'phase', level: 'info', phase: 'discover', message: 'phase complete' },
      { id: 4, runId: 'runA', at: '2026-08-14T00:00:03Z', kind: 'phase', level: 'info', message: 'run done' },
    ]
    const runB: AicrEvent[] = [
      { id: 5, runId: 'runB', at: '2026-08-14T01:00:00Z', kind: 'phase', level: 'info', phase: 'discover', message: 'phase started' },
      { id: 6, runId: 'runB', at: '2026-08-14T01:00:01Z', kind: 'log', level: 'info', phase: 'discover', message: 'deploying cluster snapshot agent' },
    ]

    render(<Wizard events={[...runA, ...runB]} />)

    // "RUN A HEADLINE" legitimately still appears once, as run A's own
    // timeline log line -- Wizard deliberately keeps the full cross-run
    // event history visible in the right rail (see the Wizard.tsx doc
    // comment on currentRunIdOf). What must NOT happen is run A's report
    // being rendered as run B's Discover screen, i.e. as a heading.
    expect(screen.queryByRole('heading', { name: /RUN A HEADLINE/ })).toBeNull()
    expect(screen.queryByTestId('punchline')).toBeNull()
    expect(screen.getByText('Discovering the cluster…')).toBeDefined()
  })

  // Regression guard: a failed /api/options fetch used to be swallowed by
  // a bare `.catch(() => {})`, leaving the loading
  // placeholder on screen forever with no way to recover.
  it('surfaces a failed options fetch and recovers once the user retries', async () => {
    let calls = 0
    mockFetch(url => {
      if (url !== '/api/options') throw new Error(`unexpected fetch: ${url}`)
      calls++
      if (calls === 1) throw new Error('network error reaching /api/options')
      return new Response(JSON.stringify(optionsResponse), { status: 200 })
    })

    render(<Wizard events={awaitingRecommendDecision} />)
    await waitFor(() => expect(screen.getByText(/network error reaching \/api\/options/)).toBeDefined())

    fireEvent.click(screen.getByRole('button', { name: /retry/i }))
    await waitFor(() => expect(screen.getAllByRole('radiogroup')).toHaveLength(2))
  })

  // Regression guard: internal/api/options.go's
  // handleOptions doc comment is explicit that a client "MUST NOT keep
  // showing a provisional set once a verified one is available" -- Wizard
  // used to fetch once and hand the result straight to Recommend regardless
  // of the `provisional` flag.
  it('refetches automatically while /api/options reports provisional, then renders the verified answer', async () => {
    let calls = 0
    const fetchMock = mockFetch(url => {
      if (url !== '/api/options') throw new Error(`unexpected fetch: ${url}`)
      calls++
      return new Response(JSON.stringify({ ...optionsResponse, provisional: calls === 1 }), { status: 200 })
    })

    render(<Wizard events={awaitingRecommendDecision} />)
    await waitFor(() => expect(screen.getAllByRole('radiogroup')).toHaveLength(2), { timeout: 3000 })
    expect(fetchMock.mock.calls.length).toBeGreaterThanOrEqual(2)
    expect(screen.queryByText(/could not be verified/)).toBeNull()
  })

  it('shows a caveat rather than presenting an exhausted-retry provisional set as final', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const fetchMock = mockFetch(url => {
      if (url !== '/api/options') throw new Error(`unexpected fetch: ${url}`)
      return new Response(JSON.stringify({ ...optionsResponse, provisional: true }), { status: 200 })
    })

    render(<Wizard events={awaitingRecommendDecision} />)

    for (let i = 0; i <= MAX_PROVISIONAL_OPTIONS_RETRIES; i++) {
      await vi.advanceTimersByTimeAsync(5000)
    }

    expect(fetchMock.mock.calls.length).toBe(MAX_PROVISIONAL_OPTIONS_RETRIES + 1)
    expect(screen.getByText(/could not be verified/)).toBeDefined()
    expect(screen.getAllByRole('radiogroup')).toHaveLength(2)
  })
})
