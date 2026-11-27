SHELL := /bin/bash
.DEFAULT_GOAL := help

.PHONY: help up down status reset obs-up obs-check obs-down obs-password alert-drill check check-docs check-images check-shell check-secrets check-go check-contracts check-bridge contracts-snapshot contracts-slither-record l1-up l1-down l1-logs l1-check l1-blob-check l2-keys l2-fund l2-deploy l2-verify l2-up l2-check l2-games l2-deposit l2-withdraw l2-load vault-deploy vault-verify vault-upgrade vault-timelock-move vault-payer vault-payer-rotate vault-check vault-reset bridge-up bridge-check suspense-list withdrawal-check deposit-check bridge-drill bridge-logs bridge-down bridge-reset l2-logs l2-down l2-reset

L1 := docker compose -f infra/devnet/l1/compose.yaml
L2 := docker compose -f infra/devnet/l2/compose.yaml
OBS := docker compose -f infra/observability/compose.yaml
BRIDGE := docker compose -f infra/bridge/compose.yaml
PROM_IMAGE := prom/prometheus:v3.16.0@sha256:e04987641b57704f408e31f6373ba4138ecf27cb4a165d24aabb6c38fddd0f06
# Foundry v1.10.1: the arm64 image of v1.10.0 shipped forge, cast and chisel as 434 KB
# stubs, and the lint of v1.9.0 counted every assembly block as an external call
# (contracts/README.md).
FOUNDRY_IMAGE := ghcr.io/foundry-rs/foundry:v1.10.1@sha256:477ed6a2574fcec250633915433201994a92dc7476697cda02adf684d9b42092
AM_IMAGE := prom/alertmanager:v0.35.1@sha256:4e3d029e69c7f61dd53a2b31a3dc427a2b81736cffca185c0854075e07969e36
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
	@$(STEP) bridge-up
	@$(STEP) obs-up
	@$(STEP) obs-check
	@echo "up: ok"

down: ## Stop observability, the bridge, the L2, then the L1; keep all data
	@$(OBS) down
	@$(BRIDGE) down
	@$(L2) down
	@$(L1) down

status: ## One look at services, heads and operator balances, without waiting
	@scripts/devnet/status.sh

reset: ## Stop everything and delete chain data, metrics, the ledger and the deployment; role keys are kept
	@$(OBS) down --volumes
	@$(BRIDGE) down --volumes
	@$(L2) down
	@$(L1) down
	@scripts/devnet/l2-init.sh --reset
	@scripts/contracts/vault-deploy.sh --reset
	@scripts/observability/obs-init.sh --reset
	@scripts/bridge/bridge-init.sh --reset
	@scripts/devnet/l2-deploy.sh --reset
	@scripts/devnet/l1-init.sh --reset

check: check-docs check-images check-shell check-secrets check-go check-contracts check-bridge ## Run every local check

check-docs: ## Formatting and documentation rules on every tracked file
	@scripts/check-docs.sh

check-images: ## Every container image pinned by digest; every image built here gets all its packages
	@scripts/check-images.sh

check-shell: ## shellcheck on every tracked shell script
	@files="$$(git ls-files '*.sh')"; \
	if [ -n "$$files" ]; then shellcheck $$files && echo "check-shell: ok"; \
	else echo "check-shell: no shell scripts"; fi

check-secrets: ## gitleaks on the full git history, findings redacted
	@gitleaks git --redact --no-banner --log-level warn . && echo "check-secrets: ok"

check-contracts: ## forge fmt, build and lint without findings, tests, gas, Halmos and Slither
	@FOUNDRY_IMAGE=$(FOUNDRY_IMAGE) scripts/contracts/check.sh

contracts-snapshot: ## Record the gas of the unit tests in contracts/.gas-snapshot, for review
	@mkdir -p .cache/foundry
	@docker run --rm -u "$$(id -u):$$(id -g)" -e HOME=/cache -v "$(CURDIR)/.cache/foundry:/cache" \
	  -v "$(CURDIR)/contracts:/work" -w /work --entrypoint forge $(FOUNDRY_IMAGE) snapshot --match-path 'test/unit/*'
	@# forge writes the file without a final newline, which check-docs requires of every file.
	@[ -z "$$(tail -c 1 contracts/.gas-snapshot)" ] || echo >> contracts/.gas-snapshot

