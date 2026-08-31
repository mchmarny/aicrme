// frames.mjs generates the still frames the README's demo GIF is assembled
// from. Run through scripts/make-demo.sh, never directly.
//
// THESE FRAMES ARE AN ILLUSTRATION, NOT A RECORDING. The data below is
// invented: no cluster was contacted to produce it, and the AWS account id is
// a placeholder because this repository is public. The README says so beside
// the image. Anything here that looks like a measurement -- a duration, a GPU
// count, a component version -- is representative of a real run's shape, not
// evidence from one.
//
// What IS real is the styling: every frame links the compiled stylesheet the
// application itself ships (internal/web/dist/assets/*.css) and uses only
// class names that appear in the real components. Tailwind emits utilities it
// finds in source, so a class invented here would silently render unstyled --
// which is the guard that keeps these frames honest about how the product
// looks, even though the content is staged.

import { mkdirSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

const OUT = process.argv[2]
const CSS = process.argv[3]
if (!OUT || !CSS) {
  console.error('usage: frames.mjs <out-dir> <css-href>')
  process.exit(1)
}
mkdirSync(OUT, { recursive: true })

// A placeholder account id. The real one must never land in a public README;
// an earlier scrub of this repository had to remove a real GKE address from
// HEAD and could not remove it from history.
const CTX = 'arn:aws:eks:us-east-1:000000000000:cluster/'
const CTX_NAME = 'aicr-demo-h100'

const esc = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')

/** rail renders the timeline, newest first -- the ordering the real one uses. */
function rail(lines) {
  const rows = lines.map(([time, text, tone]) => `
      <div class="flex gap-3 text-xs">
        <span class="shrink-0 font-mono text-ink-faint">${esc(time)}</span>
        <span class="font-mono ${tone === 'warn' ? 'text-warn' : 'text-ink'}">${esc(text)}</span>
      </div>`).join('')
  return `
    <aside class="w-full max-w-2xl min-w-80 shrink-0 basis-2/5 border-l border-ink-faint/20 pl-6">
      <div class="mb-3 text-xs text-ink-faint underline">download run log</div>
      <div class="space-y-2">${rows}</div>
    </aside>`
}

/** shell is the chrome every screen after Connect shares. */
function shell(body, railLines) {
  return `
  <header class="mb-8 flex items-baseline justify-between gap-6">
    <div class="flex min-w-0 items-baseline gap-3">
      <h1 class="text-xl font-semibold text-ink-strong">aicrme</h1>
      <span class="text-pass text-xs">connected</span>
      <span class="truncate font-mono text-xs text-ink-soft">${esc(CTX + CTX_NAME)}</span>
    </div>
    <div class="shrink-0 text-right">
      <span class="block font-mono text-xs text-ink-soft underline">AICR v0.20.0</span>
      <span class="block font-mono text-xs text-ink-faint">aicrme v0.2.0 · 2026-08-31 · 6a69f04</span>
    </div>
  </header>
  <div class="flex gap-8" style="min-height:340px">
    <main class="min-w-0 flex-1">${body}</main>
    ${railLines ? rail(railLines) : ''}
  </div>`
}

const frames = []

// 1. Connect. The screen that answers "which cluster", with the ARN prefix
// dimmed so the cluster name leads -- a real finding from an EKS run where
// every row began with forty identical characters.
frames.push(`
  <div class="mx-auto max-w-2xl">
    <h2 class="mb-2 text-2xl font-semibold text-ink-strong">Connect a cluster</h2>
    <p class="mb-6 text-sm text-ink-soft">aicrme drives the cluster with your own credentials, for as long as it runs.</p>
    <div class="mb-4 rounded border border-ink-faint/30 px-4 py-3 text-sm text-ink-faint">Filter 147 contexts…</div>
    <div class="space-y-2">
      <div class="rounded border border-accent/60 px-4 py-3">
        <div class="font-mono text-sm"><span class="text-ink-faint">${esc(CTX)}</span><span class="text-ink-strong">${esc(CTX_NAME)}</span></div>
        <div class="mt-1 font-mono text-xs text-ink-faint">https://ABCDEF0123456789.gr7.us-east-1.eks.amazonaws.com</div>
      </div>
      <div class="rounded border border-ink-faint/20 px-4 py-3">
        <div class="font-mono text-sm text-ink-soft">gke-training-a100</div>
      </div>
      <div class="rounded border border-ink-faint/20 px-4 py-3">
        <div class="font-mono text-sm text-ink-soft">aks-inference-l40s</div>
      </div>
    </div>
    <div class="mt-6 rounded bg-accent py-3 text-center font-medium text-black">Connect</div>
  </div>`)

// 2. Connected, carrying the survey this release adds. It is the only screen
// that reports what a cluster already has BEFORE anything is installed.
frames.push(shell(`
    <h2 class="mb-4 text-2xl font-semibold text-ink-strong">Connected</h2>
    <dl class="space-y-1 font-mono text-xs">
      <div class="flex gap-4"><dt class="w-20 text-ink-faint">version</dt><dd class="text-ink">v1.31.4</dd></div>
      <div class="flex gap-4"><dt class="w-20 text-ink-faint">nodes</dt><dd class="text-ink">5 total · 2 advertising GPUs</dd></div>
    </dl>
    <section class="mt-8 rounded border border-fail/40 p-4">
      <h3 class="text-sm font-semibold uppercase tracking-wide text-fail">Already installed</h3>
      <p class="mt-1 text-sm text-ink-soft">This cluster already carries 3 AICR components that no run in this console owns. 2 of them look like one install.</p>
      <p class="mt-3 text-xs text-warn"><strong>This cluster&rsquo;s GPU Operator manages the NVIDIA driver.</strong> Removing it can leave the <code>nvidia_uvm</code> kernel module wedged mid-unload, and the next install then fails driver validation until the GPU nodes are rebooted.</p>
      <ul class="mt-3">
        <li class="border-t border-ink-faint/20 py-2">
          <div class="flex items-baseline justify-between gap-3 font-mono text-xs">
            <span class="text-ink-strong">gpu-operator</span>
            <span class="text-ink-faint">gpu-operator v26.3.3 · rev 1 · first deployed 2026-08-31</span>
          </div>
        </li>
        <li class="border-t border-ink-faint/20 py-2">
          <div class="flex items-baseline justify-between gap-3 font-mono text-xs">
            <span class="text-ink-soft">cert-manager</span>
            <span class="text-ink-faint">cert-manager 1.20.2 · rev 14 · first deployed 2026-01-02</span>
          </div>
          <p class="mt-1 text-xs text-warn">first deployed 2026-01-02, 7 months before the rest of this install</p>
        </li>
      </ul>
    </section>`, [
  ['1:28:13 PM', 'connected to the cluster'],
]))

// 3. Discover. The gap report, before a single component is chosen.
frames.push(shell(`
    <h2 class="mb-2 text-2xl font-semibold text-ink-strong">What is this cluster for?</h2>
    <div class="mt-4 space-y-2">
      <div class="rounded border border-ink-faint/20 px-4 py-3 text-sm text-ink">inference</div>
      <div class="rounded border border-accent/60 px-4 py-3 text-sm text-ink-strong">training</div>
    </div>
    <p class="mt-6 text-sm text-ink-faint">Choose what the cluster is for, and how you submit work.</p>`, [
  ['1:28:18 PM', 'awaiting decision'],
  ['1:28:18 PM', '8 of 8 GPUs are usable by a workload today.'],
  ['1:28:18 PM', 'No GPU-aware scheduler, no gang scheduling', 'warn'],
  ['1:28:18 PM', 'No GPU metrics, utilization is invisible', 'warn'],
  ['1:28:18 PM', 'No device plugin, Kubernetes cannot schedule', 'warn'],
  ['1:28:13 PM', 'discover phase started'],
]))

// 4. The bundle, reviewable before it touches anything.
frames.push(shell(`
    <h2 class="mb-2 text-2xl font-semibold text-ink-strong">Review the bundle before it touches the cluster</h2>
    <p class="mb-5 text-sm text-ink-soft">15 components, 14 of 15 pinned to an upstream version; the rest are generated locally.</p>
    <div class="space-y-3 font-mono text-xs">
      <div><div class="uppercase tracking-wide text-ink-faint">gpu-operator</div><div class="text-ink">gpu-operator <span class="text-pass">v26.3.3</span> — closes: No device plugin, Kubernetes cannot schedule</div></div>
      <div><div class="uppercase tracking-wide text-ink-faint">kai-scheduler</div><div class="text-ink">kai-scheduler <span class="text-pass">v0.14.1</span> — closes: No GPU-aware scheduler</div></div>
      <div><div class="uppercase tracking-wide text-ink-faint">monitoring</div><div class="text-ink">kube-prometheus-stack <span class="text-ink-soft">84.4.0</span></div></div>
    </div>
    <div class="mt-6 inline-block rounded bg-accent px-6 py-2 font-medium text-black">Install</div>`, [
  ['1:28:32 PM', 'bundle ready: 70 files, 190696 bytes'],
  ['1:28:31 PM', '[gpu-operator] pre-installed driver observed on sampled node', 'warn'],
  ['1:28:31 PM', 'resolving recipe for intent=training platform=kubeflow'],
]))

// 5. The install, mid-flight, with the per-component narration.
frames.push(shell(`
    <h2 class="mb-3 text-2xl font-semibold text-ink-strong">Installing the bundle</h2>
    <p class="mb-2 font-mono text-xs text-ink-faint">9 of 16 installed · 15 components · 4m 12s elapsed</p>
    <div style="height:4px;border-radius:2px;background:#1b232e;margin-top:10px;margin-bottom:22px"><div style="height:4px;width:56%;border-radius:2px;background:#76b900"></div></div>
    <ul class="space-y-3 font-mono text-xs">
      <li class="text-pass">✓ cert-manager <span class="text-ink-faint">1m 58s</span></li>
      <li class="text-pass">✓ nfd <span class="text-ink-faint">22s</span></li>
      <li class="text-pass">✓ gpu-operator <span class="text-ink-faint">2m 41s</span></li>
      <li class="text-ink">kai-scheduler <span class="text-ink-faint">STARTED</span> <span class="text-pass">●</span> <span class="text-ink-faint">18s</span></li>
    </ul>`, [
  ['1:33:04 PM', '[kai-scheduler] installing kai-scheduler'],
  ['1:32:46 PM', '[gpu-operator] gpu-operator installed'],
  ['1:30:05 PM', '[gpu-operator] installing gpu-operator'],
  ['1:29:22 PM', 'Pre-flight checks passed'],
]))

// 6. Validation, narrated per check. Eight minutes of silence here was the
// single loudest complaint from the first real-hardware run.
frames.push(shell(`
    <h2 class="mb-3 text-2xl font-semibold text-ink-strong">Validating the deployment</h2>
    <p class="text-sm text-ink-soft">Everything below is installed. AICR is now checking that it actually reconciled — this can take several minutes and the run continues either way.</p>
    <p class="mt-6 text-xs uppercase tracking-wide text-ink-faint">Installed — from the previous step</p>
    <ul class="mt-2 space-y-3 font-mono text-xs opacity-40">
      <li class="text-pass">✓ gpu-operator <span class="text-ink-faint">2m 41s</span></li>
      <li class="text-pass">✓ kai-scheduler <span class="text-ink-faint">1m 04s</span></li>
    </ul>`, [
  ['1:45:54 PM', 'checking check-nvidia-smi'],
  ['1:45:53 PM', 'check gpu-operator-version: passed'],
  ['1:45:50 PM', 'check expected-resources: failed', 'warn'],
  ['1:37:49 PM', 'checking expected-resources'],
  ['1:37:45 PM', 'validating: 4 checks to run'],
]))

// 7. The payoff: the scheduler's own decision, named.
frames.push(shell(`
    <h2 class="mb-2 text-2xl font-semibold text-ink-strong">Your cluster placed a gang-scheduled workload</h2>
    <p class="text-sm text-warn">Every component installed and the gang placed, but 1 validation check failed: this deployment is running, not verified.</p>
    <p class="mt-1 font-mono text-xs text-ink-faint">16 of 16 installed in 8m 23s · gang of 2 placed · 8 of 8 GPUs usable</p>
    <ul class="mt-2 space-y-1 font-mono text-xs">
      <li class="flex items-baseline gap-2"><span class="text-fail">✗</span><span class="text-ink">deployment</span><span class="text-ink-faint">3 of 4 checks passed, 1 failed</span></li>
    </ul>
    <p class="mt-4 text-sm text-ink-soft">Discover found <strong>8 of 8 GPUs</strong> usable by a workload. The gang below is placed and running on the components this console installed since.</p>
    <ul class="mt-3 space-y-1 font-mono text-xs text-ink-soft">
      <li>gang member prove-0 placed on node ip-10-0-171-98</li>
      <li>gang member prove-1 placed on node ip-10-0-181-55</li>
    </ul>
    <div class="mt-6 inline-block rounded border border-fail/60 px-4 py-2 text-fail">Stop workload</div>`, [
  ['1:46:08 PM', 'gang placed; reference workload running'],
  ['1:46:05 PM', 'validation: 3 of 4 checks passed, 1 failed', 'warn'],
  ['1:46:05 PM', 'validate phase complete'],
]))

frames.forEach((body, i) => {
  const html = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<link rel="stylesheet" href="${CSS}">
<style>
  /* The application sets these on <body> at runtime (App.tsx), so the frames
     set them here rather than depending on a class that only React applies. */
  html,body { background:#0b0f14; color:#cdd6e4; margin:0; }
  .frame { width:1280px; height:470px; padding:34px 48px; box-sizing:border-box; overflow:hidden; }
</style></head>
<body><div class="frame">${body}</div></body></html>`
  writeFileSync(join(OUT, `frame-${String(i + 1).padStart(2, '0')}.html`), html)
})

console.log(`${frames.length} frames written to ${OUT}`)
