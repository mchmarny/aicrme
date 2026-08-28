import { describe, expect, it } from 'vitest'
import { slowStepNote } from './slowSteps'

// Calibrated by test/hardware/measure.sh's Q4 against three real Applies on
// two GKE 2x a3-megagpu-8g H100 clusters, recorded in docs/STATE.md:
//
//   cert-manager           128s  129s  125s
//   kube-prometheus-stack  137s  441s   99s
//   gpu-operator            ≤49s  36s   32s
//
// Q4's own output flags any timed component missing from this map with
// "⇐ NOT in slowSteps.ts"; cert-manager and kube-prometheus-stack were that
// gap, and nothing else has been slow on more than one run.
//
// These tests pin the claims that measurement licenses, and no more. They
// deliberately match on fragments rather than whole sentences: the wording is
// meant to be edited, the quantification and the qualifier are not.

describe('slowStepNote', () => {
  it('quantifies the two components measured slowest on real hardware', () => {
    for (const name of ['kube-prometheus-stack', 'cert-manager']) {
      const note = slowStepNote(name)
      expect(note, name).toBeDefined()
      // The point of calibrating: an operator watching a multi-minute stall
      // is told how many minutes, not merely that it is slow.
      expect(note, name).toMatch(/minutes/)
      // Every duration carries the cluster it was measured on, because this
      // same note renders during a KWOK demo where both install in seconds.
      expect(note, name).toMatch(/real hardware/)
    }
  })

  it('brackets kube-prometheus-stack rather than predicting it', () => {
    // 137s, 441s, 99s across three runs -- the cold-cluster run pulled every
    // image and provisioned the Prometheus PVC, the warm one reused both. A
    // single figure here would be wrong by 4x on one run in three.
    expect(slowStepNote('kube-prometheus-stack')).toMatch(/two to seven minutes/i)
  })

  it('claims no component is THE longest step', () => {
    // Which component is slowest changed between runs of the same recipe on
    // the same cluster: kube-prometheus-stack led the cold run at 441s,
    // cert-manager led the warm one at 125s against its 99s. The first
    // calibration shipped that superlative on gpu-operator, then moved it to
    // kube-prometheus-stack; both were unsupportable for the same reason.
    for (const name of ['gpu-operator', 'kube-prometheus-stack', 'cert-manager', 'kai-scheduler']) {
      expect(slowStepNote(name), name).not.toMatch(/longest/)
    }
    // The gpu-operator note still explains the driver compile, which is the
    // part that survives: it is real on a node image that ships no driver.
    expect(slowStepNote('gpu-operator')).toMatch(/driver/)
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
