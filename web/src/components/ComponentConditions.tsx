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
 * The trailing note names `name`, the row's own action, and reads
 * "while `<name>` installs" -- not "while installing" (Minor 2, Task 7 fix
 * round 1): the spec's own phrasing (design doc line 69, "cluster activity
 * while `<action>` installs") names the subject, and without it the
 * participle dangles onto the nearest noun in the line, which is the
 * resource, not the action. Still deliberately NOT "caused by" or "owned
 * by": attribution here is a TEMPORAL correlation, not a claim of
 * ownership. deploy.sh explicitly warns that cluster convergence continues
 * asynchronously after `--wait` returns (deploy.sh.tmpl:488-492) --
 * Nodewright alone can run 10-20 minutes past the script exiting -- so the
 * console has no basis to say this action's workloads produced the
 * condition, only that the observer saw it while this action was the one
 * installing. See
 * docs/superpowers/specs/2026-08-17-aicrme-phase-2b-iii-design.md, Section 1.
 *
 * `tense` (Minor 2, Task 7 fix round 2): "installs" is present tense,
 * correct while the run is still going -- but Ruling 27 means a condition
 * can survive to the Done screen (a still-open pod condition beside "every
 * component installed successfully" is the exact case this feature exists
 * to surface, not to hide). Cockpit.tsx's Done passes `tense="past"` so the
 * caption reads "installed" there instead of describing a run that is
 * already over as still in progress.
 */
export function ComponentConditions({ name, conditions, tense = 'present' }: { name: string; conditions: ClusterCondition[]; tense?: 'present' | 'past' }) {
  const active = activeCondition(conditions)
  if (!active) return null

  // internal/observer/pods.go's podMessage and events.go's
  // eventMessage/eventResolutionMessage already embed the raw reason text in
  // the message itself (e.g. "gpu-operator/pod: ImagePullBackOff"); handlers.go's
  // Deployment/DaemonSet rollout messages ("3/8 ready") do not. Showing the
  // reason label only when the message doesn't already say it avoids printing
  // it twice for the former without losing it for the latter (Minor 3, Task 7
  // fix round 1).
  const reasonInMessage = active.message.includes(active.reason)

  return (
    <p data-testid={`condition-${name}`} className={`mt-1 max-w-2xl text-xs ${severityClass[active.severity] ?? 'text-slate-400'}`}>
      {!reasonInMessage && <span className="font-mono">{active.reason}</span>}
      {active.message && <span className={`text-slate-500 ${reasonInMessage ? '' : 'ml-1'}`}>{active.message}</span>}
      <span className="ml-1 text-slate-600">(cluster activity while {name} {tense === 'past' ? 'installed' : 'installs'})</span>
    </p>
  )
}
