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
  return [...components]
    .filter(c => !c.generated)
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

function ItemList({ items, tone }: { items: ResidueItem[]; tone: string }) {
  return (
    <ul className="mt-2 space-y-1">
      {items.map(i => (
        <li key={`${i.kind}/${i.namespace ?? ''}/${i.name}`} className={`font-mono text-xs ${tone}`}>
          <span>{i.kind} {i.name}</span>
          {i.namespace && <span className="text-slate-500"> in {i.namespace}</span>}
          <span className="text-slate-400"> — {i.skip ?? i.error}</span>
        </li>
      ))}
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
          <p data-testid="residue-warning" className="text-sm text-amber-400">
            {run.residue.summary}
          </p>
        )}
        <button
          type="button"
          data-testid="reset"
          disabled={busy}
          onClick={() => setConfirming(true)}
          className="rounded border border-red-500/60 px-3 py-1.5 text-sm text-red-300 disabled:opacity-50"
        >
          Reset this run
        </button>
      </div>
    )
  }

  return (
    <div data-testid="reset-confirm" className="space-y-4 rounded border border-red-500/40 p-4">
      <p className="text-sm text-slate-300">
        This uninstalls what this run installed, in reverse order, and removes the
        namespaces it created and left empty. Anything it cannot prove this run
        created is left in place and named.
      </p>

      <div>
        <h3 className="text-xs uppercase tracking-wide text-slate-500">Will attempt to remove</h3>
        <ul data-testid="reset-removals" className="mt-2 space-y-1">
          {removals.map(r => (
            <li key={`${r.namespace ?? ''}/${r.name}`} className="font-mono text-xs text-slate-300">
              <span>{r.name}</span>
              {r.namespace && <span className="text-slate-500"> in {r.namespace}</span>}
            </li>
          ))}
        </ul>
      </div>

      {failed.length > 0 && (
        <div data-testid="reset-failed">
          <h3 className="text-xs uppercase tracking-wide text-slate-500">
            Left behind by the last reset
          </h3>
          <ItemList items={failed} tone="text-red-300" />
        </div>
      )}

      {skipped.length > 0 && (
        <div data-testid="reset-skipped">
          <h3 className="text-xs uppercase tracking-wide text-slate-500">
            Left in place — not created by this run
          </h3>
          <ItemList items={skipped} tone="text-amber-300" />
        </div>
      )}

      <div className="flex gap-2">
        <button
          type="button"
          data-testid="reset-confirmed"
          disabled={busy}
          onClick={onReset}
          className="rounded bg-red-600 px-3 py-1.5 text-sm text-white disabled:opacity-50"
        >
          Remove {removals.length} releases
        </button>
        <button
          type="button"
          data-testid="reset-cancel"
          onClick={() => setConfirming(false)}
          className="rounded border border-slate-600 px-3 py-1.5 text-sm text-slate-300"
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

  return (
    <section className="space-y-4">
      <h2 className="text-lg text-slate-200">Removing what this run installed</h2>
      <ul data-testid="teardown-rows" className="space-y-1">
        {components.map(c => (
          <li key={c.name} data-testid={`teardown-${c.name}`} className="font-mono text-sm text-slate-300">
            <span>{c.name}</span>
            <span className="ml-2 text-xs uppercase text-slate-500">{c.status}</span>
            {c.reason && <span className="ml-2 text-xs text-slate-400">{c.reason}</span>}
          </li>
        ))}
      </ul>
      {run.residue && (
        <p data-testid="reset-summary" className="text-sm text-slate-300">{run.residue.summary}</p>
      )}
      {failed.length > 0 && <ItemList items={failed} tone="text-red-300" />}
      {skipped.length > 0 && <ItemList items={skipped} tone="text-amber-300" />}
    </section>
  )
}
