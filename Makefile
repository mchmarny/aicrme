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
	@case "$(COVERAGE_THRESHOLD)" in \
		''|*[!0-9.]*) \
			echo "ERROR: COVERAGE_THRESHOLD ('$(COVERAGE_THRESHOLD)') is not a valid number — check quality.coverage_threshold in .settings.yaml (is yq installed?)"; \
			exit 1 ;; \
	esac
	@coverage=$$(go tool cover -func=coverage.out | grep total: | awk '{print substr($$3, 1, length($$3)-1)}'); \
	echo "Coverage: $$coverage% (threshold: $(COVERAGE_THRESHOLD)%)"; \
	if [ $$(echo "$$coverage < $(COVERAGE_THRESHOLD)" | bc) -eq 1 ]; then \
		echo "ERROR: coverage $$coverage% below threshold $(COVERAGE_THRESHOLD)%"; exit 1; \
	fi

.PHONY: check-aicr-pin
check-aicr-pin: ## Verifies go.mod pins github.com/NVIDIA/aicr to the version recorded in .settings.yaml
	@expected=$$(yq -r '.dependencies.aicr' .settings.yaml); \
	case "$$expected" in \
		''|null) echo "ERROR: dependencies.aicr is not set in .settings.yaml (is yq installed?)"; exit 1 ;; \
	esac; \
	actual=$$(go list -m -f '{{.Version}}' github.com/NVIDIA/aicr 2>/dev/null || true); \
	if [ -z "$$actual" ]; then \
		echo "check-aicr-pin: OK — github.com/NVIDIA/aicr not yet required by go.mod (pin: $$expected)"; \
	elif [ "$$actual" != "$$expected" ]; then \
		echo "ERROR: go.mod requires github.com/NVIDIA/aicr@$$actual, expected $$expected (see .settings.yaml dependencies.aicr)"; exit 1; \
	else \
		echo "check-aicr-pin: OK — github.com/NVIDIA/aicr@$$actual matches pin"; \
	fi

.PHONY: web
web: ## Builds the SPA into web/dist and syncs it into internal/web/dist for go:embed
	cd web && npm ci && npm run build
	rm -rf internal/web/dist
	cp -R web/dist internal/web/dist
	touch internal/web/dist/.gitkeep

.PHONY: build
build: web ## Builds the aicrme binary with the SPA embedded
	go build -ldflags "$(LDFLAGS)" -o bin/aicrme ./cmd/aicrme

IMAGE ?= aicrme:dev

.PHONY: image
image: ## Builds the container image; GO_VERSION comes from .go-version, never hardcoded
	docker build --build-arg GO_VERSION=$(GO_VERSION) -t $(IMAGE) .

.PHONY: qualify
qualify: web lint test-coverage check-aicr-pin ## Full local gate — must match CI exactly
# web comes first: internal/web/dist holds only .gitkeep on a clean
# checkout, and go test ./internal/web/... (part of test-coverage) needs the
# real embedded index.html, not just a directory that satisfies go:embed.
# CI gets this for free from its own step ordering (lint, then make web,
# then make test-coverage); qualify has to build it in for the same
# guarantee to hold locally, on a pristine tree, without a prior `make web`.
