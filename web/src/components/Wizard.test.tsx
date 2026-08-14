import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Wizard } from './Wizard'
import type { AicrEvent } from '../useEvents'
import kwokRun from '../fixtures/kwok-run.json'

// kwok-run.json is a real, recorded stream from the production bus + engine
// + steps pipeline (internal/steps.NewDiscover / NewRecommend), not
// hand-typed JSON -- see task-12-report.md for exactly how it was captured.
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

function mockFetch() {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, _init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input.toString()
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

  it('renders the folded, pinned component list once recommend has resolved a recipe', async () => {
    render(<Wizard events={events} />)
    await waitFor(() => expect(screen.getAllByRole('radiogroup')).toHaveLength(2))
    // "13 components" alone also matches the timeline's own log line for the
    // same event (see Timeline.tsx rendering event id 13's message) --
    // "and signed" is unique to Recommend's fold summary.
    expect(screen.getByText(/every version pinned and signed/)).toBeDefined()
    expect(screen.getByText(/gpu-operator v26.3.3/)).toBeDefined()
  })

  it('keeps the event timeline visible as a right rail', () => {
    render(<Wizard events={discoverOnly} />)
    expect(screen.getByText('deploying cluster snapshot agent')).toBeDefined()
  })
})
