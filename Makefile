# gogofhir — build, run, and test.
# `make` (or `make help`) lists targets; `make check` is the pre-push gate.

BIN     := bin/gogofhir
GO      := go
PKGS    := ./...
FHIRVERSIONS := r4 r5

.DEFAULT_GOAL := help

.PHONY: help build release run test test-race cover bench fmt fmt-check vet \
        lint check check-parity vendor gen gen-check clean

help: ## List available targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the gogofhir binary into bin/
	$(GO) build -o $(BIN) ./cmd/gogofhir

release: ## Build a stripped, trimmed release binary
	$(GO) build -trimpath -ldflags '-s -w' -o $(BIN) ./cmd/gogofhir

run: build ## Summarize the embedded conformance index (the REST server lands in M2)
	./$(BIN) conformance

vendor: ## Fetch the FHIR conformance packages pinned in third_party/packages.lock
	$(GO) run ./tools/vendorpkg -lock third_party/packages.lock -dest third_party/packages

gen: ## Compile vendored packages into the embedded conformance indexes
	@for v in $(FHIRVERSIONS); do \
		echo "confgen $$v"; \
		$(GO) run ./tools/confgen -version $$v \
			-src third_party/packages -out internal/conformance/data || exit 1; \
	done

gen-check: gen ## Fail if the committed indexes differ from a fresh generation
	@if ! git diff --quiet -- internal/conformance/data; then \
		echo "generated conformance indexes are stale; run 'make gen' and commit"; \
		git diff --stat -- internal/conformance/data; exit 1; \
	fi

test: ## Run all tests
	$(GO) test $(PKGS)

test-race: ## Run all tests with the race detector
	$(GO) test -race $(PKGS)

check-parity: ## Run the storage and REST suites against PostgreSQL as well
	@if [ -z "$$GOGOFHIR_TEST_POSTGRES" ]; then \
		echo "GOGOFHIR_TEST_POSTGRES is not set; the PostgreSQL half of the parity gate cannot run."; \
		echo "  e.g. GOGOFHIR_TEST_POSTGRES='postgres://user@host:5432/db?sslmode=disable' make check-parity"; \
		exit 1; \
	fi
	$(GO) test ./internal/storage/... ./internal/rest/...

cover: ## Run race tests with coverage and print the summary
	@mkdir -p bin
	$(GO) test -race -coverprofile=bin/cover.out \
		$$($(GO) list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' $(PKGS))
	$(GO) tool cover -func=bin/cover.out | tail -1

bench: ## Run benchmarks
	$(GO) test -bench=. -run=NONE $(PKGS)

fmt: ## Format all Go sources
	gofmt -w .

fmt-check: ## Fail if any file needs formatting
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then \
		echo "gofmt needed on:"; echo "$$out"; exit 1; \
	fi

vet: ## Run go vet
	$(GO) vet $(PKGS)

lint: fmt-check vet ## Formatting check + vet

check: lint test-race ## The pre-push gate: lint + race tests
	@if [ -n "$$GOGOFHIR_TEST_POSTGRES" ]; then $(MAKE) check-parity; else \
		echo "note: GOGOFHIR_TEST_POSTGRES unset, so the suites ran on SQLite only (see check-parity)"; \
	fi

clean: ## Remove build artifacts and runtime state
	rm -rf bin data
