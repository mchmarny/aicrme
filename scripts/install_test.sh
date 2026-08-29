#!/usr/bin/env bash
# Tests scripts/install.sh's platform mapping.
#
# WHY THIS IS WORTH A TEST
# platform_target's output is half of a contract; .goreleaser.yaml's
# archives.name_template is the other half. If they drift, install.sh builds a
# URL for an asset that was never published and every user sees a 404 -- on a
# path that only runs after a release, so nothing before the release notices.
# This pins the mapping side; the archive names in dist/ pin the other.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fails=0

check() {
  local want="$1" got="$2" desc="$3"
  if [[ "${got}" != "${want}" ]]; then
    echo "FAIL: ${desc}: got ${got}, want ${want}" >&2
    fails=$((fails + 1))
  fi
}

# Source the functions without running the installer.
# shellcheck source=./install.sh
AICRME_INSTALL_LIB=1 . "${ROOT}/scripts/install.sh"

# uname is shadowed by a shell function so each host shape can be exercised on
# whatever machine happens to be running this.
run_case() {
  local sys="$1" machine="$2"
  uname() {
    case "$1" in
      -s) echo "${sys}" ;;
      -m) echo "${machine}" ;;
      *) echo "unexpected uname flag: $1" >&2; return 1 ;;
    esac
  }
  platform_target
}

check "darwin_arm64" "$(run_case Darwin arm64)"   "Apple silicon"
check "darwin_amd64" "$(run_case Darwin x86_64)"  "Intel Mac"
check "linux_amd64"  "$(run_case Linux x86_64)"   "Linux x86_64"
check "linux_arm64"  "$(run_case Linux aarch64)"  "Linux aarch64 reports as arm64"
check "linux_arm64"  "$(run_case Linux arm64)"    "Linux arm64 spelled the BSD way"
check "linux_amd64"  "$(run_case Linux amd64)"    "Linux amd64 spelled the Go way"

# Unsupported platforms must fail rather than guess: a wrong guess produces a
# 404 the user has to interpret, while a refusal names the problem.
if run_case Windows_NT x86_64 >/dev/null 2>&1; then
  echo "FAIL: Windows was accepted; it must be refused" >&2
  fails=$((fails + 1))
fi
if run_case Linux i386 >/dev/null 2>&1; then
  echo "FAIL: 32-bit x86 was accepted; no such archive is published" >&2
  fails=$((fails + 1))
fi

# Every target this maps to must be one goreleaser actually builds. Parsed from
# the config rather than restated, so adding a platform to one and not the other
# is a test failure instead of a silent gap.
for want in darwin_arm64 darwin_amd64 linux_amd64 linux_arm64; do
  os="${want%%_*}"; arch="${want##*_}"
  grep -q "goos: \[.*${os}.*\]" "${ROOT}/.goreleaser.yaml" \
    || { echo "FAIL: .goreleaser.yaml does not build goos ${os}" >&2; fails=$((fails + 1)); }
  grep -q "goarch: \[.*${arch}.*\]" "${ROOT}/.goreleaser.yaml" \
    || { echo "FAIL: .goreleaser.yaml does not build goarch ${arch}" >&2; fails=$((fails + 1)); }
done

if (( fails > 0 )); then
  echo "${fails} failure(s)" >&2
  exit 1
fi
echo "install.sh: platform mapping OK"
