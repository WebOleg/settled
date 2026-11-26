SHELL := /bin/bash
.DEFAULT_GOAL := help

.PHONY: help up down status reset obs-up obs-check obs-down check check-docs check-shell check-secrets check-go l1-up l1-down l1-logs l1-check l1-blob-check l2-keys l2-fund l2-deploy l2-verify l2-up l2-check l2-games l2-deposit l2-withdraw l2-logs l2-down l2-reset

L1 := docker compose -f infra/devnet/l1/compose.yaml
L2 := docker compose -f infra/devnet/l2/compose.yaml
OBS := docker compose -f infra/observability/compose.yaml
PROM_IMAGE := prom/prometheus:v3.16.0@sha256:e04987641b57704f408e31f6373ba4138ecf27cb4a165d24aabb6c38fddd0f06
STEP := $(MAKE) --no-print-directory

help: ## List the targets
	@awk 'BEGIN { FS = ":.*## " } /^[a-z0-9-]+:.*## / { printf "  %-14s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

up: ## Start the whole devnet from any state, in order, and check each part
	@$(STEP) l1-up
	@$(STEP) l1-check
	@$(STEP) l2-keys
	@$(STEP) l2-fund
	@$(STEP) l2-deploy
	@$(STEP) l2-verify
	@$(STEP) l2-up
	@$(STEP) l2-check
	@$(STEP) obs-up
	@$(STEP) obs-check
	@echo "up: ok"

down: ## Stop observability, the L2, then the L1; keep all data
	@$(OBS) down
	@$(L2) down
	@$(L1) down

status: ## One look at services, heads and operator balances, without waiting
	@scripts/devnet/status.sh

reset: ## Stop everything and delete chain data, metrics and the deployment; role keys are kept
	@$(OBS) down --volumes
	@$(L2) down
	@$(L1) down
	@scripts/devnet/l2-init.sh --reset
	@scripts/devnet/l2-deploy.sh --reset
	@scripts/devnet/l1-init.sh --reset

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

l1-down: ## Stop the local L1 and keep its data; stops what uses its network first
	@$(OBS) down
	@$(L2) down
	@$(L1) down

l2-keys: ## Create the missing L2 operator keys (one per role) and list role addresses
	@go run ./cmd/l2-keys

l2-fund: ## Top up the L2 operator accounts that pay gas on the local L1
	@scripts/devnet/l2-fund.sh

l2-deploy: ## Deploy the L2 contracts to the local L1 with op-deployer (once)
	@scripts/devnet/l2-deploy.sh

l2-verify: ## Check the deployed L2 contracts on the L1: owners, operators, parameters
	@go run ./cmd/l2-verify

l2-up: ## Start the L2 sequencer, batcher and verifier; needs l1-up and l2-deploy
	@scripts/devnet/l2-init.sh
	@$(L2) up -d --quiet-pull

l2-check: ## Show that the L2 advances, its batches reach the L1 as blobs, and the verifier agrees
	@scripts/devnet/l2-check.sh

l2-games: ## Check the proposer's dispute games against roots recomputed from the verifier
	@go run ./cmd/l2-games --wait 15m

l2-deposit: ## Deposit 1 ETH from the L1 to the L2 and account for every wei
	@scripts/devnet/l2-deposit.sh

l2-withdraw: ## Withdraw 0.1 ETH from the L2 to the L1: start, prove, finalize, every wei checked
	@scripts/devnet/l2-withdraw.sh

l2-logs: ## Follow the logs of the L2 services
	@$(L2) logs --follow --tail 50

l2-down: ## Stop the L2 services and keep their data; stops observability first
	@$(OBS) down
	@$(L2) down

l2-reset: ## Stop the L2 services and delete the chain data (keys and deployment kept)
	@$(OBS) down
	@$(L2) down
	@scripts/devnet/l2-init.sh --reset

obs-up: ## Start Prometheus, scraping every client of the L1 and the L2; needs l1-up and l2-up
	@docker run --rm --entrypoint promtool -v "$(CURDIR)/infra/observability:/c:ro" $(PROM_IMAGE) check config /c/prometheus.yml
	@$(OBS) up -d --quiet-pull

obs-check: ## Show that Prometheus scrapes all nine clients and each reports series
	@scripts/observability/obs-check.sh

obs-down: ## Stop Prometheus and keep its data
	@$(OBS) down
