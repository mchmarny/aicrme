#!/usr/bin/env bash
# make-demo.sh regenerates the README's demo animation.
#
# WHAT THIS PRODUCES IS AN ILLUSTRATION, NOT A RECORDING. No cluster is
# contacted. The frames are staged HTML with invented data, and the README says
# so beside the image. What is faithful is the appearance: every frame links
# the stylesheet the application itself ships, so the colours, type and spacing
# are the product's own rather than a designer's impression of them.
#
# Regenerate after a visual change:
#   make web && scripts/make-demo.sh
#
# Requires: Google Chrome (headless screenshots), ffmpeg (palette-based GIF),
# node. All three are checked below rather than failing halfway through.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${ROOT}/.github/demo.gif"
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

# Seconds each frame holds. The install and validate frames earn longer: they
# are the two an operator actually has to read, and the rest are recognisable
# at a glance.
HOLDS=(2.0 3.0 2.4 2.4 2.6 2.6 3.4)

CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
[[ -x "${CHROME}" ]] || CHROME="/Applications/Chromium.app/Contents/MacOS/Chromium"
[[ -x "${CHROME}" ]] || { echo "make-demo: no Chrome or Chromium found" >&2; exit 1; }
command -v ffmpeg >/dev/null || { echo "make-demo: ffmpeg not installed (brew install ffmpeg)" >&2; exit 1; }
command -v node   >/dev/null || { echo "make-demo: node not installed" >&2; exit 1; }

# The compiled stylesheet, not the source. Tailwind only emits utilities it
# finds in the components, so a class invented in a frame renders unstyled --
# which is what keeps the frames honest about how the product looks.
# A glob into an array rather than parsing ls: the build emits exactly one
# hashed stylesheet, and shellcheck is right that ls is the wrong tool for
# turning filenames into a list.
shopt -s nullglob
CSS_FILES=("${ROOT}"/internal/web/dist/assets/*.css)
shopt -u nullglob
[[ ${#CSS_FILES[@]} -gt 0 ]] || { echo "make-demo: no built stylesheet; run 'make web' first" >&2; exit 1; }
CSS="${CSS_FILES[0]}"

node "${ROOT}/scripts/demo/frames.mjs" "${WORK}" "file://${CSS}"

# Rendered at 2x and downscaled, so text is not soft. --headless=new is the
# only mode that honours --force-device-scale-factor.
i=0
for f in "${WORK}"/frame-*.html; do
    i=$((i + 1))
    "${CHROME}" --headless=new --disable-gpu --hide-scrollbars \
        --force-device-scale-factor=2 --window-size=1280,470 \
        --screenshot="${WORK}/shot-$(printf '%02d' "${i}").png" \
        "file://${f}" >/dev/null 2>&1
done
[[ ${i} -gt 0 ]] || { echo "make-demo: no frames rendered" >&2; exit 1; }

# Build a concat list that holds each frame for its own duration. The last
# entry is repeated because ffmpeg's concat demuxer ignores the final
# duration otherwise, and the closing frame is the one worth resting on.
LIST="${WORK}/list.txt"
: > "${LIST}"
for n in $(seq 1 "${i}"); do
    idx=$((n - 1))
    hold="${HOLDS[${idx}]:-2.4}"
    printf "file '%s/shot-%02d.png'\nduration %s\n" "${WORK}" "${n}" "${hold}" >> "${LIST}"
done
printf "file '%s/shot-%02d.png'\n" "${WORK}" "${i}" >> "${LIST}"

# Two passes. A single-pass GIF quantises against a generic palette and bands
# the accent green and amber badly -- the two colours that carry meaning here.
mkdir -p "$(dirname "${OUT}")"
ffmpeg -y -f concat -safe 0 -i "${LIST}" \
    -vf "fps=12,scale=1000:-1:flags=lanczos,palettegen=max_colors=128:stats_mode=diff" \
    "${WORK}/palette.png" >/dev/null 2>&1
ffmpeg -y -f concat -safe 0 -i "${LIST}" -i "${WORK}/palette.png" \
    -lavfi "fps=12,scale=1000:-1:flags=lanczos[x];[x][1:v]paletteuse=dither=bayer:bayer_scale=3" \
    -loop 0 "${OUT}" >/dev/null 2>&1

SIZE=$(( $(wc -c < "${OUT}") / 1024 ))
echo "wrote ${OUT} (${SIZE} KB, ${i} frames)"
# GitHub serves READMEs through a proxy that is unhappy well before this, and a
# reader on a phone pays for every byte. Warn rather than fail: the number to
# act on is the size, and the operator can decide.
[[ ${SIZE} -lt 8192 ]] || echo "make-demo: WARNING ${SIZE} KB is large for a README; drop fps or scale" >&2
