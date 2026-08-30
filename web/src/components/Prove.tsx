import { deriveComponents, deploymentActionsTotal, formatSeconds, installedCount, runElapsed } from '../pipeline'
import type { AicrEvent } from '../useEvents'
import type { Validation } from '../api'
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
 * `report.simulated` is internal/gap's own definition (gap.Report.Simulated):
 * true when kwok-controller is running, regardless of how many GPUs the fake
 * nodes report. The `totalGpus === 0` clause stays alongside it rather than
 * replacing it -- a real cluster that genuinely has no GPU nodes yet is still
 * not real hardware to claim a result against, and that case has no
 * kwok-controller to set the first flag. What changed is that the two are no
 * longer the SAME check: the e2e's four fake nodes advertise 32 GPUs total
 * (test/e2e/lib.sh), which used to read as a real cluster here while the same
 * report told Validate's skipReason (internal/steps/validate.go) the opposite.
 *
 * undefined is a real third answer, not a default. A run adopted at startup
 * (internal/engine/reconcile.go) or recovered from a record has no capability
 * report in its stream at all, and claiming either "simulated" or a GPU count
 * with no measurement behind it would be the console making something up on
 * the one screen whose whole job is to show what actually happened.
 */
export function isSimulated(run: RunState): boolean | undefined {
  if (!run.report) return undefined
  return run.report.simulated || run.report.totalGpus === 0
}

function Claim({ run }: { run: RunState }) {
  const simulated = isSimulated(run)

  if (simulated === undefined) {
    return (
      <p className="text-sm text-ink-soft">
        This console has no capability report for this run, so it makes no claim about
        the hardware underneath — only that the workload below is placed and running.
      </p>
    )
  }

  if (simulated) {
    return (
      <p data-testid="prove-simulated" className="text-sm text-ink-soft">
        <span className="text-warn">Simulated cluster, no GPU hardware.</span>{' '}
        Nothing here computed a result, and this screen claims none. What is real is the
        decision below: a gang-scheduled job was admitted and every member was bound to a
        node together, by the scheduler this console installed.
      </p>
    )
  }

  // Past tense once the run is over. This sentence used to say the gang "is
  // placed and running" underneath a heading reading "The reference workload
  // has stopped" -- the screen contradicting itself two lines apart, observed
  // on real H100s 2026-08-30.
  const running = run.state === 'active'

  return (
    <p data-testid="prove-real" className="text-sm text-ink-soft">
      Discover found <strong>{run.report?.usableGpus} of {run.report?.totalGpus} GPUs</strong>{' '}
      usable by a workload. The gang below {running ? 'is placed and running' : 'was placed'} on
      the components this console installed since.
    </p>
  )
}

/**
 * Summary is the run's own result line.
 *
 * The GPU figure appears only when the cluster reported one: on a simulated
 * cluster totalGpus is 0, and "0 of 0 GPUs" claims a measurement where there
 * was none -- the simulated caveat below already says the true thing.
 */
function Summary({ events, run, placed }: { events: AicrEvent[]; run: RunState; placed: number }) {
  const components = deriveComponents(events, run.recipe?.components.map(c => c.name))
  const actions = deploymentActionsTotal(components)
  const done = installedCount(components)
  const seconds = runElapsed(components, Date.now())
  const gpus = run.report && run.report.totalGpus > 0 ? run.report : undefined

  return (
    <p data-testid="prove-summary" className="mt-1 font-mono text-xs text-ink-faint">
      {actions !== undefined && <span>{done} of {actions} installed</span>}
      {seconds !== undefined && <span> in {formatSeconds(seconds)}</span>}
      {placed > 0 && <span> · gang of {placed} placed</span>}
      {gpus && <span> · {gpus.usableGpus} of {gpus.totalGpus} GPUs usable</span>}
    </p>
  )
}

/**
 * ValidationPanel reports what AICR's validator found, or why it did not run.
 *
 * A skip renders as a skip. On a simulated cluster the validator lands on
 * KWOK's fake nodes and reports passes for checks that never executed, so
 * "skipped" is the only honest thing this screen can say there -- the same
 * reason Prove labels a simulated placement rather than claiming throughput.
 */
