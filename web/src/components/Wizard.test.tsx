import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { deriveRunState, MAX_PROVISIONAL_OPTIONS_RETRIES, Wizard } from './Wizard'
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

  // Regression guard (M1): run.error is derived by replaying every event a
  // run has ever emitted, including a stale 'error' kind from an attempt
  // that later succeeded -- deriveRunState used to have no way to drop it,
  // so a successful Retry rendered the success state with the previous
  // attempt's red failure line still above it. engine.Retry
  // (internal/engine/engine.go) publishes a "run retrying" KindPhase event
  // before relaunching execute, which is the signal deriveRunState now uses
  // to clear the stale error.
  it('clears a prior failure once a retry starts, and does not resurrect it after the retry succeeds', () => {
    const failThenRetrySucceed: AicrEvent[] = [
      { id: 1, runId: 'runX', at: '2026-08-15T00:00:00Z', kind: 'phase', level: 'info', phase: 'apply', message: 'phase started' },
      { id: 2, runId: 'runX', at: '2026-08-15T00:00:01Z', kind: 'error', level: 'error', phase: 'apply', message: 'network-operator failed: no matches for kind "NodeFeatureRule"' },
      { id: 3, runId: 'runX', at: '2026-08-15T00:00:02Z', kind: 'phase', level: 'error', message: 'run failed' },
      { id: 4, runId: 'runX', at: '2026-08-15T00:00:03Z', kind: 'phase', level: 'info', message: 'run retrying' },
      { id: 5, runId: 'runX', at: '2026-08-15T00:00:04Z', kind: 'phase', level: 'info', phase: 'apply', message: 'phase started' },
      { id: 6, runId: 'runX', at: '2026-08-15T00:00:05Z', kind: 'phase', level: 'info', message: 'run done' },
    ]

    const beforeRetry = deriveRunState(failThenRetrySucceed.slice(0, 3))
    expect(beforeRetry.state).toBe('failed')
    expect(beforeRetry.error).toBeDefined()

    const afterRetry = deriveRunState(failThenRetrySucceed)
    expect(afterRetry.state).toBe('done')
    expect(afterRetry.error).toBeUndefined()
  })

  /**
   * The live path onto the payoff screen, with no restart involved.
   *
   * engine.finish publishes "run " + state for every state it reaches, and
   * StateActive is a state it now reaches (internal/engine's ActiveStep).
   * Before this branch existed it fell through to 'running', so a run that
   * had finished with a workload deliberately left running rendered as one
   * still in flight -- on the Discover screen, since nothing else claimed the
   * prove phase -- with no way to stop what it started.
   */
  it('lands an active run on the Prove screen with a Stop control', () => {
    const proveRun: AicrEvent[] = [
      { id: 1, runId: 'runP', at: '2026-08-21T00:00:00Z', kind: 'phase', level: 'info', phase: 'prove', message: 'phase started' },
      { id: 2, runId: 'runP', at: '2026-08-21T00:00:01Z', kind: 'log', level: 'info', phase: 'prove', message: 'applying the reference workload' },
      { id: 3, runId: 'runP', at: '2026-08-21T00:00:02Z', kind: 'cluster', level: 'info', phase: 'prove', message: 'gang member prove-runP-0 placed on node kwok-gpu-0' },
      { id: 4, runId: 'runP', at: '2026-08-21T00:00:03Z', kind: 'cluster', level: 'info', phase: 'prove', message: 'gang member prove-runP-1 placed on node kwok-gpu-1' },
      { id: 5, runId: 'runP', at: '2026-08-21T00:00:04Z', kind: 'phase', level: 'info', message: 'run active' },
    ]

    expect(deriveRunState(proveRun).state).toBe('active')

    render(<Wizard events={proveRun} />)
    expect(screen.getByTestId('prove')).toBeDefined()
    expect(screen.getByRole('button', { name: /stop workload/i })).toBeDefined()
    expect(screen.getByTestId('prove-placements').textContent).toMatch(/kwok-gpu-1/)
    // Not the recovery panel: this run was never recovered.
    expect(screen.queryByTestId('prove-recovered')).toBeNull()
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

// RECOVERED_RUN_ID is the 16-hex-character shape engine.newID produces and
// internal/engine's recover tests seed, so the URLs asserted below are the
// ones a real recovered run would produce.
const RECOVERED_RUN_ID = '0123456789abcdef'

/**
 * recoveryEvents reproduces, in order, exactly what
 * internal/engine/recover.go's publishRecoveryBootstrap emits for a recovered
 * run: the KindRecovered marker, one KindComponent per persisted component
 * row, the run's error as a KindError when it carries one, and the
 * "run <state>" KindPhase event last. Hand-built rather than sliced from
 * kwok-run.json because no recorded stream contains a restart -- but every
 * field here is pinned on the producing side by
 * TestRecoverPublishesTheRecoveryMarkerInEveryPhaseAndState and
 * TestRecoverPublishesBootstrapEvents.
 */
function recoveryEvents(
  phase: string,
  state: string,
  error?: string,
  components: Array<{ name: string; status: string }> = [],
  truncated?: string[],
): AicrEvent[] {
  const out: AicrEvent[] = [{
    id: 1, runId: RECOVERED_RUN_ID, at: '2026-08-17T00:00:00Z', kind: 'recovered', level: 'warn',
    phase, message: 'recovered a previous run; retry or discard it before starting a new one',
    // internal/engine/recover.go omits Data entirely for an intact record,
    // so its absence is meaningful and this mirrors that.
    ...(truncated ? { data: { truncated } } : {}),
  }]
  components.forEach((c, i) => {
    out.push({
      id: out.length + 1, runId: RECOVERED_RUN_ID, at: `2026-08-17T00:00:0${i + 1}Z`,
      kind: 'component', level: 'info', phase, component: c.name,
      message: `${c.name} ${c.status}`, data: { name: c.name, status: c.status },
    })
  })
  if (error) {
    out.push({
      id: out.length + 1, runId: RECOVERED_RUN_ID, at: '2026-08-17T00:00:08Z',
      kind: 'error', level: 'error', phase, message: error,
    })
  }
  out.push({
    id: out.length + 1, runId: RECOVERED_RUN_ID, at: '2026-08-17T00:00:09Z',
    kind: 'phase', level: state === 'failed' ? 'error' : 'info', phase, message: `run ${state}`,
  })
  return out
}

/**
 * The Critical finding: a recovered run had no reachable operator action
 * outside bundle/apply, because the only Retry button lived inside Cockpit
 * and Wizard renders Cockpit only for those two phases. Discard had no caller
 * anywhere in the SPA, so a run recovered in a state Retry refuses -- `done`,
 * which every `helm upgrade` of a release that has completed a demo recovers
 * -- had no exit at all: POST /api/runs 409s by design, and POST .../retry
 * answers "run is not in a failed state".
 *
 * The matrix below is the shape of that miss. The Go-side test meant to cover
 * this exercised PhaseApply alone.
 */
describe('Wizard: a recovered run', () => {
  beforeEach(() => {
    mockFetch()
  })
  afterEach(() => {
    vi.unstubAllGlobals()
    vi.useRealTimers()
  })

  const phases = ['discover', 'recommend', 'bundle', 'apply']
  const recoveredStates: Array<{ label: string; state: string; error?: string; retryable: boolean }> = [
    // engine.Recover flips every non-terminal state it finds to `failed` with
    // this exact error, so this covers a discover/recommend interruption and
    // a mid-Apply crash alike.
    { label: 'interrupted', state: 'failed', error: 'interrupted by a console restart', retryable: true },
    // A run that had already failed on its own before the pod went away.
    { label: 'already failed', state: 'failed', error: 'network-operator failed: no matches for kind "NodeFeatureRule"', retryable: true },
    // Scenario B: nothing interrupted it, and engine.Retry refuses it.
    { label: 'done', state: 'done', retryable: false },
    // `active` is deliberately absent from this matrix. It used to be here,
    // asserting the same Discard button as every other state, from before
    // Stop existed -- and the engine now REJECTS Discard for a run with a
    // workload still running (internal/engine's TestDiscardRejectsActiveRun),
    // so that assertion pinned a button guaranteed to 409. The recovered
    // active run has its own test below, where Stop is the affordance and
    // Discard's absence is the assertion.
  ]

  for (const phase of phases) {
    for (const { label, state, error, retryable } of recoveredStates) {
      it(`offers discard in ${phase}/${label}, and retry only when the run is retryable`, () => {
        render(<Wizard events={recoveryEvents(phase, state, error)} />)

        expect(screen.getByTestId('recovered-run')).toBeDefined()
        expect(screen.getByTestId('recovery-discard')).toBeDefined()
        if (retryable) {
          expect(screen.getByTestId('recovery-retry')).toBeDefined()
        } else {
          expect(screen.queryByTestId('recovery-retry')).toBeNull()
        }

        // No ordinary phase body may render underneath: a run recovered on
        // `recommend` used to show "Resolving the recipe for the answers you
        // gave…" forever, with no step running and none coming.
        expect(screen.queryByText(/Resolving the recipe/)).toBeNull()
        expect(screen.queryByText(/Discovering the cluster…/)).toBeNull()
        expect(screen.queryByTestId('cockpit-success')).toBeNull()
      })
    }
  }

  it('tells an interruption apart from a failure the run reached on its own', () => {
    const { unmount } = render(
      <Wizard events={recoveryEvents('apply', 'failed', 'interrupted by a console restart')} />)
    expect(screen.getByText(/The console restarted while this run was in the apply phase/)).toBeDefined()
    unmount()

    render(<Wizard events={recoveryEvents('apply', 'failed', 'helm upgrade --install failed')} />)
    expect(screen.getByText(/had already failed during the apply phase/)).toBeDefined()
    // The run's own error is still shown: it is what the operator needs to
    // judge whether retrying is worth anything. Scoped to the panel, since
    // the timeline rail legitimately renders the same message as a log line.
    const panel = within(screen.getByTestId('recovered-run'))
    expect(panel.getByText('helm upgrade --install failed')).toBeDefined()
  })

  it('says a completed run finished rather than claiming it was interrupted', () => {
    render(<Wizard events={recoveryEvents('apply', 'done')} />)
    expect(screen.getByText(/finished before the console restarted/)).toBeDefined()
    expect(screen.queryByText(/interrupted/i)).toBeNull()
  })

  it('redraws the persisted component rows the bootstrap replayed', () => {
    render(<Wizard events={recoveryEvents('apply', 'failed', 'interrupted by a console restart', [
      { name: 'gpu-operator', status: 'installed' },
      { name: 'kai-scheduler', status: 'failed' },
    ])} />)

    expect(screen.getByTestId('recovered-component-gpu-operator').textContent).toMatch(/installed/i)
    expect(screen.getByTestId('recovered-component-kai-scheduler').textContent).toMatch(/failed/i)
  })

  it('does not fetch /api/options for a run recovered on the recommend phase', () => {
    const fetchMock = mockFetch()
    render(<Wizard events={recoveryEvents('recommend', 'failed', 'interrupted by a console restart')} />)
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('discards through DELETE and lets the console start a new run without a reload', async () => {
    const fetchMock = mockFetch(url => {
      if (url !== `/api/runs/${RECOVERED_RUN_ID}`) throw new Error(`unexpected fetch: ${url}`)
      return new Response(null, { status: 204 })
    })
    const onDiscarded = vi.fn()

    render(<Wizard events={recoveryEvents('apply', 'done')} onDiscarded={onDiscarded} />)
    fireEvent.click(screen.getByTestId('recovery-discard'))

    await waitFor(() => expect(onDiscarded).toHaveBeenCalled())
    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toBe(`/api/runs/${RECOVERED_RUN_ID}`)
    expect(init?.method).toBe('DELETE')

    // The stream still ends on the discarded run until its replacement
    // publishes a first event, so the panel must not re-offer buttons that
    // would now 404.
    await waitFor(() => expect(screen.queryByTestId('recovery-discard')).toBeNull())
    expect(screen.getByText(/Starting a new run/)).toBeDefined()
  })

  it('surfaces a failed discard instead of pretending the run is gone', async () => {
    mockFetch(url => {
      if (url !== `/api/runs/${RECOVERED_RUN_ID}`) throw new Error(`unexpected fetch: ${url}`)
      return new Response(JSON.stringify({ error: 'deleting the persisted run failed' }), { status: 503 })
    })
    const onDiscarded = vi.fn()

    render(<Wizard events={recoveryEvents('apply', 'done')} onDiscarded={onDiscarded} />)
    fireEvent.click(screen.getByTestId('recovery-discard'))

    await waitFor(() => expect(screen.getByText(/Failed to discard the run/)).toBeDefined())
    expect(onDiscarded).not.toHaveBeenCalled()
    expect(screen.getByTestId('recovery-discard')).toBeDefined()
  })

  it('retries through POST and stops offering recovery actions once the retry starts', async () => {
    const fetchMock = mockFetch(url => {
      if (url !== `/api/runs/${RECOVERED_RUN_ID}/retry`) throw new Error(`unexpected fetch: ${url}`)
      return new Response(JSON.stringify({ id: RECOVERED_RUN_ID, state: 'running' }), { status: 200 })
    })

    const recovered = recoveryEvents('bundle', 'failed', 'interrupted by a console restart')
    const { rerender } = render(<Wizard events={recovered} />)
    fireEvent.click(screen.getByTestId('recovery-retry'))

    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(
      `/api/runs/${RECOVERED_RUN_ID}/retry`, { method: 'POST' }))

    // engine.Retry publishes this before relaunching execute, and it is the
    // moment the gate clears server-side -- the console must follow.
    rerender(<Wizard events={[...recovered, {
      id: 99, runId: RECOVERED_RUN_ID, at: '2026-08-17T00:01:00Z',
      kind: 'phase', level: 'info', message: 'run retrying',
    }]} />)

    expect(screen.queryByTestId('recovered-run')).toBeNull()
    expect(screen.queryByTestId('recovery-discard')).toBeNull()
  })

  // A truncated checkpoint cannot be retried: recovery rewinds to Bundle, and
  // internal/steps/bundle.go reads snapshot.yaml, which is the first artifact
  // the size guard sheds. The record has always named the loss; without this
  // the console offered "Retry this run" for a record whose retry is a dead
  // end -- honest storage, dishonest UI.
  it('warns that a truncated checkpoint cannot be retried, naming what was dropped', () => {
    render(<Wizard events={recoveryEvents(
      'apply', 'failed', 'interrupted by a console restart', [], ['snapshot.yaml'])} />)

    const note = screen.getByTestId('recovery-truncated')
    expect(note.textContent).toMatch(/snapshot\.yaml/)
    expect(note.textContent).toMatch(/too large to store in full/)
    expect(note.textContent).toMatch(/discarding and starting over/i)
    // Retry stays reachable rather than hidden: suppressing it would rest on
    // the current step slice happening to guarantee failure, which is an
    // accident of today's steps, not a structural property.
    expect(screen.getByTestId('recovery-retry')).toBeDefined()
    expect(screen.getByTestId('recovery-discard')).toBeDefined()
  })

  it('shows no truncation warning for an intact recovered record', () => {
    render(<Wizard events={recoveryEvents('apply', 'failed', 'interrupted by a console restart')} />)
    expect(screen.queryByTestId('recovery-truncated')).toBeNull()
  })

  /**
   * The recovered-active dead end, pinned from the console's side.
   *
   * A pod restart while the reference workload is running recovers the record
   * as StateActive (internal/engine/recover.go leaves terminal states alone),
   * and the workload is genuinely still there holding accelerators. Both
   * recovery actions are rejected for it: Retry requires StateFailed, and
   * Discard refuses a run with a workload running rather than orphaning it.
   * Stop is the only thing the engine accepts, so it has to be the only thing
   * this screen offers -- a panel of two dead buttons is how an operator ends
   * up reaching for kubectl.
   */
  it('offers Stop and not Discard for a recovered active run', () => {
    render(<Wizard events={recoveryEvents('prove', 'active')} />)

    expect(screen.getByRole('button', { name: /stop workload/i })).toBeDefined()
    expect(screen.queryByTestId('recovery-discard')).toBeNull()
    expect(screen.queryByTestId('recovery-retry')).toBeNull()
    expect(screen.queryByTestId('recovered-run')).toBeNull()
    // The screen still says where the workload came from, because the
    // timeline rail is replaying recovery's own "retry or discard it" marker
    // alongside it.
    expect(screen.getByTestId('prove-recovered')).toBeDefined()
  })

  it('stops through POST and lets the console start a new run without a reload', async () => {
    const fetchMock = mockFetch(url => {
      if (url !== `/api/runs/${RECOVERED_RUN_ID}/stop`) throw new Error(`unexpected fetch: ${url}`)
      return new Response(JSON.stringify({ id: RECOVERED_RUN_ID, state: 'done' }), { status: 200 })
    })
    const onStopped = vi.fn()

    render(<Wizard events={recoveryEvents('prove', 'active')} onStopped={onStopped} />)
    fireEvent.click(screen.getByTestId('prove-stop'))

    await waitFor(() => expect(onStopped).toHaveBeenCalled())
    expect(fetchMock).toHaveBeenCalledWith(
      `/api/runs/${RECOVERED_RUN_ID}/stop`, { method: 'POST' })
  })

  // engine.Stop leaves the run exactly where it was on any failure and says
  // what failed. A console that cleared the screen anyway would be claiming a
  // workload was stopped while it is still running -- the one outcome the
  // whole Stop design is built to avoid.
  it('surfaces a failed stop and keeps offering the button', async () => {
    mockFetch(url => {
      if (url !== `/api/runs/${RECOVERED_RUN_ID}/stop`) throw new Error(`unexpected fetch: ${url}`)
      return new Response(JSON.stringify({ error: 'stopping the workload failed' }), { status: 503 })
    })
    const onStopped = vi.fn()

    render(<Wizard events={recoveryEvents('prove', 'active')} onStopped={onStopped} />)
    fireEvent.click(screen.getByTestId('prove-stop'))

    await waitFor(() => expect(screen.getByText(/Failed to stop the workload/)).toBeDefined())
    expect(onStopped).not.toHaveBeenCalled()
    expect(screen.getByTestId('prove-stop')).toBeDefined()
  })

  it('leaves an ordinary failure on the cockpit rather than the recovery panel', () => {
    const ordinary: AicrEvent[] = [
      { id: 1, runId: 'runY', at: '2026-08-17T00:00:00Z', kind: 'phase', level: 'info', phase: 'apply', message: 'phase started' },
      { id: 2, runId: 'runY', at: '2026-08-17T00:00:01Z', kind: 'error', level: 'error', phase: 'apply', message: 'deploy.sh failed: exit status 1' },
      { id: 3, runId: 'runY', at: '2026-08-17T00:00:02Z', kind: 'phase', level: 'error', phase: 'apply', message: 'run failed' },
    ]
    render(<Wizard events={ordinary} />)

    expect(screen.queryByTestId('recovered-run')).toBeNull()
    expect(screen.queryByTestId('recovery-discard')).toBeNull()
    expect(screen.getByRole('heading', { name: /install failed/i })).toBeDefined()
  })
})
