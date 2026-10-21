#!/usr/bin/env bash
# Proof harness — deterministic green/red proof for this repo state, with receipts.
#
# Usage:
#   scripts/proof.sh          # CORE tier (default): fast, target < ~3 minutes
#   scripts/proof.sh --full   # FULL tier: explicitly slow (race suites + golangci-lint)
#   scripts/proof.sh --ui     # UI tier: navigates the real renderer (Playwright, ~1 min)
#
# Every check runs even after an earlier failure. Results print as
# PASS / FAIL / SKIP(reason) lines and are recorded in a receipt JSON under
# .proof/receipts/. Attach the before/after receipts when proving work.
#
# Tiers mirror CI (.github/workflows/go.yml + frontend checks); each is standalone:
#   CORE ~ backend "build-test" job (without -race, with -short) +
#          npm run frontend:typecheck + npm run product-ui:check
#   FULL ~ the -race suites go.yml runs + npm run lint (golangci-lint)
#   UI   ~ CI's renderer-smoke slice: Playwright drives the real renderer
#          under dev:web through the T0/P0 navigation specs (behavioral proof)
set -euo pipefail

MODE="core"
case "${1:-}" in
  "") ;;
  --full) MODE="full" ;;
  --ui) MODE="ui" ;;
  -h|--help)
    echo "usage: scripts/proof.sh [--full|--ui]"
    exit 0
    ;;
  *)
    echo "unknown argument: $1 (expected --full, --ui, or nothing)" >&2
    exit 2
    ;;
esac

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

RUN_TS="$(date -u +%Y%m%dT%H%M%SZ)"
RUN_ID="${RUN_TS}-${MODE}"
RECEIPTS_DIR=".proof/receipts"
LOG_DIR=".proof/logs/${RUN_ID}"
mkdir -p "$RECEIPTS_DIR" "$LOG_DIR"

# --- git facts (captured up front; the run itself must not dirty the tree) ---
GIT_BRANCH="$(git rev-parse --abbrev-ref HEAD)"
GIT_SHA="$(git rev-parse HEAD)"
GIT_DIRTY="false"
if [ -n "$(git status --porcelain)" ]; then GIT_DIRTY="true"; fi

RESULTS_TSV="$(mktemp)"
trap 'rm -f "$RESULTS_TSV"' EXIT

START_EPOCH="$(date +%s)"

# run_check <name> <tool> <command...>
# Runs the command, captures output, prints a PASS/FAIL line, appends a TSV row.
# <tool> is the binary the check actually needs (NOT derived from the command
# string — most checks start with `cd`, which is always on PATH). If that tool
# is missing, the check is SKIP(tool not on PATH), never a silent pass.
run_check() {
  local name="$1" tool="$2"; shift 2
  local cmd_str="$*"
  if ! command -v "$tool" >/dev/null 2>&1; then
    printf 'SKIP  %-34s (%s not on PATH)\n' "$name" "$tool"
    printf '%s\t%s\tSKIP\t0\t0\n' "$name" "$cmd_str" >> "$RESULTS_TSV"
    return 0
  fi

  local log="$LOG_DIR/$(printf '%s' "$name" | tr ' /' '__').log"
  local t0 t1 status exit_code
  t0="$(date +%s)"
  set +e
  bash -c "$cmd_str" >"$log" 2>&1
  exit_code=$?
  set -e
  t1="$(date +%s)"
  status=$((t1 - t0))

  if [ "$exit_code" -eq 0 ]; then
    printf 'PASS  %-34s (%ss)\n' "$name" "$status"
    printf '%s\t%s\tPASS\t%s\t%s\n' "$name" "$cmd_str" "$status" "$exit_code" >> "$RESULTS_TSV"
  else
    printf 'FAIL  %-34s (%ss, exit %s)\n' "$name" "$status" "$exit_code"
    printf '      expected: exit 0\n      observed: exit %s — output tail:\n' "$exit_code"
    tail -n 25 "$log" | sed 's/^/        /'
    printf '%s\t%s\tFAIL\t%s\t%s\n' "$name" "$cmd_str" "$status" "$exit_code" >> "$RESULTS_TSV"
  fi
  return 0
}

# skip_check <name> <cmd> <reason> — for env-dependent checks that cannot run here.
skip_check() {
  local name="$1" cmd="$2" reason="$3"
  printf 'SKIP  %-34s (%s)\n' "$name" "$reason"
  printf '%s\t%s\tSKIP\t0\t0\n' "$name" "$cmd" >> "$RESULTS_TSV"
  return 0
}

echo "proof harness — mode: $MODE — branch: $GIT_BRANCH @ ${GIT_SHA:0:9} (dirty=$GIT_DIRTY)"
echo "----------------------------------------------------------------"

