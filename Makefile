SHELL := /bin/bash
.DEFAULT_GOAL := help

MODULE   := $(shell go list -m)
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -s -w -X $(MODULE)/internal/version.Version=$(VERSION) -X $(MODULE)/internal/version.Commit=$(COMMIT) -X $(MODULE)/internal/version.Date=$(DATE)

.PHONY: help build test lint vet fmt e2e hooks record release clean

help: ## list targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

build: ## build ./bin/sidecar
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/sidecar ./cmd/sidecar

test: ## unit tests with race detector and coverage
	@set -euo pipefail; \
	cov=$$(mktemp "$${TMPDIR:-/tmp}/sidecar-cover.XXXXXX"); \
	trap 'rm -f "$$cov"' EXIT; \
	go test -race -shuffle=on -coverprofile="$$cov" ./...; \
	go tool cover -func="$$cov" | tail -n 1

# CI pins golangci-lint v2.15.0 (.github/workflows/ci.yml); use the same version locally.
lint: fmt vet ## gofmt + go vet + golangci-lint
	golangci-lint run ./...

vet:
	go vet ./...
	go vet -tags e2e ./test/e2e/...

fmt: ## fail if any file is not gofmt-ed
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || (echo "run gofmt -w ." && exit 1)

e2e: ## end-to-end test: builds sidecar + examples/agent, fake upstream and control plane
	go test -race -count=1 -tags e2e ./test/e2e/...

hooks: ## install the pre-commit hook (gofmt, go vet, gitleaks, refuses sidecar.yaml)
	git config core.hooksPath .githooks

record: ## record a provider contract fixture: make record PROVIDER=together
	@test -n "$(PROVIDER)" || (echo "PROVIDER is required" && exit 1)
	go run ./scripts/record -provider $(PROVIDER) -out internal/providers/testdata/$(PROVIDER)

release: ## local snapshot release (no publish)
	goreleaser release --snapshot --clean

clean:
	rm -rf bin dist
