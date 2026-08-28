import { describe, expect, it } from 'vitest'
import { slowStepNote } from './slowSteps'

// Calibrated against run 715521fe05b0248a on real GKE H100s (2x a3-megagpu-8g),
// timed by test/hardware/measure.sh's Q4 and recorded in docs/STATE.md:
// kube-prometheus-stack 137s and cert-manager 128s are 44% of a 15m18s Apply,
// and every other component -- gpu-operator included -- came in at or under
// 49s. Q4's own output flags any timed component missing from this map with
// "⇐ NOT in slowSteps.ts"; those two were the whole of that gap.
//
// These tests pin the claims that measurement licenses, and no more. They
// deliberately match on fragments rather than whole sentences: the wording is
// meant to be edited, the quantification and the qualifier are not.

describe('slowStepNote', () => {
  it('quantifies the two components measured slowest on real hardware', () => {
    for (const name of ['kube-prometheus-stack', 'cert-manager']) {
      const note = slowStepNote(name)
      expect(note, name).toBeDefined()
      // The point of calibrating: an operator watching a two-minute stall is
      // told it is two minutes, not merely that it is slow.
      expect(note, name).toMatch(/two minutes/)
      // Every duration carries the cluster it was measured on, because this
      // same note renders during a KWOK demo where both install in seconds.
      expect(note, name).toMatch(/real hardware/)
    }
  })

  it('stops calling gpu-operator the longest step of the install', () => {
    // It was never timed when that claim was written, and on the one cluster
    // where it has been, it was not: GKE's H100 node image ships the driver,
    // so the compile the note describes did not happen and the step came in
    // under 49s. The driver explanation survives -- a node image without a
    // driver still compiles one -- but the superlative moves to the component
    // that measured 137s.
    const note = slowStepNote('gpu-operator')
    expect(note).toMatch(/driver/)
    expect(note).not.toMatch(/longest/)
    expect(slowStepNote('kube-prometheus-stack')).toMatch(/longest/)
  })

  it('keeps kai-scheduler, the one component installed without --wait', () => {
    expect(slowStepNote('kai-scheduler')).toMatch(/--wait/)
  })

  it('answers a readiness gate by suffix, since the bundler names one per recipe', () => {
    const note = slowStepNote('gpu-operator-readiness')
    expect(note).toMatch(/readiness gate/)
    expect(slowStepNote('nvsentinel-readiness')).toBe(note)
  })

  it('says nothing about a component with no measured stall', () => {
    // Silence is the default. A note on every row is a note the operator stops
    // reading, and only a stall long enough to look like a hang earns one --
    // nfd and kubeflow-trainer are both well inside the ≤49s band.
    expect(slowStepNote('nfd')).toBeUndefined()
    expect(slowStepNote('kubeflow-trainer')).toBeUndefined()
  })
})
