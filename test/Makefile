# Test pyramid entry points for the tentacron service.
#
#   make -C test test           # vet + full suite incl. golden E2E (seconds)
#   make -C test test-race      # race detector over everything
#   make -C test e2e            # golden end-to-end corpus only, verbose
#   make -C test golden-update  # accept an INTENDED behavior change
#   make -C test fuzz           # every fuzz target for FUZZTIME each
#   make -C test stress         # race detector, shuffled, repeated STRESS_COUNT times
#
# The root Makefile aliases e2e, golden-update, fuzz, stress and live.

GO           ?= go
ROOT         := ..
FUZZTIME     ?= 20s
STRESS_COUNT ?= 5
FUZZ_PKGS    := ./internal/resolver ./internal/upstream

.DEFAULT_GOAL := test
.PHONY: test test-race e2e golden-update fuzz stress live help

test: ## Vet + the full host suite (unit, integration, golden E2E)
	cd $(ROOT) && $(GO) vet ./... && $(GO) test ./...

test-race: ## Race-detector pass with shuffled order
	cd $(ROOT) && $(GO) test -race -shuffle=on ./...

e2e: ## Golden end-to-end corpus only, verbose
	cd $(ROOT) && $(GO) test -v -count=1 ./test/e2e

golden-update: ## Regenerate the golden files after an intended behavior change
	cd $(ROOT) && $(GO) test ./test/e2e -update -count=1
	cd $(ROOT) && $(GO) test ./test/e2e -count=1

fuzz: ## Run every Fuzz* target for FUZZTIME (default 20s) each; seeds also run under plain go test
	cd $(ROOT) && for pkg in $(FUZZ_PKGS); do \
		for f in $$($(GO) test -list 'Fuzz.*' $$pkg | grep '^Fuzz'); do \
			echo "== $$pkg $$f"; \
			$(GO) test $$pkg -run '^$$' -fuzz "^$$f$$" -fuzztime $(FUZZTIME) || exit 1; \
		done; \
	done

stress: ## Race detector, shuffled order, repeated STRESS_COUNT times (default 5) — flakiness hunt
	cd $(ROOT) && $(GO) test -race -shuffle=on -count=$(STRESS_COUNT) -timeout 30m ./...

live: ## One real request through real upstreams (TENTACRON_LIVE_CONFIG + TENTACRON_LIVE_REQUEST)
	cd $(ROOT) && $(GO) test -v -count=1 -timeout 30m ./test/live

help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
