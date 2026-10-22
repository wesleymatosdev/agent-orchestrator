#!/usr/bin/env bash
# Side-by-side proof video: run ONE Playwright spec against BASE and HEAD,
# then composite both recordings into a single comparison video.
#   usage: proof-side-by-side.sh <spec-path> <base-sha> <head-sha> <out.mp4> [test-filter]
set -euo pipefail
SPEC=$1; BASE=$2; HEAD=$3; OUT=$4; FILTER=${5:-}
ROOT=$(git rev-parse --show-toplevel)
run_one() { # $1=sha $2=dir
  git worktree add "$2" --detach "$1" >/dev/null 2>&1 || true
  ( cd "$2" && npm install --silent >/dev/null 2>&1; ( cd packages/product-ui && npm install --silent >/dev/null 2>&1 ) )
  ( cd "$2/frontend" && npx playwright test "$SPEC" ${FILTER:+--grep "$FILTER"} --update-snapshots >/dev/null 2>&1 || true )
  V=$(find "$2/frontend/test-results" -name video.webm 2>/dev/null | head -1)
  [ -n "$V" ] && cp "$V" "$2-recording.webm"
}
run_one "$BASE" "${TMPDIR:-/tmp}/sbs-base"
run_one "$HEAD" "${TMPDIR:-/tmp}/sbs-head"
[ -f "${TMPDIR:-/tmp}/sbs-base-recording.webm" ] && [ -f "${TMPDIR:-/tmp}/sbs-head-recording.webm" ] || { echo "missing recordings" >&2; exit 1; }
ffmpeg -y -i "${TMPDIR:-/tmp}/sbs-base-recording.webm" -i "${TMPDIR:-/tmp}/sbs-head-recording.webm" \
  -filter_complex "[0:v]scale=-2:810,drawtext=text='BEFORE (base)':fontcolor=white:fontsize=28:box=1:boxcolor=black@0.6:x=10:y=10[l];[1:v]scale=-2:810,drawtext=text='AFTER (branch)':fontcolor=white:fontsize=28:box=1:boxcolor=black@0.6:x=10:y=10[r];[l][r]hstack=inputs=2" \
  -c:v libx264 -pix_fmt yuv420p -r 30 "$OUT"
echo "WROTE $OUT"
