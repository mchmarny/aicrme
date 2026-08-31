import { afterEach, describe, expect, it, vi } from 'vitest'
import { surveyCluster } from './api'

afterEach(() => { vi.unstubAllGlobals() })

function stub(status: number, body: unknown) {
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(body), { status })))
}

const survey = {
  clusterUid: 'uid-1', driverMode: 'managed', complete: true,
  releases: [{ name: 'gpu-operator' }],
}

describe('surveyCluster', () => {
  it('reports found when the cluster carries AICR components', async () => {
    stub(200, survey)
    const got = await surveyCluster()
    expect(got.state).toBe('found')
  })

  // Empty is a real answer and a different one from every failure below. A
  // clean cluster is exactly what an operator wants to be told.
  it('reports empty distinctly from a failure', async () => {
    stub(200, { ...survey, releases: [] })
    const got = await surveyCluster()
    expect(got.state).toBe('empty')
  })

  // THE DEFECT THE REVIEW FOUND. Collapsing failure into the same value as
  // "nothing found" tells an operator their cluster is clean when this console
  // simply could not look.
  it('reports an error distinctly, and keeps the message', async () => {
    stub(500, { error: 'helm: not found' })
    const got = await surveyCluster()
    expect(got.state).toBe('error')
    if (got.state === 'error') expect(got.message).toBeTruthy()
  })

  // Unavailable is not an error the operator can act on: an older console has
  // no such route, one built without a surveyor answers 503, and 409 means no
  // cluster is connected yet. The panel is simply not offered.
  it('reports unavailable for 404, 503 and 409', async () => {
    for (const status of [404, 503, 409]) {
      stub(status, {})
      expect((await surveyCluster()).state).toBe('unavailable')
    }
  })

  // THE DEFECT THE REVIEW FOUND. writeErr (internal/api/runs.go) puts the
  // real diagnostic in the response body -- an RBAC denial, an unreachable
  // cluster, the bounded stderr clear.BashExec deliberately captures -- and
  // it must survive to the operator rather than being discarded for a bare
  // status code at this last hop.
  it('surfaces the server body\'s error over the bare status code', async () => {
    stub(500, { error: 'exec: "helm": executable file not found in $PATH' })
    const got = await surveyCluster()
    expect(got.state).toBe('error')
    if (got.state === 'error') expect(got.message).toBe('exec: "helm": executable file not found in $PATH')
  })

  it('falls back to the status code when the error body is unparseable, without throwing', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('not json', { status: 502 })))
    const got = await surveyCluster()
    expect(got.state).toBe('error')
    if (got.state === 'error') expect(got.message).toMatch(/HTTP 502/)
  })

  it('reports an error when the network fails', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => { throw new Error('offline') }))
    expect((await surveyCluster()).state).toBe('error')
  })
})
