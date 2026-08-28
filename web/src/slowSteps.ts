/**
 * Contextual slow-step explanations, surfaced BEFORE a known multi-minute
 * stall rather than after it. This is not decoration: every GPU cluster
 * install stalls somewhere, and an unexplained stall is precisely where a
 * demo audience concludes the tool is broken. Naming it before it happens
 * converts the worst moment into a credibility moment.
 *
 * Calibrated by test/hardware/measure.sh's Q4 against three real Applies on
 * two 2x a3-megagpu-8g H100 clusters (numbers in docs/STATE.md).
 *
 * NO SUPERLATIVES HERE, and that is a finding rather than a style rule. The
 * first calibration ran on one Apply and moved "the longest step of the
 * install" from gpu-operator onto kube-prometheus-stack. Two further runs
 * showed why that was a mistake in kind, not just in aim: which component is
 * slowest changes between runs of the same recipe on the same cluster.
 * Durations belong here; rankings do not.
 *
 * Every duration says "on real hardware" and brackets what was actually
 * observed. The same note renders during a KWOK demo, where these components
 * install in seconds, and a precise-sounding number is a fabricated one at
 * exactly the moment the tool is asking to be believed.
 */
const EXACT: Record<string, string> = {
  // Measured 137s, 441s, 99s. The widest spread of any component, and the
  // reason this note gives a range: the 441s run was the only one pulling
  // every image onto a cold node and provisioning the Prometheus PVC, and
  // the 99s run reused both. It is the step most worth explaining and the
  // least worth predicting.
  'kube-prometheus-stack':
    'One chart bringing up Prometheus, Alertmanager, Grafana, kube-state-metrics and a node-exporter DaemonSet that has to land on every node — Helm returns only once all of them are Ready. Two to seven minutes on real hardware: the first install on a cluster pulls every image and provisions Prometheus a volume, and a later one reuses both. Any Unhealthy warning below it is usually node-exporter’s readiness probe settling, and clears on its own.',
  // Measured 128s, 129s, 125s -- the most reproducible number in this file,
  // across two clusters and a cold/warm pair. It installs first because
  // everything after it depends on it: cert-manager issues the certificates
  // gpu-operator's webhooks present (see deriveComponents's note on teardown
  // order in pipeline.ts).
  'cert-manager':
    'Three Deployments — the controller, the CA injector and the admission webhook — and Helm holds until all three are Ready, because every component installed after this one depends on the certificates it issues. Reliably about two minutes on real hardware.',
  // Measured 36s and 32s on the two runs that timed it individually, both on
  // node images that already carried the driver -- AICR detects that and logs
  // "auto-disabled gpu-operator driver install: pre-installed driver detected
  // in snapshot", which is the mechanism behind the number. The compile below
  // is real where it happens; nobody has yet timed a node image without one.
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
