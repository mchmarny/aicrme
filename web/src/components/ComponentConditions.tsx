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
 * `terminal` (Ruling 38, Task 7 final fix wave, replacing fix round 2's
 * `tense` prop). Teardown stops the observer's informers the instant a run
 * reaches a terminal state (StateDone or StateFailed) -- deliberately, and
 * that is not being revisited here. The consequence for THIS component:
 * after teardown, nothing can ever publish a resolution for a condition
 * still open at that instant, and `tracked` (pipeline.ts) never expires it
 * either -- by design, since a genuinely broken component is exactly what
 * an operator needs to keep seeing (see deriveComponents's doc comment on
 * `tracked`). So a condition surviving to a terminal screen is not stale
 * information the console failed to update; it is the LAST thing the
 * observer ever saw, permanently, because nothing is watching anymore.
 * "(cluster activity while gpu-operator installs)" on that screen claims a
 * present fact the console has no way to still know. `terminal` swaps the
 * caption to "(last observed while gpu-operator installed)" -- the same
 * copy discipline that governs the temporal-correlation label itself: the
 * console states only what it actually knows, and post-teardown that is
 * "this was true when we stopped watching," not "this is true."
 * Cockpit.tsx passes `terminal` for both Done and Failed (both terminal
 * states that tear down the observer identically); Running leaves it at
 * its default `false`.
 */
export function ComponentConditions({ name, conditions, terminal = false }: { name: string; conditions: ClusterCondition[]; terminal?: boolean }) {
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

  const note = terminal ? `last observed while ${name} installed` : `cluster activity while ${name} installs`

  return (
    <p data-testid={`condition-${name}`} className={`mt-1 max-w-2xl text-xs ${severityClass[active.severity] ?? 'text-slate-400'}`}>
      {!reasonInMessage && <span className="font-mono">{active.reason}</span>}
      {active.message && <span className={`text-slate-500 ${reasonInMessage ? '' : 'ml-1'}`}>{active.message}</span>}
      <span className="ml-1 text-slate-600">({note})</span>
    </p>
  )
}
