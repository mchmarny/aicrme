/**
 * Contextual slow-step explanations, surfaced BEFORE a known multi-minute
 * stall rather than after it. This is not decoration: every GPU cluster
 * install stalls somewhere, and an unexplained stall is precisely where a
 * demo audience concludes the tool is broken. Naming it before it happens
 * converts the worst moment into a credibility moment.
 *
 * Calibrated once, on real GPU hardware: a 15m18s Apply on 2x a3-megagpu-8g
 * H100s, timed per component by test/hardware/measure.sh's Q4 (numbers in
 * docs/STATE.md). Two components account for 44% of it and every other one
 * finishes inside 49s, so the two that need explaining are the two below
 * that carry a duration.
 *
 * Every duration says "on real hardware" and none is precise. The same note
 * renders during a KWOK demo, where these components install in seconds, and
 * an unqualified "two minutes" would be a fabricated number on the screen at
 * exactly the moment the tool is asking to be believed. One measurement on
 * one cluster does not license more precision than that either.
 */
const EXACT: Record<string, string> = {
  // Measured 137s. The chart brings up Prometheus, Alertmanager, Grafana,
  // kube-state-metrics and a node-exporter DaemonSet that has to land on
  // every node, and --wait holds until all of them are Ready.
  'kube-prometheus-stack':
    'One chart bringing up Prometheus, Alertmanager, Grafana, kube-state-metrics and a node-exporter DaemonSet that has to land on every node — Helm returns only once all of them are Ready. The longest step of the install, about two minutes on real hardware. Any Unhealthy warning below it is usually node-exporter’s readiness probe settling, and clears on its own.',
  // Measured 128s. It installs first because everything after it depends on
  // it -- cert-manager issues the certificates gpu-operator's webhooks
  // present (see deriveComponents's note on teardown order in pipeline.ts).
  'cert-manager':
    'Three Deployments — the controller, the CA injector and the admission webhook — and Helm holds until all three are Ready, because every component installed after this one depends on the certificates it issues. About two minutes on real hardware.',
  // Measured at or under 49s, on a cluster whose node image already carried
  // the driver. The compile below is real where it happens; it is no longer
  // described as the longest step, because on the one cluster where anyone
  // has timed it, it was not.
  'gpu-operator':
    'Where the node image does not already ship an NVIDIA driver, the driver DaemonSet compiles the kernel module against each node’s running kernel and then loads it — that is supposed to look stalled. On a node image that does ship one (GKE’s H100 pools do), there is nothing to compile and this step is quick.',
  'kai-scheduler':
    'Installed without --wait: its custom resources reconcile asynchronously, so Helm returning does not mean the scheduler is ready yet.',
}

const READINESS_SUFFIX = '-readiness'
const READINESS_NOTE =
  'A readiness gate. It polls the components installed before it until they actually pass, on a long deadline the bundler derives — so it holds here on purpose rather than failing fast.'

export function slowStepNote(component: string): string | undefined {
  if (component.endsWith(READINESS_SUFFIX)) return READINESS_NOTE
  return EXACT[component]
}
