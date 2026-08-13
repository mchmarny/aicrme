GO_VERSION          := $(shell cat .go-version)
COVERAGE_THRESHOLD  ?= $(shell yq -r '.quality.coverage_threshold' .settings.yaml)
VERSION             ?= $(shell git describe --tags --always --dirty)
COMMIT              ?= $(shell git rev-parse --short HEAD)
LDFLAGS             := -X github.com/mchmarny/aicrme/internal/version.Version=$(VERSION) \
                       -X github.com/mchmarny/aicrme/internal/version.Commit=$(COMMIT)

.PHONY: all
all: help

.PHONY: help
help: ## Prints available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-22s\033[0m %s\n", $$1, $$2}'

.PHONY: tidy
tidy: ## Formats code and tidies modules
	go fmt ./...
	go mod tidy

.PHONY: lint
lint: ## Lints Go sources
	golangci-lint run --timeout 5m ./...

.PHONY: test
test: ## Runs unit tests with race detector and coverage
	go test -race -covermode=atomic -coverprofile=coverage.out ./...

.PHONY: test-coverage
test-coverage: test ## Runs tests and enforces the coverage floor
	@if [ -z "$(COVERAGE_THRESHOLD)" ]; then \
		echo "ERROR: COVERAGE_THRESHOLD is empty — could not read quality.coverage_threshold from .settings.yaml (is yq installed?)"; exit 1; \
	fi
	@coverage=$$(go tool cover -func=coverage.out | grep total: | awk '{print substr($$3, 1, length($$3)-1)}'); \
	echo "Coverage: $$coverage% (threshold: $(COVERAGE_THRESHOLD)%)"; \
	if [ $$(echo "$$coverage < $(COVERAGE_THRESHOLD)" | bc) -eq 1 ]; then \
		echo "ERROR: coverage $$coverage% below threshold $(COVERAGE_THRESHOLD)%"; exit 1; \
	fi

.PHONY: web
web: ## Builds the SPA into web/dist
	cd web && npm ci && npm run build

.PHONY: build
build: web ## Builds the aicrme binary with the SPA embedded
	go build -ldflags "$(LDFLAGS)" -o bin/aicrme ./cmd/aicrme

.PHONY: qualify
qualify: lint test-coverage ## Full local gate — must match CI exactly