contracts-slither-record: ## Record Slither's findings in contracts/slither.db.json, once each is reviewed
	@scripts/contracts/slither.sh --record

check-go: ## gofmt, go vet and go test on every Go package; the ledger's tests against a throwaway PostgreSQL
	@scripts/check-go.sh

check-bridge: ## The deposit indexer and the withdrawal payer end to end on anvil: reorgs, crashes, two payers, a fee rise, suspense
	@FOUNDRY_IMAGE=$(FOUNDRY_IMAGE) scripts/check-bridge.sh

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
	@$(BRIDGE) down
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

l2-load: ## Send 50 transfers a second to the L2 for 2 minutes; measure inclusion and safe latency
	@scripts/devnet/l2-load.sh

l2-logs: ## Follow the logs of the L2 services
	@$(L2) logs --follow --tail 50

l2-down: ## Stop the L2 services and keep their data; stops observability and the bridge first
	@$(OBS) down
	@$(BRIDGE) down
	@$(L2) down

l2-reset: ## Stop the L2 services and delete the chain data, the vault record and the ledger (keys and L1 deployment kept)
	@$(OBS) down
	@$(BRIDGE) down --volumes
	@$(L2) down
	@scripts/devnet/l2-init.sh --reset
	@scripts/contracts/vault-deploy.sh --reset

vault-deploy: ## Deploy the vault to the L2 behind a timelock and a proxy; once per L2
	@FOUNDRY_IMAGE=$(FOUNDRY_IMAGE) scripts/contracts/vault-deploy.sh

vault-verify: ## Check the vault on the L2 against the repository: code, roles, delays, configuration
	@FOUNDRY_IMAGE=$(FOUNDRY_IMAGE) scripts/contracts/build.sh
	@go run ./cmd/vault-verify

vault-upgrade: ## Upgrade the vault to the code the repository builds, through the timelock
	@FOUNDRY_IMAGE=$(FOUNDRY_IMAGE) scripts/contracts/build.sh
	@go run ./cmd/vault-upgrade

vault-timelock-move: ## Move the vault's admin to a new timelock whose canceller is a separate key, through both delays
	@FOUNDRY_IMAGE=$(FOUNDRY_IMAGE) scripts/contracts/vault-timelock-move.sh

vault-payer: ## Give the withdrawal payer its key in kms-dev with the vault's operator role, through the timelock; bridge-up and vault-deploy do it too
	@FOUNDRY_IMAGE=$(FOUNDRY_IMAGE) scripts/contracts/vault-payer.sh

vault-payer-rotate: ## Replace the payer's key: a new one in kms-dev gets the operator role, the old one loses it; the signer starts again on it
	@FOUNDRY_IMAGE=$(FOUNDRY_IMAGE) scripts/contracts/vault-payer.sh -rotate
	@$(BRIDGE) restart withdrawal-signer
	@echo "vault-payer-rotate: ok"

vault-check: ## Run the vault's rules on the L2 with real transactions and account for every wei
	@FOUNDRY_IMAGE=$(FOUNDRY_IMAGE) scripts/contracts/vault-check.sh

vault-reset: ## Forget the vault deployment and keep its keys, for a new L2
	@scripts/contracts/vault-deploy.sh --reset

