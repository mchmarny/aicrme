#!/usr/bin/env bash
# Proves a goreleaser-built binary actually carries the console.
#
# WHY THIS EXISTS
# The SPA is compiled by Vite and pulled in with go:embed. `make build` cannot
# get this wrong -- it depends on the `web` target -- but goreleaser invokes the
# Go toolchain directly, so the embed is only correct because .goreleaser.yaml
# has a `before` hook running `make web`.
#
# Delete that hook and everything still passes: four archives build, checksums
# are written, the attestation signs, `aicrme --version` answers. The only
# symptom is a blank browser tab for whoever installs it, and no Go test can see
# it because internal/web embeds whatever happens to be on disk when the
# compiler runs.
#
# So this runs the artifact end to end and asserts the console is served. It is
# the only check standing between a deleted hook and a shipped empty console.
set -euo pipefail

DIST="${1:-dist}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

fail() { echo "FAIL: $*" >&2; exit 1; }

# Match the host to the archive goreleaser named. `uname -m` says arm64 on
# Apple silicon and aarch64 on Linux; both are goreleaser's "arm64".
case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux)  os=linux ;;
  *) fail "unsupported OS for the smoke check: $(uname -s)" ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) fail "unsupported architecture for the smoke check: $(uname -m)" ;;
esac

archive="$(find "${DIST}" -maxdepth 1 -name "*_${os}_${arch}.tar.gz" -print -quit 2>/dev/null || true)"
[[ -n "${archive}" ]] || fail "no ${os}_${arch} archive in ${DIST}/ -- run goreleaser first"
echo "smoke: ${archive}"

work="$(mktemp -d)"
pid=""
cleanup() {
  [[ -n "${pid}" ]] && kill "${pid}" 2>/dev/null || true
  rm -rf "${work}"
}
trap cleanup EXIT

tar -xzf "${archive}" -C "${work}"
[[ -x "${work}/aicrme" ]] || fail "no aicrme binary inside ${archive}"

# --version must answer without a cluster, a kubeconfig, or a home directory:
# the Homebrew cask's own test runs exactly this, and a downloaded binary that
# needs a cluster to identify itself is not much of an identity.
version_out="$("${work}/aicrme" --version)"
echo "smoke: ${version_out}"
[[ "${version_out}" == aicrme\ * ]] || fail "--version printed %q, want it to start with 'aicrme '"
# "dev" means the ldflags path broke: goreleaser's -X did not reach
# internal/version, so a released binary would not know what it is.
[[ "${version_out}" != *"aicrme dev "* ]] || fail "--version says 'dev' -- ldflags did not reach internal/version"

# Port 0 lets the OS pick, so the URL has to be read back rather than assumed.
AICRME_WORK_DIR="${work}/state" "${work}/aicrme" --open=false --addr 127.0.0.1:0 \
  >"${work}/out.log" 2>"${work}/err.log" &
pid=$!

url=""
for _ in $(seq 1 40); do
  url="$(grep -ohE 'http://127\.0\.0\.1:[0-9]+/\?t=[A-Za-z0-9_-]+' "${work}/err.log" "${work}/out.log" 2>/dev/null | head -1 || true)"
  [[ -n "${url}" ]] && break
  kill -0 "${pid}" 2>/dev/null || { cat "${work}/err.log" >&2; fail "the binary exited before printing a URL"; }
  sleep 0.25
done
[[ -n "${url}" ]] || { cat "${work}/err.log" >&2; fail "no tokenized URL after 10s"; }
echo "smoke: serving at ${url}"

body="$(curl -fsS --max-time 10 "${url}")" || { cat "${work}/err.log" >&2; fail "could not fetch the console"; }

# The console shell, and a hashed Vite bundle. An empty internal/web/dist
# produces neither -- which is the whole point of this check.
[[ "${body}" == *"<!doctype html"* ]] || fail "root did not serve HTML: ${body:0:200}"
[[ "${body}" == *"/assets/index-"* ]] \
  || fail "no built asset bundle in the console -- the SPA was NOT embedded. Check .goreleaser.yaml's before hook. Body: ${body:0:200}"

echo "smoke: PASS -- the console is embedded and served"
