#!/usr/bin/env bash
# Installs aicrme from its GitHub releases.
#
#   curl -fsSL https://raw.githubusercontent.com/mchmarny/aicrme/main/scripts/install.sh | bash
#
# Environment:
#   AICRME_VERSION  tag to install (default: the latest release)
#   AICRME_BIN_DIR  install directory (default: /usr/local/bin, else ~/.local/bin)
#
# This script FAILS CLOSED. An unknown platform, a checksum mismatch, or a
# failed attestation aborts without installing anything. Everything is staged in
# a temp directory and only moved into place once verified, so a run that dies
# part-way leaves no half-installed binary behind -- which matters more than
# usual for something people pipe from the internet into bash.
set -euo pipefail

REPO="mchmarny/aicrme"
: "${AICRME_VERSION:=}"
: "${AICRME_BIN_DIR:=}"

fail() { echo "error: $*" >&2; exit 1; }
info() { echo "==> $*"; }

# platform_target maps the host to the OS_ARCH pair goreleaser puts in archive
# names. Kept as its own function with no side effects so test/install_test.sh
# can call it directly: this mapping and .goreleaser.yaml's name_template have
# to agree, and nothing else would notice if they drifted.
platform_target() {
  local os arch
  case "$(uname -s)" in
    Darwin) os=darwin ;;
    Linux)  os=linux ;;
    *) echo "unsupported operating system: $(uname -s)" >&2; return 1 ;;
  esac
  # uname -m says arm64 on Apple silicon and aarch64 on Linux; goreleaser calls
  # both arm64. x86_64 and amd64 likewise.
  case "$(uname -m)" in
    x86_64|amd64)  arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) echo "unsupported architecture: $(uname -m)" >&2; return 1 ;;
  esac
  echo "${os}_${arch}"
}

# latest_tag resolves the newest release without needing a token. The
# /releases/latest URL redirects to the tag, so the tag is the last path
# segment of wherever it lands -- no API call, no rate limit, no jq.
latest_tag() {
  local url
  url="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest")" \
    || { echo "could not reach GitHub to resolve the latest release" >&2; return 1; }
  local tag="${url##*/}"
  [[ "${tag}" == v* ]] || { echo "unexpected redirect target: ${url}" >&2; return 1; }
  echo "${tag}"
}

# sha256_of prints the checksum of a file using whichever tool the host has.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    echo "neither sha256sum nor shasum is available; cannot verify the download" >&2
    return 1
  fi
}

# Sourced by the test rather than run: everything below is the install itself.
[[ "${AICRME_INSTALL_LIB:-}" == "1" ]] && return 0

command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v tar  >/dev/null 2>&1 || fail "tar is required"

target="$(platform_target)" || exit 1
tag="${AICRME_VERSION:-$(latest_tag)}" || exit 1
# Archive names carry the version without its leading v, matching goreleaser's
# .Version. The tag keeps it.
version="${tag#v}"
archive="aicrme_${version}_${target}.tar.gz"
base="https://github.com/${REPO}/releases/download/${tag}"

info "installing aicrme ${tag} (${target})"

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

curl -fsSL "${base}/${archive}" -o "${work}/${archive}" \
  || fail "could not download ${archive} from ${tag} -- is there a release for ${target}?"
curl -fsSL "${base}/checksums.txt" -o "${work}/checksums.txt" \
  || fail "could not download checksums.txt for ${tag}"

want="$(awk -v f="${archive}" '$2 == f || $2 == "*"f {print $1}' "${work}/checksums.txt" | head -1)"
[[ -n "${want}" ]] || fail "${archive} is not listed in checksums.txt"
got="$(sha256_of "${work}/${archive}")" || exit 1
[[ "${got}" == "${want}" ]] || fail "checksum mismatch for ${archive}: got ${got}, want ${want}"
info "checksum verified"

# Provenance, when the tooling is present. Absence of gh is not a failure --
# most people piping this script will not have it -- but a gh that is present
# and says no IS a failure, because that is a signed statement that this
# artifact did not come from the repo's release workflow.
if command -v gh >/dev/null 2>&1; then
  if gh attestation verify "${work}/${archive}" --repo "${REPO}" >/dev/null 2>&1; then
    info "build provenance verified"
  else
    fail "attestation verification FAILED for ${archive} -- refusing to install. Re-run with gh authenticated, or download and inspect manually."
  fi
else
  info "gh not found; skipping provenance check (checksum was still verified)"
fi

tar -xzf "${work}/${archive}" -C "${work}"
[[ -f "${work}/aicrme" ]] || fail "no aicrme binary inside ${archive}"
chmod +x "${work}/aicrme"

# Prefer a directory already on PATH and writable without sudo. Asking for root
# to install a tool that then runs with the user's own kubeconfig would be an
# odd trade.
dir="${AICRME_BIN_DIR}"
if [[ -z "${dir}" ]]; then
  if [[ -w /usr/local/bin ]]; then dir=/usr/local/bin; else dir="${HOME}/.local/bin"; fi
fi
mkdir -p "${dir}"
mv "${work}/aicrme" "${dir}/aicrme"
info "installed ${dir}/aicrme"

case ":${PATH}:" in
  *":${dir}:"*) ;;
  *) info "note: ${dir} is not on your PATH" ;;
esac

"${dir}/aicrme" --version
