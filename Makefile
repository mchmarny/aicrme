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
# No --timeout flag on purpose: .golangci.yaml's run.timeout is the single
# source, and CI runs golangci-lint-action, which reads that file and takes no
# flag from this target. A flag here would silently override the config locally
# and let the two diverge.
lint: ## Lints Go sources
	golangci-lint run ./...

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
	if awk -v c="$$coverage" -v t="$(COVERAGE_THRESHOLD)" 'BEGIN{exit !(c<t)}'; then \
		echo "ERROR: coverage $$coverage% below threshold $(COVERAGE_THRESHOLD)%"; exit 1; \
	fi

# Source file holding the snapshot agent image constant. The image tag is a
# second copy of the AICR version literal that go.mod and .settings.yaml also
# carry, and neither Go's module graph nor yq can see it — check-aicr-pin
# below greps it so all three stay equal or the build fails.
AICR_IMAGE_SRC := internal/console/console.go

.PHONY: check-aicr-pin
check-aicr-pin: ## Verifies go.mod and the snapshot agent image both pin github.com/NVIDIA/aicr to the version in .settings.yaml
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
	fi; \
	image=$$(sed -n 's/^const defaultSnapshotAgentImage = "\(.*\)"$$/\1/p' $(AICR_IMAGE_SRC)); \
	if [ -z "$$image" ]; then \
		echo "ERROR: no non-empty defaultSnapshotAgentImage constant in $(AICR_IMAGE_SRC) — Discover would deploy the snapshot agent with an empty image, which the API server rejects"; exit 1; \
	fi; \
	tag=$${image##*:}; \
	if [ "$$tag" = "$$image" ]; then \
		echo "ERROR: $(AICR_IMAGE_SRC) pins $$image with no tag, expected :$$expected (see .settings.yaml dependencies.aicr)"; exit 1; \
	elif [ "$$tag" != "$$expected" ]; then \
		echo "ERROR: $(AICR_IMAGE_SRC) pins snapshot agent image $$image, expected tag $$expected (see .settings.yaml dependencies.aicr)"; exit 1; \
	else \
		echo "check-aicr-pin: OK — snapshot agent image $$image matches pin"; \
	fi

.PHONY: demo
demo: ## Stands up a local browser-usable demo cluster and leaves it running
	./scripts/demo.sh up

.PHONY: demo-down
demo-down: ## Deletes the local demo cluster
	./scripts/demo.sh down

.PHONY: demo-status
demo-status: ## Shows whether the local demo is running, and its URL and password
	./scripts/demo.sh status

.PHONY: check-tools
check-tools: ## Warns when a local lint tool has drifted from its .settings.yaml pin
	@# Warns, never fails: tooling here is Homebrew-managed and upgrades on its
	@# own schedule, so drift is normal and blocking on it would just be noise.
	@# What is NOT acceptable is finding out from a red CI run on an unchanged
	@# file, which is what happened on 2026-08-19 when an unpinned shellcheck
	@# flagged SC2015 in CI and not locally.
	@#
	@# Deliberately covers lint tools only. helm and kubectl are pinned in the
	@# Dockerfile because they ship INSIDE the console image and deploy.sh runs
	@# against them -- those pins are the product's contract, and matching them
	@# to a developer's machine is the mistake that produced the dry-run ceiling
	@# confusion (docs/phase-2-handoff.md).
	@want=$$(sed -n 's/^  shellcheck: *.\(v[0-9.]*\).*/\1/p' .settings.yaml); \
	have=v$$(shellcheck --version 2>/dev/null | awk '/^version:/{print $$2}'); \
	if [ -n "$$have" ] && [ "$$want" != "$$have" ]; then \
		echo "WARNING: shellcheck local $$have != pinned $$want (CI uses $$want)"; \
	fi
	@want=$$(sed -n "s/^  golangci_lint: *.\(v[0-9.]*\).*/\1/p" .settings.yaml); \
	have=v$$(golangci-lint --version 2>/dev/null | awk '{print $$4}'); \
	if [ -n "$$have" ] && [ "$$want" != "$$have" ]; then \
		echo "WARNING: golangci-lint local $$have != pinned $$want (CI uses $$want)"; \
	fi

.PHONY: lint-shell
lint-shell: check-tools ## Lints shell scripts with shellcheck
	shellcheck -x -P test/e2e -P test/chart -P test/hardware -P scripts test/e2e/*.sh test/chart/*.sh test/hardware/*.sh scripts/*.sh

.PHONY: test-chart
test-chart: ## Runs helm lint plus the chart contract assertions (offline, no cluster)
	./test/chart/contract.sh

.PHONY: test-e2e-apply
test-e2e-apply: ## Runs the Discover-to-Apply dry-run e2e on Kind+KWOK (needs Docker)
	./test/e2e/apply-dryrun.sh

.PHONY: test-web
test-web: ## Runs the SPA unit tests
	cd web && npm test

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
	@# PLATFORM is empty by default, which builds for the host and is what Kind
	@# wants: an arm64 laptop running an arm64 Kind node needs an arm64 image,
	@# and forcing amd64 there would emulate for no reason. It is set only when
	@# the target cluster's architecture differs from the host's --
	@# scripts/demo-remote.sh reads kubernetes.io/arch off the nodes and passes
	@# it, because a managed cluster is almost always amd64 while the laptop
	@# building the image increasingly is not, and the failure that mismatch
	@# produces (CrashLoopBackOff with "exec format error") says nothing about
	@# its own cause.
	docker build $(if $(PLATFORM),--platform $(PLATFORM),) --build-arg GO_VERSION=$(GO_VERSION) -t $(IMAGE) .

.PHONY: qualify
qualify: web lint lint-shell test-chart test-web test-coverage check-aicr-pin ## Full local gate — must match CI exactly
# web comes first: internal/web/dist holds only .gitkeep on a clean
# checkout, and go test ./internal/web/... (part of test-coverage) needs the
# real embedded index.html, not just a directory that satisfies go:embed.
# CI gets this for free from its own step ordering (lint, then make web,
# then make test-coverage); qualify has to build it in for the same
# guarantee to hold locally, on a pristine tree, without a prior `make web`.
