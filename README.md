# aicrme
AI Cluster Runtime Configurer

A single Helm chart that turns a vanilla GPU cluster into a working AI platform through a
browser console: it discovers the cluster, recommends a validated
[AICR](https://github.com/NVIDIA/aicr) configuration, installs it while streaming live cluster
telemetry, then runs a reference workload that proves the cluster works.

## Install

```sh
helm install aicrme oci://ghcr.io/mchmarny/aicrme/charts/aicrme \
  -n aicrme --create-namespace
```

NOTES.txt prints the port-forward one-liner, the URL, and the generated password. The username
is always `admin` — the console is single-user and has no user management.

Direct internet access to `ghcr.io`, `nvcr.io`, and the upstream Helm repositories is an assumed
precondition. Air-gapped clusters are not supported.

## Security

**This console runs with `cluster-admin`, and it is a demo and eval tool only.**

- It is granted `cluster-admin` via a ClusterRoleBinding. There is no honest way to narrow it:
  the console installs gpu-operator, cert-manager, DRA drivers, CRDs, and privileged DaemonSets,
  and creates namespaces, so any hand-enumerated role breaks the first time a recipe gains a
  component.
- AICR's snapshot agent runs privileged pods on GPU nodes in order to read `nvidia-smi` and PCIe
  topology. This is existing AICR behavior, not new exposure, but it is part of the same
  disclosure.
- The default Service type is `ClusterIP`, reached by `kubectl port-forward`, deliberately.
  `--set service.type=LoadBalancer` is available, but a cluster-admin console fronted by a public
  address and one password is a cluster-takeover surface. Do not expose it on an untrusted
  network.
- Do not install it on a production cluster. Use disposable demo and eval clusters.

Auth is a single user, a session cookie (HttpOnly, SameSite=Strict, Secure when TLS is present),
constant-time password comparison, rate-limited login, and an 8-hour session. There is no OIDC,
no multi-user support, and no scoped RBAC — by design, and only defensible under the framing
above.

## Development

```sh
make qualify   # full local gate: SPA build, lint, tests with coverage floor, AICR pin check
make help      # all targets
```

## License

MIT. See [LICENSE](LICENSE).
