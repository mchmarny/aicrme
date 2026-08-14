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
  gaps: CapabilityGap[]
}

export function Discover({ report }: { report: CapabilityReport }) {
  return (
    <section className="mx-auto max-w-2xl space-y-6">
      <div>
        <h2 className="text-2xl font-semibold text-slate-100">{report.headline}</h2>
        {report.detail && <p className="mt-1 text-sm text-slate-400">{report.detail}</p>}
      </div>

      <ul className="space-y-2">
        {report.gaps.map(g => (
          <li key={g.id} data-testid={`gap-${g.id}`} className="rounded border border-slate-800 bg-slate-900 p-3">
            <p className="text-slate-200">{g.title}</p>
            <p className="mt-1 text-xs text-slate-500">Closed by {g.component}</p>
          </li>
        ))}
      </ul>

      <p data-testid="punchline" className="text-xl font-semibold text-amber-400">
        {report.punchline}
      </p>

      <details className="text-sm text-slate-500">
        <summary className="cursor-pointer">Node detail, driver versions, taints, labels, raw snapshot</summary>
        <pre className="mt-2 overflow-auto text-xs">{JSON.stringify(report, null, 2)}</pre>
      </details>
    </section>
  )
}
