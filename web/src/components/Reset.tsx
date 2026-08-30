import { useState } from 'react'
import { deriveComponents, type ComponentState } from '../pipeline'
import type { AicrEvent } from '../useEvents'
import type { ResidueItem, RunState } from './Wizard'

/**
 * Removal is one thing the confirm gate promises to remove, or names as
 * something it will not. Built from what the console already knows -- the
 * run's own component rows -- rather than from a server-side dry run: the
 * ownership evidence lives in the run record, and a second endpoint that
 * recomputed it would be a second answer needing reconciling against the
 * one Reset actually acts on.
 */
export interface Removal {
  name: string
  namespace?: string
}

/**
 * plannedRemovals is what a Reset will attempt, newest first -- the order it
 * actually works in, so the list the operator reads matches the sequence
 * they are about to watch.
 *
 * Every component the run installed is listed. The console deliberately
 * does NOT predict which of them ownership will spare: that evidence is the
 * pre-Apply snapshot, it is not in the event stream, and a gate that
 * promised "12 releases" and then removed 9 would be worse than one that
 * says what it will try. What ownership skipped is reported as it happens
 * and summarized at the end (see Skipped, below).
 */
export function plannedRemovals(components: ComponentState[]): Removal[] {
  // Generated actions are INCLUDED. They were filtered out here on the grounds
  // that they are not recipe components, which is true and irrelevant:
  // gpu-operator-pre and kubeflow-trainer-post are real helm releases and Reset
  // uninstalls them. Excluding them made the gate promise "Remove 14 releases"
  // and then remove 16 -- the operator approving a smaller set than the one
  // that ran. Observed on real H100s 2026-08-30.
  return [...components]
    .sort((a, b) => (b.index ?? 0) - (a.index ?? 0))
    .map(c => ({ name: c.name, namespace: c.namespace }))
}

/** skippedItems are the things the last Reset declined to remove, and why. */
export function skippedItems(run: RunState): ResidueItem[] {
  return (run.residue?.items ?? []).filter(i => i.skip)
}

/** failedItems are the things the last Reset tried and could not remove. */
export function failedItems(run: RunState): ResidueItem[] {
  return (run.residue?.items ?? []).filter(i => i.error)
}

/**
 * cleanupCommand is what the operator runs to finish the job by hand, or
 * undefined when there is nothing for them to do.
 *
 * Only namespaces this run created get one. Reset deletes no namespaces at
 * all -- whoever applied the bundle owns the cleanup of what it applied, and
 * a namespace deleted out from under something is unrecoverable where one
 * left standing is a single command -- so every namespace it names is work
 * that has landed on the operator. Naming it without supplying the command
 * makes them reconstruct it; supplying one for a namespace that PREDATES the
 * install would be advising them to destroy something the console
 * deliberately did not touch.
 */
export function cleanupCommand(i: ResidueItem): string | undefined {
  if (i.kind !== 'namespace' || !i.created) return undefined
  return `kubectl delete namespace ${i.name}`
}

function ItemList({ items, tone }: { items: ResidueItem[]; tone: string }) {
  return (
    <ul className="mt-2 space-y-1">
      {items.map(i => {
        const command = cleanupCommand(i)
        return (
          <li key={`${i.kind}/${i.namespace ?? ''}/${i.name}`} className={`font-mono text-xs ${tone}`}>
            <span>{i.kind} {i.name}</span>
            {i.namespace && <span className="text-ink-faint"> in {i.namespace}</span>}
            <span className="text-ink-soft"> — {i.skip ?? i.error}</span>
            {command && (
              <div className="mt-0.5 select-all text-ink-faint">{command}</div>
            )}
          </li>
        )
      })}
    </ul>
  )
}

/**
 * ResetGate is the confirmation. Two clicks, not one, and the list of what
 * will be removed sits between them: Reset is the only operation in this
 * console that destroys rather than creates, and the second click has to be
 * made against a visible inventory rather than against a word.
 */