# --- CORE tier: backend build-test job (go.yml) without -race, plus fast frontend checks ---
if [ "$MODE" != "ui" ]; then
  run_check "gofmt" "gofmt" \
    "cd backend && test -z \"\$(gofmt -l .)\" || { echo 'These files need gofmt:'; gofmt -l .; exit 1; }"
  run_check "go-build" "go" "cd backend && go build ./..."
  run_check "go-vet" "go" "cd backend && go vet ./..."
  # -count=1 for determinism (no stale-cache pass); -short is honored by
  # backend/internal/browserruntime. CI's build-test job runs the same suite
  # with -race; that costs minutes locally, so it lives in the FULL tier.
  run_check "go-test" "go" "cd backend && go test -count=1 -short -timeout=15m ./..."
  run_check "frontend-typecheck" "npm" "npm run frontend:typecheck"
  run_check "product-ui-check" "npm" "npm run product-ui:check"
fi

# --- FULL tier: race suites + golangci-lint (mirror go.yml exactly) ---
if [ "$MODE" = "full" ]; then
  run_check "go-test-race" "go" "cd backend && go test -race -count=1 -timeout=20m ./..."
  run_check "golangci-lint" "go" \
    "cd backend && go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run --path-mode=abs"
  # Env-dependent: the e2e gate needs Docker; report SKIP, never silently pass.
  if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    skip_check "docker-dependent-ci" "n/a" \
      "Docker available but no local mapping for .github/workflows/e2e-gate-tests.yml — run npx @redwoodjs/agent-ci run --all for full CI parity"
  else
    skip_check "docker-dependent-ci" "n/a" "Docker unavailable; e2e gate jobs not runnable locally"
  fi
fi

# --- UI tier: behavioral proof — Playwright navigates the real renderer (CI's renderer-smoke slice) ---
if [ "$MODE" = "ui" ]; then
  # Dedicated port + CI=true: CI disables reuseExistingServer, so if anything
  # already listens on the port the suite fails loudly instead of silently
  # testing ANOTHER worktree's dev:web renderer (see frontend/playwright.config.ts).
  run_check "e2e-typecheck" "npm" "cd frontend && npm run typecheck:e2e"
  run_check "renderer-nav" "npx" \
    "cd frontend && CI=true AO_E2E_PORT=5199 npx playwright test --grep \"@T0|@P0\" --reporter=line"
fi

END_EPOCH="$(date +%s)"
DURATION=$((END_EPOCH - START_EPOCH))

# --- write receipt JSON ---
RECEIPT="$RECEIPTS_DIR/${RUN_TS}-${MODE}.json"
python3 - "$RESULTS_TSV" "$RECEIPT" ".proof/latest.json" "$RUN_ID" "$MODE" \
  "$START_EPOCH" "$END_EPOCH" "$GIT_BRANCH" "$GIT_SHA" "$GIT_DIRTY" <<'PYEOF'
import json, sys, datetime

tsv_path, receipt_path, latest_path, run_id, mode, start, end, branch, sha, dirty = sys.argv[1:11]

def iso(epoch):
    return datetime.datetime.fromtimestamp(int(epoch), datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

checks = []
with open(tsv_path) as f:
    for line in f:
        parts = line.rstrip("\n").split("\t")
        if len(parts) < 5:
            continue
        name, cmd, status, d, exit_code = parts[:5]
        checks.append({
            "name": name,
            "cmd": cmd,
            "status": status,
            "exit": int(exit_code) if exit_code.lstrip("-").isdigit() else 0,
            "duration_s": int(d) if d.isdigit() else 0,
        })

failed = sum(1 for c in checks if c["status"] == "FAIL")
receipt = {
    "run_id": run_id,
    "mode": mode,
    "started": iso(start),
    "ended": iso(end),
    "duration_s": int(end) - int(start),
    "exit": 1 if failed else 0,
    "git": {"branch": branch, "sha": sha, "dirty": dirty == "true"},
    "checks": checks,
}
with open(receipt_path, "w") as f:
    json.dump(receipt, f, indent=2)
    f.write("\n")
with open(latest_path, "w") as f:
    json.dump(receipt, f, indent=2)
    f.write("\n")
PYEOF

# --- final summary line ---
PASSED=$(awk -F'\t' '$3=="PASS"' "$RESULTS_TSV" | wc -l | tr -d ' ')
FAILED=$(awk -F'\t' '$3=="FAIL"' "$RESULTS_TSV" | wc -l | tr -d ' ')
SKIPPED=$(awk -F'\t' '$3=="SKIP"' "$RESULTS_TSV" | wc -l | tr -d ' ')

echo "----------------------------------------------------------------"
EXIT_CODE=0
[ "$FAILED" -gt 0 ] && EXIT_CODE=1
echo "proof $MODE: ${PASSED} passed, ${FAILED} failed, ${SKIPPED} skipped — receipt $RECEIPT"
exit "$EXIT_CODE"
