export interface CapabilityGap {
  id: string
  title: string
  detail?: string
  component: string
}

export interface CapabilityReport {
  headline: string
  detail?: string
  punchline: string
  usableGpus: number
  totalGpus: number
  // gap.Report.Analyzed (internal/gap/gap.go): zero gaps means either
  // "every capability present" or "nothing measured at all". Without this,
  // a cluster that was never snapshotted renders the same green
  // already-capable copy as one that is genuinely fully capable.
  analyzed: boolean
  // gap.Report.Gaps (internal/gap/gap.go) carries no `omitempty`, so Go
  // marshals a nil slice as JSON null, not `[]` -- and a nil slice is
  // exactly what a cluster with every gap closed produces (see
  // internal/gap.TestAnalyzeFullyCapableClusterHasNoGaps). That is the
  // product's own end state, not an edge case, so this type says so instead
  // of promising an array that is sometimes actually null.
  gaps: CapabilityGap[] | null
}

export function Discover({ report }: { report: CapabilityReport }) {
  const gaps = report.gaps ?? []

  return (
    <section className="mx-auto max-w-2xl space-y-6">
      <div>
        <h2 className="text-2xl font-semibold text-ink-strong">{report.headline}</h2>
        {report.detail && <p className="mt-1 text-sm text-ink-soft">{report.detail}</p>}
      </div>

      {!report.analyzed ? (
        <p data-testid="no-snapshot" className="text-warn">
          No cluster snapshot is available yet, so nothing has been measured — this is not a clean bill of health.
        </p>
      ) : gaps.length === 0 ? (
        <p data-testid="no-gaps" className="text-pass">
          Every capability this workload needs is already installed — there is nothing left to close.
        </p>
      ) : (
        <ul className="space-y-2">
          {gaps.map(g => (
            <li key={g.id} data-testid={`gap-${g.id}`} className="rounded border border-line bg-panel p-3">
              <p className="text-ink">{g.title}</p>
              <p className="mt-1 text-xs text-ink-faint">Closed by {g.component}</p>
            </li>
          ))}
        </ul>
      )}

      <p data-testid="punchline" className="text-xl font-semibold text-warn">
        {report.punchline}
      </p>

      {/*
        The fold deliberately promises nothing beyond CapabilityReport. The
        browser only ever receives that struct, published as the Data field
        of the discover phase's log event (internal/steps/discover.go) --
        the raw snapshot.yaml artifact never reaches the SPA, so copy
        offering "node detail, driver versions, taints, labels, raw
        snapshot" here would be advertising data this screen cannot show.
      */}
      <details className="text-sm text-ink-faint">
        <summary className="cursor-pointer">Full capability report as JSON</summary>
        <pre className="mt-2 overflow-auto text-xs">{JSON.stringify(report, null, 2)}</pre>
      </details>
    </section>
  )
}
