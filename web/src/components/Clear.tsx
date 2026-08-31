import type { SurveyRelease, SurveyResult } from '../api'

/**
 * day renders an ISO timestamp as a date. The date is the evidence; the time
 * is noise. "first deployed 2026-01-02" against "the rest is from today" is
 * the judgement being asked for.
 */
function day(iso: string): string {
  if (!iso) return 'unknown'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return 'unknown'
  // Release.FirstDeployed has no `omitempty`, so a Go zero time.Time -- the
  // value a failed `helm history` read leaves behind -- marshals as
  // "0001-01-01T00:00:00Z": truthy, and a valid JS Date. Kubernetes did not
  // exist in year 1, so any such date is that zero-value artifact rather
  // than a real first-deployed timestamp, and rendering it fabricates the
  // one field the whole recommendation rests on.
  if (d.getUTCFullYear() < 1990) return 'unknown'
  return d.toISOString().slice(0, 10)
}

function Row({ r }: { r: SurveyRelease }) {
  const id = `${r.namespace}-${r.name}`
  return (
    <li
      data-testid={r.nodeLevel ? `clear-node-level-${id}` : `clear-row-${id}`}
      className="border-t border-ink-faint/20 py-2"
    >
      <div className="flex items-baseline justify-between gap-3 font-mono text-xs">
        <span className={r.recommended ? 'text-ink-strong' : 'text-ink-soft'}>{r.name}</span>
        <span className="text-ink-faint">
          {r.chart} {r.chartVersion} · {r.namespace} · rev {r.revision} · first deployed {day(r.firstDeployed)}
        </span>
      </div>
      {/* An unrecommended row without a reason is an unanswerable question. */}
      {r.reason && <p className="mt-1 text-xs text-warn">{r.reason}</p>}
    </li>
  )
}

/**
 * ClearPanel reports the AICR components already on this cluster.
 *
 * READ-ONLY, deliberately and completely. This is increment 1 of the Clear
 * design: it exists to be pointed at a real cluster and judged before anything
 * is capable of deleting. The test asserting that no button renders is
 * load-bearing rather than incidental.
 *
 * FIVE OUTCOMES, KEPT APART. Loading, error, unavailable, empty and found were
 * one value once, and a failed survey then rendered exactly like a clean
 * cluster -- telling an operator there was nothing here when this console had
 * simply failed to look.
 */
export function ClearPanel({ result, recoveredRun }: {
  result: SurveyResult | 'loading'
  recoveredRun?: boolean
}) {
  // A recovered run owns these releases, so "no run in this console owns them"
  // would be false. internal/console sets info.RecoveredRun before this screen
  // renders, so the claim is suppressed rather than softened.
  if (recoveredRun) {
    return (
      <p data-testid="clear-recovered" className="mt-8 text-xs text-ink-faint">
        A run from a previous session is still active on this cluster, so this console already owns
        what is installed. Reset that run rather than clearing the cluster.
      </p>
    )
  }

  if (result === 'loading') {
    return (
      <p data-testid="clear-loading" className="mt-8 text-xs text-ink-faint">
        Checking what is already installed…
      </p>
    )
  }

  if (result.state === 'unavailable') return null

  if (result.state === 'error') {
    return (
      <p data-testid="clear-error" className="mt-8 text-xs text-warn">
        This console could not check what is already installed: {result.message}. That is not the
        same as finding nothing — the cluster may well carry AICR components this screen cannot see.
      </p>
    )
  }

  if (result.state === 'empty') {
    // THE DEFECT THE REVIEW FOUND. Matching nothing is not the same as
    // looking and finding nothing: if a catalog overlay fails to resolve,
    // aicrclient.Universe comes back incomplete and the chart that would
    // have matched this cluster's real gpu-operator is simply missing from
    // the map, so nothing matches and `incomplete` explains why. Saying
    // "clean" here is the one wrong answer an operator acts on
    // destructively, so an incomplete survey must say it could not look
    // properly rather than that it found nothing.
    if (!result.survey.complete) {
      return (
        <p data-testid="clear-empty-incomplete" className="mt-8 text-xs text-warn">
          This console could not look at this cluster properly, so it cannot say it is clean:{' '}
          {result.survey.incomplete}
        </p>
      )
    }
    return (
      <p data-testid="clear-empty" className="mt-8 text-xs text-ink-faint">
        No AICR components found on this cluster.
      </p>
    )
  }

  const { survey } = result
  const removable = survey.releases.filter(r => r.recommended).length

  return (
    <section data-testid="clear-panel" className="mt-8 rounded border border-fail/40 p-4">
      <h3 className="text-sm font-semibold uppercase tracking-wide text-fail">Already installed</h3>
      <p className="mt-1 text-sm text-ink-soft">
        This cluster already carries {survey.releases.length} AICR component
        {survey.releases.length === 1 ? '' : 's'} that no run in this console owns.
        {survey.complete && ` ${removable} of them look like one install.`}
      </p>

      {/* An incomplete survey recommends nothing, and says so rather than
          rendering a confident-looking list of unticked rows. */}
      {!survey.complete && (
        <p data-testid="clear-incomplete" className="mt-2 text-xs text-warn">
          This console is not confident about what it found, so it is suggesting nothing:{' '}
          {survey.incomplete}
        </p>
      )}

      {/* Shown before any decision exists, which is the point. An operator who
          learns after the fact that their nodes need rebooting has been told
          too late to decide differently. */}
      {survey.driverMode === 'managed' && (
        <p data-testid="clear-driver-warning" className="mt-3 text-xs text-warn">
          <strong>This cluster&rsquo;s GPU Operator manages the NVIDIA driver.</strong> Removing it can
          leave the <code>nvidia_uvm</code> kernel module wedged mid-unload, and the next install then
          fails driver validation until the GPU nodes are rebooted. Reboot them before reinstalling.
        </p>
      )}
      {survey.driverMode === 'unknown' && (
        <p data-testid="clear-driver-unknown" className="mt-3 text-xs text-warn">
          This console could not tell whether the GPU Operator manages this cluster&rsquo;s NVIDIA
          driver. If it does, removing it can require rebooting the GPU nodes before the next install
          will succeed.
        </p>
      )}

      <ul className="mt-3">
        {survey.releases.map(r => <Row key={`${r.namespace}/${r.name}`} r={r} />)}
      </ul>

      <p className="mt-3 text-xs text-ink-faint">
        Nothing here removes anything yet. This console is reporting what it found so the list can be
        checked against what you expect before removal is offered.
      </p>
    </section>
  )
}
