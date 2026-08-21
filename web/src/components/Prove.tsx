import type { AicrEvent } from '../useEvents'
import type { RunState } from './Wizard'

/**
 * placements returns the scheduler's own decisions for this run, in the
 * order they were observed.
 *
 * `cluster` + the prove phase is exactly and only what
 * internal/steps/prove.go's awaitGang emits: one event per gang member, the
 * instant that member's Pod.Spec.NodeName is set -- the field the scheduler
 * itself writes when it binds a pod. internal/observer's own KindCluster
 * telemetry carries no phase at all (it publishes outside the engine's emit
 * path, which is what stamps one), so it cannot land here, and the discover
 * phase's KindCluster gap lines are excluded by the same test.
 *
 * The messages render verbatim rather than being parsed into pod and node
 * fields. There is nothing to gain from re-deriving structure the producer
 * already phrased for a human, and a regex over another package's message
 * text is the exact coupling that has already cost this repo a silently dead
 * guard.
 */
export function placements(events: AicrEvent[], runId?: string): AicrEvent[] {
  return events.filter(e => e.kind === 'cluster' && e.phase === 'prove'
    && (!runId || e.runId === runId))
}

/**
 * isSimulated answers the one question this screen must not get wrong, and
 * returns undefined when it cannot answer it.
 *
 * `totalGpus === 0` is internal/gap's own definition of a simulated cluster,
 * not a heuristic invented here: gap.go's punchline() says "No GPU hardware
 * detected — this is a simulated cluster." for exactly that case, and the
 * recorded KWOK stream (src/fixtures/kwok-run.json) carries it.
 *
 * undefined is a real third answer, not a default. A run adopted at startup
 * (internal/engine/reconcile.go) or recovered from a record has no capability
 * report in its stream at all, and claiming either "simulated" or a GPU count
 * with no measurement behind it would be the console making something up on
 * the one screen whose whole job is to show what actually happened.
 */
export function isSimulated(run: RunState): boolean | undefined {
  if (!run.report) return undefined
  return run.report.totalGpus === 0
}

function Claim({ run }: { run: RunState }) {
  const simulated = isSimulated(run)

  if (simulated === undefined) {
    return (
      <p className="text-sm text-slate-400">
        This console has no capability report for this run, so it makes no claim about
        the hardware underneath — only that the workload below is placed and running.
      </p>
    )
  }

  if (simulated) {
    return (
      <p data-testid="prove-simulated" className="text-sm text-slate-400">
        <span className="text-amber-400">Simulated cluster, no GPU hardware.</span>{' '}
        Nothing here computed a result, and this screen claims none. What is real is the
        decision below: a gang-scheduled job was admitted and every member was bound to a
        node together, by the scheduler this console installed.
      </p>
    )
  }

  return (
    <p data-testid="prove-real" className="text-sm text-slate-400">
      Discover found <strong>{run.report?.usableGpus} of {run.report?.totalGpus} GPUs</strong>{' '}
      usable by a workload. The gang below is placed and running on the components this
      console installed since.
    </p>
  )
}

/**
 * Prove is the payoff screen and the console's only exit from an active run.
 *
 * It replaces the cockpit rather than extending it (the design's own open
 * question, resolved provisionally): the pipeline's job was to get here, and
 * every row in it is already `installed` by the time this renders.
 *
 * The Stop control is the whole reason this screen is not just a static
 * result. The workload is deliberately left running when the step returns --
 * the most valuable minutes of a demo are the ones after the narration -- so
 * something has to end it, and engine.Stop is the only thing that can:
 * Discard is rejected while a workload is running and Retry requires a failed
 * run. A screen without this button strands the operator with a cluster only
 * kubectl can clean up.
 */
export function Prove({ events, run, busy, onStop }: {
  events: AicrEvent[]
  run: RunState
  busy: boolean
  onStop: () => void
}) {
  const placed = placements(events, run.runId)
  const active = run.state === 'active'

  return (
    <section data-testid="prove" className="mx-auto max-w-2xl space-y-5">
      <div>
        <h2 className="text-2xl font-semibold text-slate-100">
          {active
            ? 'A gang-scheduled workload is running on your cluster'
            : 'The reference workload has stopped'}
        </h2>
        <Claim run={run} />
      </div>

      {/* A recovered or adopted run reached this screen without the operator
          watching it get here, and the timeline rail still carries recovery's
          own marker telling them to retry or discard -- neither of which the
          engine accepts for an active run. Saying so here is what keeps the
          two from contradicting each other. */}
      {active && run.recovered && (
        <p data-testid="prove-recovered" className="text-xs text-amber-400">
          This workload was already running when the console started. Stopping it is the
          only action available for it.
        </p>
      )}

      {placed.length > 0 ? (
        <ul data-testid="prove-placements" className="space-y-1 font-mono text-xs text-slate-400">
          {placed.map(e => (
            <li key={e.id}>{e.message}</li>
          ))}
        </ul>
      ) : (
        <p className="text-sm text-slate-500">
          {active
            ? 'Waiting for the scheduler to place every member of the gang…'
            : 'No placement decisions were recorded for this run.'}
        </p>
      )}

      {active ? (
        <div className="space-y-2">
          <button
            data-testid="prove-stop"
            disabled={busy}
            onClick={onStop}
            className="rounded bg-red-700 px-4 py-2 text-white disabled:opacity-50"
          >
            Stop workload
          </button>
          <p className="text-xs text-slate-500">
            The workload keeps running until you stop it. Stopping deletes it and waits
            for its pods to actually be gone before the run is closed.
          </p>
        </div>
      ) : (
        <p data-testid="prove-stopped" className="text-sm text-slate-400">
          The workload was deleted and its pods are gone. Nothing this console started is
          still holding the cluster's accelerators.
        </p>
      )}
    </section>
  )
}
