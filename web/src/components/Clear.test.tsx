import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { ClearPanel } from './Clear'
import type { ClusterSurvey, SurveyRelease, SurveyResult } from '../api'

function release(over: Partial<SurveyRelease> = {}): SurveyRelease {
  return {
    name: 'gpu-operator', namespace: 'gpu-operator', chart: 'gpu-operator',
    chartVersion: 'v26.3.3', component: 'gpu-operator', revision: 1,
    firstDeployed: '2026-08-30T20:29:00Z', lastUpdated: '2026-08-30T20:40:00Z',
    nodeLevel: false, recommended: true, ...over,
  }
}

function survey(over: Partial<ClusterSurvey> = {}): ClusterSurvey {
  return { clusterUid: 'uid-1', driverMode: 'host', complete: true, releases: [release()], ...over }
}

function found(over: Partial<ClusterSurvey> = {}): SurveyResult {
  return { state: 'found', survey: survey(over) }
}

describe('ClearPanel', () => {
  it('says it is still looking while the survey runs', () => {
    render(<ClearPanel result="loading" />)
    expect(screen.getByTestId('clear-loading')).toBeDefined()
  })

  // THE DEFECT THE REVIEW FOUND. A failed survey used to render exactly like a
  // clean cluster, telling an operator there was nothing here when this console
  // had simply failed to look.
  it('shows a failed survey as a failure, never as a clean cluster', () => {
    render(<ClearPanel result={{ state: 'error', message: 'helm: not found' }} />)

    const err = screen.getByTestId('clear-error')
    expect(err.textContent).toMatch(/helm: not found/)
    expect(screen.queryByTestId('clear-empty')).toBeNull()
  })

  it('says the cluster is clean when it is', () => {
    render(<ClearPanel result={{ state: 'empty', survey: survey({ releases: [] }) }} />)
    expect(screen.getByTestId('clear-empty')).toBeDefined()
  })

  // THE DEFECT THE REVIEW FOUND. Matching nothing is not the same as looking
  // and finding nothing: an incomplete universe can mean the chart that would
  // have matched this cluster's real gpu-operator never made it into the map.
  // "Clean" is the one wrong answer an operator acts on destructively.
  it('refuses to call the cluster clean when the survey that found nothing is incomplete', () => {
    render(<ClearPanel result={{
      state: 'empty',
      survey: survey({ releases: [], complete: false, incomplete: 'this console could not read 3 of AICR’s recipe overlays' }),
    }} />)

    const warning = screen.getByTestId('clear-empty-incomplete')
    expect(warning.textContent).toMatch(/could not read 3/)
    expect(screen.queryByTestId('clear-empty')).toBeNull()
  })

  it('renders nothing when the survey is unavailable', () => {
    render(<ClearPanel result={{ state: 'unavailable' }} />)
    expect(screen.queryByTestId('clear-panel')).toBeNull()
  })

  // A recovered run OWNS these releases, so "no run in this console owns them"
  // would be false. console.go sets info.RecoveredRun before this screen
  // renders, so the claim has to be suppressed rather than merely softened.
  it('does not claim releases are unowned when a run was recovered', () => {
    render(<ClearPanel result={found()} recoveredRun />)

    expect(screen.queryByTestId('clear-panel')).toBeNull()
    expect(screen.getByTestId('clear-recovered')).toBeDefined()
  })

  it('lists each release with the evidence to judge it', () => {
    render(<ClearPanel result={found()} />)

    const panel = screen.getByTestId('clear-panel')
    expect(panel.textContent).toMatch(/gpu-operator/)
    expect(panel.textContent).toMatch(/v26\.3\.3/)
    expect(panel.textContent).toMatch(/2026-08-30/)
  })

  // THE DEFECT THE REVIEW FOUND. Release.FirstDeployed has no `omitempty`, so
  // a `helm history` read that failed server-side marshals as Go's zero
  // time.Time ("0001-01-01T00:00:00Z") rather than as an empty string -- a
  // valid-looking date directly above a reason saying the console could not
  // establish when the release was deployed.
  it('renders a Go zero time as unknown, not as a fabricated date', () => {
    render(<ClearPanel result={found({
      releases: [release({ firstDeployed: '0001-01-01T00:00:00Z' })],
    })} />)

    const panel = screen.getByTestId('clear-panel')
    expect(panel.textContent).toMatch(/first deployed unknown/)
    expect(panel.textContent).not.toMatch(/0001-01-01/)
  })

  it('states why a release is not recommended', () => {
    render(<ClearPanel result={found({
      releases: [release({ name: 'cert-manager', recommended: false, reason: 'first deployed 2026-01-02, 8 months before the rest of this install' })],
    })} />)

    expect(screen.getByTestId('clear-panel').textContent).toMatch(/8 months before the rest of this install/)
  })

  // An incomplete survey recommends nothing, and must say so rather than
  // rendering a confident-looking list of unticked rows.
  it('says when its own evidence is incomplete', () => {
    render(<ClearPanel result={found({
      complete: false,
      incomplete: 'this console could not read 3 of AICR’s recipe overlays',
    })} />)

    expect(screen.getByTestId('clear-incomplete').textContent).toMatch(/could not read 3/)
  })

  it('warns about a driver-managed cluster', () => {
    render(<ClearPanel result={found({ driverMode: 'managed' })} />)

    const warning = screen.getByTestId('clear-driver-warning')
    expect(warning.textContent).toMatch(/reboot/i)
  })

  // Unknown is not "no warning". The operator is missing the fact that decides
  // whether their nodes need rebooting, and has to be told that.
  it('says so when it could not determine driver mode', () => {
    render(<ClearPanel result={found({ driverMode: 'unknown' })} />)
    expect(screen.getByTestId('clear-driver-unknown')).toBeDefined()
  })

  it('does not warn when the driver comes with the infrastructure', () => {
    render(<ClearPanel result={found({ driverMode: 'host' })} />)
    expect(screen.queryByTestId('clear-driver-warning')).toBeNull()
    expect(screen.queryByTestId('clear-driver-unknown')).toBeNull()
  })

  it('marks a node-level release as not removable', () => {
    // namespace: 'skyhook' -- not the release() default. nodewright-operator
    // always lives there (internal/clear/recommend.go's nodeLevelNamespace),
    // and the testid below encodes namespace/name the same way Row does.
    render(<ClearPanel result={found({
      releases: [release({ name: 'nodewright-operator', namespace: 'skyhook', nodeLevel: true, recommended: false, reason: 'node-level: removing this leaves the node as it is' })],
    })} />)

    expect(screen.getByTestId('clear-node-level-skyhook-nodewright-operator')).toBeDefined()
  })

  // Increment 1 is read-only. Nothing here may fire an action.
  it('offers no control that could remove anything', () => {
    render(<ClearPanel result={found()} />)
    expect(screen.queryByRole('button')).toBeNull()
  })
})
