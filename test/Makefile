# Test pyramid entry points for the tentacron service.
#
#   make -C test test           # vet + full suite incl. golden E2E (seconds)
#   make -C test test-race      # race detector over everything
#   make -C test e2e            # golden end-to-end corpus only, verbose
#   make -C test golden-update  # accept an INTENDED behavior change
#
# The root Makefile aliases e2e and golden-update.

GO   ?= go
ROOT := ..

.DEFAULT_GOAL := test
.PHONY: test test-race e2e golden-update help

test: ## Vet + the full host suite (unit, integration, golden E2E)
	cd $(ROOT) && $(GO) vet ./... && $(GO) test ./...

test-race: ## Race-detector pass with shuffled order
	cd $(ROOT) && $(GO) test -race -shuffle=on ./...

e2e: ## Golden end-to-end corpus only, verbose
	cd $(ROOT) && $(GO) test -v -count=1 ./test/e2e

golden-update: ## Regenerate the golden files after an intended behavior change
	cd $(ROOT) && $(GO) test ./test/e2e -update -count=1
	cd $(ROOT) && $(GO) test ./test/e2e -count=1

help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
