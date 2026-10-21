# Minimal Makefile — proof harness entry points.
# See scripts/proof.sh and the "Proof harness" section in AGENTS.md.

.PHONY: help proof proof-full proof-ui

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-12s %s\n", $$1, $$2}'

proof: ## Fast green/red proof of this repo state (core tier, ~3 min)
	bash scripts/proof.sh

proof-full: ## Full proof incl. race suites + golangci-lint (slow)
	bash scripts/proof.sh --full

proof-ui: ## Behavioral proof: Playwright navigates the real renderer (T0/P0 specs)
	bash scripts/proof.sh --ui $(ARGS)

# Record traces of just the specs your diff touches, e.g.:
#   make proof-ui-trace ARGS="e2e/chat-retry.spec.ts"
proof-ui-trace: ## proof-ui plus a --trace on pass of ARGS specs (visual recording)
	bash scripts/proof.sh --ui $(ARGS)