function ValidationPanel({ validation }: { validation?: Validation }) {
  if (!validation || (!validation.skipped && !validation.phases?.length)) return null

  if (validation.skipped) {
    return (
      <div data-testid="prove-validation" className="text-xs text-ink-faint">
        <span className="text-warn">Validation skipped.</span> {validation.skipped}
      </div>
    )
  }

  return (
    <ul data-testid="prove-validation" className="space-y-1 font-mono text-xs">
      {validation.phases?.map(p => (
        <li key={p.phase} className="flex items-baseline gap-2">
          <span className={p.failed > 0 ? 'text-fail' : 'text-pass'}>
            {p.failed > 0 ? '✗' : '✓'}
          </span>
          <span className="text-ink">{p.phase}</span>
          <span className="text-ink-faint">
            {p.passed} of {p.tests} checks passed
            {p.failed > 0 ? `, ${p.failed} failed` : ''}
          </span>
        </li>
      ))}
    </ul>
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
  // Four states reach this screen, and each of them makes a different claim
  // true. Wizard renders it for the whole prove phase, not only for an active
  // run, so a two-way active/not-active split would tell an operator watching
  // the gang be placed that it "has stopped", and would tell one whose run
  // just failed here that nothing is holding their accelerators -- which the
  // console cannot know when its own cleanup could not be confirmed.
  const active = run.state === 'active'
  const stopped = run.state === 'done'
  const failed = run.state === 'failed'

  return (
    <section data-testid="prove" className="mx-auto max-w-2xl space-y-5">
      <div>
        <h2 className="text-2xl font-semibold text-ink-strong">
          {/* "placed", not "running": on a simulated cluster KWOK completes
              every pod in the same second it binds it, so a heading that
              claimed a running computation would be false on the substrate
              this screen most often renders against. Placement is the claim
              that holds on both. */}
          {/* While a Stop is in flight the heading leads with it. Otherwise
              the screen was dominated by "Your cluster placed a gang-scheduled
              workload / This run succeeded" -- a description of what already
              happened -- and the only copy explaining the one-to-two minute
              wait sat in small grey text under a disabled button. Observed on
              real H100s 2026-08-30. */}
          {active && busy && 'Stopping the reference workload…'}
          {active && !busy && 'Your cluster placed a gang-scheduled workload'}
          {stopped && 'The reference workload has stopped'}
          {failed && 'The reference workload did not run'}
          {!active && !stopped && !failed && 'Placing the reference workload…'}
        </h2>
        {/* THE SUCCESS SIGNAL, and it earns its place by ruling out a
            specific wrong conclusion rather than by decorating a good one.
            StateActive is this step's TERMINAL success state -- the reference
            workload is `sleep infinity` and holds its placement by design, so
            no later state ever arrives and nothing polls for one. Without a
            line saying so, the screen shows a neutral heading between two red
            destructive controls, and it was read as a failure on real
            hardware by the person who built it.

            Deliberately about the RUN, not the hardware: it is equally true
            on a simulated cluster, where the placement is exactly as real.
            Claim, below, is what keeps the hardware story honest. */}
        {active && (
          <p data-testid="prove-success" className="text-sm text-pass">
            This run succeeded. Prove is the last step and it ends here — the workload
            holds its placement until you stop it, so nothing further happens on its own.
          </p>
        )}
        {/* WHAT THE RUN ACHIEVED, in one line.
            This is the frame that ends up in a slide, and every number in it
            was already on the screen -- scattered across forty timeline
            lines, which is the same as not being there. Derived, never
            parsed: the counts come from the component events and the GPU
            figures from the capability report, so nothing here re-reads
            another package's prose. */}
        {active && <Summary events={events} run={run} placed={placed.length} />}
        {active && <ValidationPanel validation={run.validation} />}
        {/* The claim is about what the placement below means, so it is shown
            only when there is a placement to mean anything about. A run that
            failed before anything was bound has nothing to say here, and the
            error above it is already saying the true thing. */}
        {placed.length > 0 && <Claim run={run} />}
      </div>

      {/* A recovered or adopted run reached this screen without the operator
          watching it get here, and the timeline rail still carries recovery's
          own marker telling them to retry or discard -- neither of which the
          engine accepts for an active run. Saying so here is what keeps the
          two from contradicting each other. */}
      {active && run.recovered && (
        <p data-testid="prove-recovered" className="text-xs text-warn">
          This workload was already running when the console started. Stopping it is the
          only action available for it.
        </p>
      )}

      {placed.length > 0 ? (
        <ul data-testid="prove-placements" className="space-y-1 font-mono text-xs text-ink-soft">
          {placed.map(e => (
            <li key={e.id}>{e.message}</li>
          ))}
        </ul>
      ) : (
        <p className="text-sm text-ink-faint">
          {failed
            ? 'No gang member was ever bound to a node.'
            : 'Waiting for the scheduler to place every member of the gang…'}
        </p>
      )}

      {active && (
        <div className="space-y-2">
          {/* Outlined rather than solid, and matching ResetGate's first-stage
              button: this is a destructive action on a screen whose news is
              good, and two solid red blocks were the loudest thing on the
              successful outcome. Still red, still named plainly -- demoted in
              weight, not in clarity. */}
          <button
            data-testid="prove-stop"
            disabled={busy}
            onClick={onStop}
            className="rounded border border-fail/60 px-4 py-2 text-fail disabled:opacity-50"
          >
            {busy ? 'Stopping…' : 'Stop workload'}
          </button>
          {/* Stop is synchronous over a wait that is minutes long on a real
              cluster -- it deletes the workload and then waits for the pods to
              actually be gone. A disabled button was the entire feedback, so
              the screen was indistinguishable from a dead click for the whole
              operation. Naming what it is waiting on is what makes the wait
              read as work. */}
          {busy ? (
            // text-sm and text-ink, not text-xs and text-ink-soft: during the
            // wait this is the only sentence describing what is happening, and
            // it was the least prominent thing on a screen still celebrating
            // the finished run.
            <p data-testid="prove-stopping" className="text-sm text-ink">
              Deleting the workload and waiting for its pods to actually be gone. On a real
              cluster this takes a minute or two; the run closes when they are.
            </p>
          ) : (
            <p className="text-xs text-ink-faint">
              The workload keeps running until you stop it. Stopping deletes it and waits
              for its pods to actually be gone before the run is closed.
            </p>
          )}
        </div>
      )}

      {stopped && (
        <p data-testid="prove-stopped" className="text-sm text-ink-soft">
          The workload was deleted and its pods are gone. Nothing this console started is
          still holding the cluster's accelerators.
        </p>
      )}

      {/* Deliberately makes no claim about what is left in the cluster. The
          step cleans up after itself and usually confirms the workload gone,
          but when it cannot the engine says so by refusing to start a new run
          (internal/engine's unconfirmed-cleanup guard) -- and that fact is not
          in the event stream this screen derives from. Claiming "nothing is
          holding your accelerators" here would be the console asserting
          something it did not observe. */}
      {failed && (
        <p data-testid="prove-failed" className="text-sm text-ink-soft">
          This run failed at the Prove step; the error above is what it reported. The step
          removes what it created before failing, and if it could not confirm that, the
          console will refuse to start a new run until you resolve it.
        </p>
      )}
    </section>
  )
}