bridge-up: ## Start the ledger's PostgreSQL and kms-dev, migrate the ledger, give the payer its key in kms-dev, start the indexer, signer and payer; needs l2-up
	@scripts/bridge/bridge-init.sh
	@go run ./cmd/signer-tls
	@$(BRIDGE) up -d --quiet-pull --build --wait postgres kms-dev
	@go run ./cmd/ledger-migrate -db 'postgres://admin@127.0.0.1:15432/ledger?sslmode=disable' \
		-db-password-file .devnet/bridge/postgres-admin.txt \
		-login indexer:deposit_indexer:.devnet/bridge/indexer.txt -login auditor:ledger_reader:.devnet/bridge/auditor.txt \
		-login ops_a:suspense_operator:.devnet/bridge/ops-a.txt -login ops_b:suspense_operator:.devnet/bridge/ops-b.txt \
		-login payer:withdrawal_payer:.devnet/bridge/payer.txt -login requester:withdrawal_requester:.devnet/bridge/requester.txt \
		-login signer:withdrawal_signer:.devnet/bridge/signer.txt
	@FOUNDRY_IMAGE=$(FOUNDRY_IMAGE) scripts/contracts/vault-payer.sh
	@$(BRIDGE) up -d --quiet-pull --build --remove-orphans deposit-indexer withdrawal-signer withdrawal-payer
	@echo "bridge-up: ok"

bridge-check: ## Reconcile the ledger with the chain, read independently: every final deposit posted, to the wei
	@FOUNDRY_IMAGE=$(FOUNDRY_IMAGE) scripts/contracts/build.sh
	@go run ./cmd/bridge-check

suspense-list: ## The payments in suspense not yet explained, and the proposals waiting for a second operator
	@go run ./cmd/suspense -db 'postgres://auditor@127.0.0.1:15432/ledger?sslmode=disable' -db-password-file .devnet/bridge/auditor.txt list

withdrawal-check: ## A real withdrawal through the payer: deposit, credited, requested, held, paid, settled at finality; about half an hour
	@FOUNDRY_IMAGE=$(FOUNDRY_IMAGE) scripts/bridge/withdrawal-check.sh

deposit-check: ## Deposit 0.1 ETH to the vault and follow it: held at once, credited at finality; about 15 minutes
	@FOUNDRY_IMAGE=$(FOUNDRY_IMAGE) scripts/bridge/deposit-check.sh

bridge-drill: ## Stop the ledger's PostgreSQL, see DepositIndexerStale fire and reach Alertmanager, start it, see it clear
	@scripts/bridge/bridge-drill.sh

bridge-logs: ## Follow the logs of the deposit indexer, the withdrawal signer and payer, and the ledger's PostgreSQL
	@$(BRIDGE) logs -f --tail 50

bridge-down: ## Stop the deposit indexer, the withdrawal signer and payer, and the ledger's PostgreSQL; keep the ledger
	@$(BRIDGE) down

bridge-reset: ## Stop the bridge and delete the ledger and kms-dev's keys; passwords, access keys and the payer's key are made again at the next bridge-up
	@$(BRIDGE) down --volumes
	@scripts/bridge/bridge-init.sh --reset

obs-up: ## Start Prometheus, Alertmanager, Grafana and the vault exporter; needs l1-up, l2-up and bridge-up
	@scripts/observability/obs-init.sh
	@docker run --rm --entrypoint promtool -v "$(CURDIR)/infra/observability:/c:ro" $(PROM_IMAGE) check config /c/prometheus.yml
	@docker run --rm --entrypoint amtool -v "$(CURDIR)/infra/observability:/c:ro" $(AM_IMAGE) check-config /c/alertmanager.yml
	@$(OBS) up -d --quiet-pull --remove-orphans --build
	@# Their configs are single files mounted at creation; a file replaced on disk is a new
	@# file the old container does not see, so both are recreated to read the current ones.
	@$(OBS) up -d --quiet-pull --no-deps --force-recreate prometheus alertmanager

obs-check: ## Show that all eleven targets are scraped, every panel has data and every alert rule matches its tile
	@scripts/observability/obs-check.sh

alert-drill: ## Stop the batcher, see its alerts fire and reach Alertmanager, start it, see them clear
	@scripts/observability/alert-drill.sh

obs-down: ## Stop Prometheus, Alertmanager and Grafana and keep their data
	@$(OBS) down

obs-password: ## Copy the Grafana admin password to the clipboard, without printing it
	@tr -d '[:space:]' < .devnet/observability/grafana-admin.txt | pbcopy && echo "obs-password: copied"
