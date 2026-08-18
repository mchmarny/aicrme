import { activeCondition, type ClusterCondition } from '../pipeline'

const severityClass: Record<number, string> = {
  0: 'text-slate-400',
  1: 'text-amber-400',
  2: 'text-red-400',
}

/**
 * ComponentConditions renders the one cluster condition a row shows --
 * activeCondition's pick, or nothing once every condition on the row has
 * resolved.
 *
 * The trailing note is deliberately "while installing", not "caused by" or
 * "owned by": attribution here is a TEMPORAL correlation, not a claim of
 * ownership. deploy.sh explicitly warns that cluster convergence continues
 * asynchronously after `--wait` returns (deploy.sh.tmpl:488-492) -- Nodewright
 * alone can run 10-20 minutes past the script exiting -- so the console has
 * no basis to say this action's workloads produced the condition, only that
 * the observer saw it while this action was the one installing. See
 * docs/superpowers/specs/2026-08-17-aicrme-phase-2b-iii-design.md, Section 1.
 */
export function ComponentConditions({ name, conditions }: { name: string; conditions: ClusterCondition[] }) {
  const active = activeCondition(conditions)
  if (!active) return null

  return (
    <p data-testid={`condition-${name}`} className={`mt-1 max-w-2xl text-xs ${severityClass[active.severity] ?? 'text-slate-400'}`}>
      <span className="font-mono">{active.reason}</span>
      {active.message && <span className="ml-1 text-slate-500">{active.message}</span>}
      <span className="ml-1 text-slate-600">(cluster activity while installing)</span>
    </p>
  )
}
