SHELL := /bin/bash
.DEFAULT_GOAL := help

.PHONY: help check check-docs check-shell check-secrets check-go l1-up l1-down l1-reset l1-logs l1-check l1-blob-check l2-keys l2-fund l2-deploy l2-verify l2-up l2-check l2-logs l2-down l2-reset

L1 := docker compose -f infra/devnet/l1/compose.yaml
L2 := docker compose -f infra/devnet/l2/compose.yaml

help: ## List the targets
	@awk 'BEGIN { FS = ":.*## " } /^[a-z0-9-]+:.*## / { printf "  %-14s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

check: check-docs check-shell check-secrets check-go ## Run every local check

check-docs: ## Formatting and documentation rules on every tracked file
	@scripts/check-docs.sh

check-shell: ## shellcheck on every tracked shell script
	@files="$$(git ls-files '*.sh')"; \
	if [ -n "$$files" ]; then shellcheck $$files && echo "check-shell: ok"; \
	else echo "check-shell: no shell scripts"; fi

check-secrets: ## gitleaks on the full git history, findings redacted
	@gitleaks git --redact --no-banner --log-level warn . && echo "check-secrets: ok"

check-go: ## gofmt, go vet and go test on every Go package
	@files="$$(gofmt -l cmd internal)"; \
	if [ -n "$$files" ]; then echo "check-go: not formatted with gofmt:"; echo "$$files"; exit 1; fi
	@go vet ./...
	@go test -count=1 ./...
	@echo "check-go: ok"

l1-up: ## Start the local L1; creates its genesis on first run
	@scripts/devnet/l1-init.sh
	@$(L1) up -d --quiet-pull

l1-check: ## Show that the local L1 answers, advances and agrees across layers
	@scripts/devnet/l1-check.sh

l1-blob-check: ## Send one blob transaction on the local L1 and wait for its inclusion
	@scripts/devnet/l1-blob-check.sh

l1-logs: ## Follow the logs of the local L1
	@$(L1) logs --follow --tail 50

l1-down: ## Stop the local L1 and keep its data
	@$(L1) down

l1-reset: ## Stop the local L1 and delete its data and genesis
	@$(L1) down
	@scripts/devnet/l1-init.sh --reset

l2-keys: ## Create the missing L2 operator keys (one per role) and list role addresses
	@go run ./cmd/l2-keys

l2-fund: ## Top up the L2 operator accounts that pay gas on the local L1
	@scripts/devnet/l2-fund.sh

l2-deploy: ## Deploy the L2 contracts to the local L1 with op-deployer (once)
	@scripts/devnet/l2-deploy.sh

l2-verify: ## Check the deployed L2 contracts on the L1: owners, operators, parameters
	@go run ./cmd/l2-verify

l2-up: ## Start the L2 (op-reth, op-node, op-batcher); needs l1-up and l2-deploy
	@scripts/devnet/l2-init.sh
	@$(L2) up -d --quiet-pull

l2-check: ## Show that the L2 advances, follows the L1, and its batches reach the L1 as blobs
	@scripts/devnet/l2-check.sh

l2-logs: ## Follow the logs of the L2 services
	@$(L2) logs --follow --tail 50

l2-down: ## Stop the L2 services and keep their data
	@$(L2) down

l2-reset: ## Stop the L2 services and delete the chain data (keys and deployment kept)
	@$(L2) down
	@scripts/devnet/l2-init.sh --reset
