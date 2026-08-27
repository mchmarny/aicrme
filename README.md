# aicrme
AI Cluster Runtime Configurer

A single binary that turns a vanilla GPU cluster into a working AI platform through a
browser console: it discovers the cluster, recommends a validated
[AICR](https://github.com/NVIDIA/aicr) configuration, installs it while streaming live cluster
telemetry, then runs a reference workload that proves the cluster works.

It runs on your machine, not in the cluster. You pick a context from your own kubeconfig, and it
drives the cluster from there.

## Install

Release automation is not wired up yet. Build it from source:

```sh
make build
./bin/aicrme
```

It binds a loopback address, prints a tokenized URL, and opens your browser at it. Flags:
`--addr` (default `127.0.0.1:0`, and a non-loopback address is refused rather than warned about),
`--kubeconfig`, `--context`, `--work-dir` (default `~/.aicrme`, or `AICRME_WORK_DIR`), and
`--open=false` to skip the browser.

`bash`, `helm` and `kubectl` must be on your PATH; `jq` is used when present. They are checked at
startup, before anything touches a cluster, and the resolved versions are recorded on every run
and shipped in its evidence bundle.

Direct internet access to `ghcr.io`, `nvcr.io`, and the upstream Helm repositories is an assumed
precondition. Air-gapped clusters are not supported.

## Security

**This console acts with your cluster credentials, and it is a demo and eval tool only.**

- It has exactly what your kubeconfig has. That needs no justification because it is not a grant:
  nothing is created in the cluster to give this console permission. It cannot be narrowed either
  — the console installs gpu-operator, cert-manager, DRA drivers, CRDs, and privileged
  DaemonSets, and creates namespaces, so in practice it wants what a cluster admin has.
- It holds those credentials for as long as it runs. The selected context is flattened into a
  per-launch file under the work directory so a `kubectl config use-context` mid-install cannot
  redirect it — which means a bearer token or client key in that context is inlined on disk until
  the process exits, and deleted when it does. An exec-based context (`gke-gcloud-auth-plugin`,
  `aws eks get-token`) flattens to the stanza rather than a secret, and is the benign case.
- AICR's snapshot agent runs privileged pods on GPU nodes in order to read `nvidia-smi` and PCIe
  topology. This is existing AICR behavior, not new exposure, but it is part of the same
  disclosure.
- The HTTP surface is loopback-only and authenticated by a launch token printed in the URL,
  exchanged once for a process-lifetime session cookie. There is no password, no user management,
  and no OIDC. The session dies with the process.
- Do not point it at a production cluster. Use disposable demo and eval clusters.

## Demo

To run the whole arc locally in a browser — Kind + simulated GPU nodes, a real install of every
component, live cockpit telemetry — see **[DEMO.md](DEMO.md)**:

```sh
make demo
```

## Development

```sh
make qualify   # full local gate: SPA build, lint, tests with coverage floor, AICR pin check
make help      # all targets
```

**[docs/STATE.md](docs/STATE.md)** is where to start: what is proven and on what hardware, what is
left to do, and how to run each verification. The `docs/phase-*.md` files are unmaintained working
notes from the phase that produced them.

## License

MIT. See [LICENSE](LICENSE).
