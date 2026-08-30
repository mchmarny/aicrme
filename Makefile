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
# Source file holding the AICR version constant. A THIRD copy of the same
# literal, and the one that decides which validator images AICR pulls --
# see the comment on aicrclient.AICRVersion for what a wrong value costs.
AICR_VERSION_SRC := internal/aicrclient/client.go

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
	fi; \
	constant=$$(sed -n 's/^const AICRVersion = "\(.*\)"$$/\1/p' $(AICR_VERSION_SRC)); \
	if [ -z "$$constant" ]; then \
		echo "ERROR: no AICRVersion constant in $(AICR_VERSION_SRC) — aicr.WithVersion would receive an empty version and AICR would leave its validator images on :latest"; exit 1; \
	elif [ "$$constant" != "$$expected" ]; then \
		echo "ERROR: $(AICR_VERSION_SRC) pins AICRVersion=$$constant, expected $$expected (see .settings.yaml dependencies.aicr). This is the value AICR rewrites its validator image tags with — a wrong one makes every validator Job pull an image that does not exist"; exit 1; \
	else \
		echo "check-aicr-pin: OK — AICRVersion $$constant matches pin"; \
	fi

.PHONY: demo
demo: ## Stands up a local browser-usable demo cluster and leaves it running
	./scripts/demo.sh up

.PHONY: demo-down
demo-down: ## Deletes the local demo cluster
	./scripts/demo.sh down

.PHONY: demo-status
demo-status: ## Shows whether the local demo is running, and the console's URL
	./scripts/demo.sh status

.PHONY: check-tools
check-tools: ## Warns when a local lint tool has drifted from its .settings.yaml pin
	@# Warns, never fails: tooling here is Homebrew-managed and upgrades on its
	@# own schedule, so drift is normal and blocking on it would just be noise.
	@# What is NOT acceptable is finding out from a red CI run on an unchanged
	@# file, which is what happened on 2026-08-19 when an unpinned shellcheck
	@# flagged SC2015 in CI and not locally.
	@#
	@# Deliberately covers lint tools only. helm and kubectl are no longer
	@# pinned anywhere: the image that used to ship them is gone, and the
	@# binary now uses whatever the operator has. internal/console/preflight.go
	@# resolves and records those versions per run so the evidence bundle can
	@# say which helm installed a cluster -- which is the property the pins
	@# used to provide, moved from build time to run time.
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
	shellcheck -x -P test/e2e -P test/hardware -P scripts test/e2e/*.sh test/hardware/*.sh scripts/*.sh

.PHONY: test-install
# The installer's platform mapping has to agree with .goreleaser.yaml's
# archive names. Nothing else compares them, and a mismatch only shows up
# after a release, as a 404 for whoever ran the install one-liner.
test-install: ## Tests scripts/install.sh's platform mapping
	./scripts/install_test.sh

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

.PHONY: qualify
qualify: web lint lint-shell test-install test-web test-coverage check-aicr-pin ## Full local gate — must match CI exactly
# web comes first: internal/web/dist holds only .gitkeep on a clean
# checkout, and go test ./internal/web/... (part of test-coverage) needs the
# real embedded index.html, not just a directory that satisfies go:embed.
# CI gets this for free from its own step ordering (lint, then make web,
# then make test-coverage); qualify has to build it in for the same
# guarantee to hold locally, on a pristine tree, without a prior `make web`.
