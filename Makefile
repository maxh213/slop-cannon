# Slop Cannon build targets. Run `make help` for a summary.

SHELL := /bin/sh
MODULE := slop-cannon
BIN_DIR := bin
BINARY := $(BIN_DIR)/sc
GOLANGCI_LINT_VERSION := v2.13.2
GOLANGCI_LINT := $(BIN_DIR)/golangci-lint

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X $(MODULE)/internal/cli.Version=$(VERSION) \
	-X $(MODULE)/internal/cli.Commit=$(COMMIT) \
	-X $(MODULE)/internal/cli.Date=$(DATE)

.PHONY: help build test lint fmt check install integration clean tools update-golden update-golden-tui check-golden docs-cli cartridge-conformance demo

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*## "}; {printf "  %-18s %s\n", $$1, $$2}'

build: ## Build bin/sc with version metadata
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/sc

test: ## Run unit tests with the race detector
	go test -race ./...

lint: $(GOLANGCI_LINT) ## Run golangci-lint (installed to bin/ on first use)
	$(GOLANGCI_LINT) run ./...

fmt: ## Fail if any Go file is not gofmt-clean
	@out="$$(gofmt -l -s .)"; if [ -n "$$out" ]; then echo "gofmt: files need formatting:"; echo "$$out"; exit 1; fi

check: fmt lint test ## fmt + lint + test (the CI gate)

install: ## go install ./cmd/sc with version metadata
	go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/sc

integration: ## Run tests tagged `integration` (need real bd/git/gh)
	go test -race -tags integration ./...

demo: ## Run the deterministic demo: the TUI against the fixture with a mock scenario (P8-07)
	@scripts/demo/demo.sh $(DEMO_ARGS)

update-golden: ## Regenerate testdata/**/*.golden files (and docs/cli.md) from current output
	UPDATE_GOLDEN=1 go test -count=1 ./...

update-golden-tui: ## Regenerate only the TUI frames in testdata/tui/*.golden (P7-09)
	UPDATE_GOLDEN=1 go test -count=1 ./internal/tui/...

check-golden: ## Fail if any golden file or generated doc is stale (regenerates in place; run it on a clean tree)
	@UPDATE_GOLDEN=1 go test -count=1 ./... >/dev/null
	@stale="$$(git status --porcelain -- '*.golden' '*.golden.*' '*.expected' docs/cli.md docs/architecture.md)"; \
	if [ -n "$$stale" ]; then \
		echo "golden files are out of date; run 'make update-golden' and commit the result:"; \
		echo "$$stale"; \
		exit 1; \
	fi

docs-cli: ## Regenerate docs/cli.md from the sc command tree
	go run ./cmd/gendocs -o docs/cli.md

CARTRIDGE ?= mock

cartridge-conformance: ## Run conformance (CARTRIDGE=mock|claude|cursor|agy|codex|kimi|grok|junie|hermes|kilo|dandelion; real CLIs need logins)
	CARTRIDGE=$(CARTRIDGE) go test -count=1 -race -run '^TestConformance$$' -v ./internal/cartridge/conformance/

clean: ## Remove build output and caches produced by this Makefile
	rm -rf $(BIN_DIR) dist coverage.out

tools: $(GOLANGCI_LINT) ## Install pinned developer tools into bin/

$(GOLANGCI_LINT):
	@mkdir -p $(BIN_DIR)
	GOBIN=$(CURDIR)/$(BIN_DIR) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
