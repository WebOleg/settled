SHELL := /bin/bash
.DEFAULT_GOAL := help

.PHONY: help check check-docs check-shell check-secrets

help: ## List the targets
	@awk 'BEGIN { FS = ":.*## " } /^[a-z-]+:.*## / { printf "  %-14s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

check: check-docs check-shell check-secrets ## Run every local check

check-docs: ## Formatting and documentation rules on every tracked file
	@scripts/check-docs.sh

check-shell: ## shellcheck on every tracked shell script
	@files="$$(git ls-files '*.sh')"; \
	if [ -n "$$files" ]; then shellcheck $$files && echo "check-shell: ok"; \
	else echo "check-shell: no shell scripts"; fi

check-secrets: ## gitleaks on the full git history, findings redacted
	@gitleaks git --redact --no-banner --log-level warn . && echo "check-secrets: ok"
