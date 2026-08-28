import { useState } from 'react'
import type { Options } from '../api'

// PlatformsByIntent, not the flat Platforms union, is what the platform
// radiogroup below renders from: on a `kind` cluster only 5 of 12 (intent,
// platform) pairs actually resolve, and Platforms alone would let the
// console offer one that dead-ends in Recommend. Provisional is not read
// here -- narrowing by intent behaves identically either way, it is only the
// mandatory re-fetch-on-awaiting_decision discipline (see Wizard.tsx) that
// depends on it. See internal/api/options.go's handleOptions doc comment.
export type { Options }

export interface ComponentSummary { name: string; kind: string; version: string; namespace: string }
export interface RecipeSummary { name: string; version: string; componentCount: number; components: ComponentSummary[] }

// Platform labels describe what the user types to use it, not what it is.
const platformLabel: Record<string, string> = {
  kubeflow: 'kubectl apply -f trainjob.yaml',
  slurm: 'sbatch train.sh',
  runai: 'runai submit',
  dynamo: 'dynamo deploy',
  nim: 'nim deploy',
  any: 'just the runtime',
}

/** union returns the deduped, sorted set of every platform across every intent -- the pre-narrowing choice shown before the user has picked one. */
function union(platformsByIntent: Record<string, string[]>): string[] {
  return [...new Set(Object.values(platformsByIntent).flat())].sort()
}

function Choice({ name, legend, values, value, onChange, describe }: {
  name: string; legend: string; values: string[]; value: string
  onChange: (v: string) => void; describe?: (v: string) => string
}) {
  return (
    <fieldset role="radiogroup" aria-label={legend} className="space-y-2">
      <legend className="text-sm font-medium text-ink">{legend}</legend>
      {values.map(v => (
        <label key={v} className="flex cursor-pointer items-center gap-3 rounded border border-line bg-panel p-3">
          <input type="radio" name={name} value={v} aria-label={v}
            checked={value === v} onChange={() => onChange(v)} />
          <span className="text-ink">{v}</span>
          {describe && <code className="ml-auto text-xs text-ink-faint">{describe(v)}</code>}
        </label>
      ))}
    </fieldset>
  )
}

// Recommend is the ask-form only. It never renders the resolved recipe:
// Wizard mounts it solely while the run is awaiting_decision, and the recipe
// does not exist until after the decisions are submitted. Wizard's own
// ResolvedRecommend renders it once it does.
export function Recommend({ options, onDecide }: {
  options: Options
  onDecide: (d: { intent: string; platform: string }) => void
}) {
  const [intent, setIntent] = useState('')
  const [platform, setPlatform] = useState('')

  const platformChoices = intent ? (options.platformsByIntent[intent] ?? []) : union(options.platformsByIntent)

  function chooseIntent(next: string) {
    setIntent(next)
    // A platform valid under the old intent can be dead under the new one --
    // clear it rather than let the user submit a narrowed-out pair.
    if (!(options.platformsByIntent[next] ?? []).includes(platform)) setPlatform('')
  }

  return (
    <section className="mx-auto max-w-2xl space-y-6">
      <Choice name="intent" legend="What is this cluster for?" values={options.intents}
        value={intent} onChange={chooseIntent} />
      <Choice name="platform" legend="How do you want to submit work?" values={platformChoices}
        value={platform} onChange={setPlatform} describe={v => platformLabel[v] ?? ''} />

      <button
        onClick={() => { if (intent && platform) onDecide({ intent, platform }) }}
        className="w-full rounded bg-accent py-2 font-medium text-bg disabled:opacity-40"
        disabled={!intent || !platform}
      >
        Continue
      </button>
      {/* Nothing is preselected here, deliberately -- unlike Connect, which
          preselects because the kubeconfig's current-context is a real
          signal about what the operator meant. There is no equivalent signal
          for intent, and inventing a default would put a recipe choice in
          the console's hands. What a disabled button owes the operator is a
          reason, which this had none of. */}
      {(!intent || !platform) && (
        <p className="text-xs text-ink-faint">
          {!intent && !platform && 'Choose what the cluster is for, and how you submit work.'}
          {intent && !platform && 'Choose how you submit work.'}
          {!intent && platform && 'Choose what the cluster is for.'}
        </p>
      )}
    </section>
  )
}
