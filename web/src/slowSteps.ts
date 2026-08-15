/**
 * Contextual slow-step explanations, surfaced BEFORE a known multi-minute
 * stall rather than after it. This is not decoration: every GPU cluster
 * install stalls somewhere, and an unexplained stall is precisely where a
 * demo audience concludes the tool is broken. Naming it before it happens
 * converts the worst moment into a credibility moment.
 *
 * Deliberately unquantified. Real per-node timings are calibrated in Phase
 * 4 against real hardware; inventing minute counts here would put a
 * fabricated number on the screen during a KWOK demo, which is worse than
 * saying less.
 */
const EXACT: Record<string, string> = {
  'gpu-operator':
    'The driver DaemonSet compiles the NVIDIA kernel module against each node’s running kernel, then loads it. This is the longest step of the install and it is supposed to look stalled.',
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