export function ResetGate({
  run, components, busy, onReset,
}: {
  run: RunState
  components: ComponentState[]
  busy: boolean
  onReset: () => void
}) {
  const [confirming, setConfirming] = useState(false)
  const removals = plannedRemovals(components)
  const skipped = skippedItems(run)
  const failed = failedItems(run)

  if (!confirming) {
    return (
      <div className="space-y-3">
        {run.residue?.incomplete && (
          <p data-testid="residue-warning" className="text-sm text-warn">
            {run.residue.summary}
          </p>
        )}
        <button
          type="button"
          data-testid="reset"
          disabled={busy}
          onClick={() => setConfirming(true)}
          className="rounded border border-fail/60 px-3 py-1.5 text-sm text-fail disabled:opacity-50"
        >
          Reset this run
        </button>
      </div>
    )
  }

  return (
    <div data-testid="reset-confirm" className="space-y-4 rounded border border-fail/40 p-4">
      <p className="text-sm text-ink">
        This uninstalls the releases this run installed, in reverse order. Anything it
        cannot prove this run created is left in place and named. Namespaces are never
        deleted — they are listed afterwards, with the command to remove them.
      </p>

      <div>
        <h3 className="text-xs uppercase tracking-wide text-ink-faint">Will attempt to remove</h3>
        <ul data-testid="reset-removals" className="mt-2 space-y-1">
          {removals.map(r => (
            <li key={`${r.namespace ?? ''}/${r.name}`} className="font-mono text-xs text-ink">
              <span>{r.name}</span>
              {r.namespace && <span className="text-ink-faint"> in {r.namespace}</span>}
            </li>
          ))}
        </ul>
      </div>

      {failed.length > 0 && (
        <div data-testid="reset-failed">
          <h3 className="text-xs uppercase tracking-wide text-ink-faint">
            Left behind by the last reset
          </h3>
          <ItemList items={failed} tone="text-fail" />
        </div>
      )}

      {skipped.length > 0 && (
        <div data-testid="reset-skipped">
          <h3 className="text-xs uppercase tracking-wide text-ink-faint">
            Left in place
          </h3>
          <ItemList items={skipped} tone="text-warn" />
        </div>
      )}

      <div className="flex gap-2">
        <button
          type="button"
          data-testid="reset-confirmed"
          disabled={busy}
          onClick={onReset}
          className="rounded bg-fail-fill px-3 py-1.5 text-sm text-ink-strong disabled:opacity-50"
        >
          Remove {removals.length} releases
        </button>
        <button
          type="button"
          data-testid="reset-cancel"
          onClick={() => setConfirming(false)}
          className="rounded border border-line-hi px-3 py-1.5 text-sm text-ink"
        >
          Cancel
        </button>
      </div>
    </div>
  )
}

/**
 * Reset is the teardown screen: the pipeline redrawn as a removal, plus
 * whatever the last attempt left behind.
 */
export function Reset({ events, run }: { events: AicrEvent[]; run: RunState }) {
  const components = deriveComponents(events, run.recipe?.components.map(c => c.name))
  const skipped = skippedItems(run)
  const failed = failedItems(run)

  // A teardown is finished when the residue inventory exists: Reset publishes
  // it once, at the end, after the last release is gone.
  const finished = run.residue !== undefined
  const done = components.filter(c => c.status === 'removed').length

  return (
    <section className="space-y-4">
      <h2 className="text-lg text-ink">
        {finished ? 'Reset complete' : 'Removing what this run installed'}
      </h2>
      {/* A count, because rows flipping one at a time never answered "is it
          done". The operator asked twice on real H100s 2026-08-30, and the
          second time it HAD finished -- the screen just never said so. */}
      <p data-testid="teardown-progress" className="text-sm text-ink-soft">
        {done} of {components.length} removed
        {!finished && components.length > 0 && ' — this takes a couple of minutes'}
      </p>
      <ul data-testid="teardown-rows" className="space-y-1">
        {components.map(c => {
          // REMOVED and REMOVING are two letters apart in the same weight and
          // colour, so the list could not be scanned -- the person who reported
          // it misread it himself a minute later. Apply's row vocabulary (mark,
          // colour) already solved this; reuse it rather than invent a third.
          const removed = c.status === 'removed'
          return (
            <li key={c.name} data-testid={`teardown-${c.name}`} className="font-mono text-sm">
              <span className={removed ? 'text-pass' : 'text-ink-faint'}>{removed ? '✓' : '·'}</span>
              <span className={`ml-2 ${removed ? 'text-ink-faint' : 'text-ink'}`}>{c.name}</span>
              <span className={`ml-2 text-xs uppercase ${removed ? 'text-ink-faint' : 'text-warn'}`}>
                {c.status}
              </span>
              {c.reason && <span className="ml-2 text-xs text-ink-soft">{c.reason}</span>}
            </li>
          )
        })}
      </ul>
      {run.residue && (
        <p data-testid="reset-summary" className="text-sm text-ink">{run.residue.summary}</p>
      )}
      {failed.length > 0 && <ItemList items={failed} tone="text-fail" />}
      {skipped.length > 0 && <ItemList items={skipped} tone="text-warn" />}
      {finished && (
        <p data-testid="reset-done" className="text-sm text-pass">
          Nothing further is running. You can close this tab.
        </p>
      )}
    </section>
  )
}
