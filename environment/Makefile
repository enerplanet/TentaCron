# Containerized environment lifecycle for the tentacron service: one Docker
# image carrying the Go toolchain, make and the sqlite3 CLI.
#
#   make -C environment build   # build the image
#   make -C environment test    # run the Go suite inside the container
#   make -C environment run     # serve the API
#   make -C environment shell   # interactive shell in the image
#
# ENV=dev (default) or prod selects .env.$(ENV).

ENV      ?= dev
ENV_FILE := .env.$(ENV)
# Prefer compose v2 (docker compose); fall back to the legacy v1 binary.
COMPOSE_BIN := $(shell docker compose version >/dev/null 2>&1 && echo "docker compose" || echo "docker-compose")
COMPOSE  := $(COMPOSE_BIN) --env-file $(ENV_FILE) -f docker-compose.yml

.DEFAULT_GOAL := help
.PHONY: build test run shell help

build: ## Build the environment image (Go toolchain + tooling)
	$(COMPOSE) build

test: ## Build and run the Go test suite inside the container
	$(COMPOSE) run --rm test

run: ## Run the API in the container
	$(COMPOSE) up api

shell: ## Interactive shell in the environment image
	$(COMPOSE) run --rm shell

help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
