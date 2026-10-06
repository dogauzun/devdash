# Developer entry points (DEV-71). Each target runs the command CI runs
# (.github/workflows/ci.yml), so `make check` before a push sees what CI will.
# Plain POSIX sh recipes: works with the GNU make 3.81 that ships with macOS.

GO            ?= go
# CI pins golangci-lint v2.13.2 and goreleaser v2.18.2.
GOLANGCI_LINT ?= golangci-lint
GORELEASER    ?= goreleaser
VHS           ?= vhs
DOCKER        ?= docker
DEMO_IMAGE    ?= devdash-demo

# The release targets; the darwin files are build-tagged, so vet and lint run once per OS.
TARGETS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64
OSES    := darwin linux

.DEFAULT_GOAL := help

.PHONY: help check build install cross vet lint fmt test bench licenses licenses-check snapshot docker-gate demo demo-docker clean

help: ## List the targets
	@awk 'BEGIN { FS = ":.*## " } /^[a-z-]+:.*## / { printf "  %-15s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

check: lint vet cross test licenses-check ## The pre-push gate: lint, vet, cross-build, race tests, notices

build: ## Build ./devdash for this machine
	CGO_ENABLED=0 $(GO) build -trimpath -o devdash ./cmd/devdash

install: ## go install ./cmd/devdash
	CGO_ENABLED=0 $(GO) install -trimpath ./cmd/devdash

cross: ## Compile every package for the four release targets
	@for target in $(TARGETS); do \
		echo "== $$target"; \
		CGO_ENABLED=0 GOOS=$${target%/*} GOARCH=$${target#*/} $(GO) build -trimpath ./... || exit 1; \
	done

vet: ## go vet for darwin and linux
	@for os in $(OSES); do \
		echo "== go vet ($$os)"; \
		GOOS=$$os $(GO) vet ./... || exit 1; \
	done

lint: ## golangci-lint for darwin and linux (includes the gofmt and goimports checks)
	@for os in $(OSES); do \
		echo "== golangci-lint ($$os)"; \
		GOOS=$$os $(GOLANGCI_LINT) run || exit 1; \
	done

fmt: ## Rewrite files with gofmt and goimports
	$(GOLANGCI_LINT) fmt

test: ## go test -race, uncached; the run-devdash skill's emulator test
	$(GO) test -race -count=1 ./...
	python3 -B .claude/skills/run-devdash/drive_test.py

bench: ## Benchmarks, 20 iterations each
	$(GO) test -run '^$$' -bench . -benchtime 20x ./...

# THIRD_PARTY_LICENSES ships in the release archives (DEV-98); scripts/licenses rebuilds it from
# `go list -deps` for the four targets and the module cache.
licenses: ## Rewrite THIRD_PARTY_LICENSES from the modules the release binaries link
	$(GO) run ./scripts/licenses

licenses-check: ## Fail if THIRD_PARTY_LICENSES is not what `make licenses` writes
	$(GO) run ./scripts/licenses -check

snapshot: ## goreleaser check, a snapshot release into dist/ (nothing is published), then CI's archive check
	$(GORELEASER) check
	$(GORELEASER) release --snapshot --clean
	sh scripts/check-archives.sh

docker-gate: ## Phase 3 Docker gate (needs docker with compose v2, jq, curl)
	sh scripts/docker-gate.sh

# vhs drives Chromium, which refuses to start sandboxed as root, and scripts/demo.sh needs root.
demo: ## Re-record docs/demo.gif from demo.tape (Linux, as root; needs vhs, ttyd, ffmpeg, Chromium)
	VHS_NO_SANDBOX=true $(VHS) demo.tape

# The same recording from macOS: the image's entrypoint is vhs and it sets VHS_NO_SANDBOX itself.
# --privileged because scripts/demo.sh unshares, mounts and sets the hostname.
demo-docker: ## make demo in a Linux container (needs docker; the way to record on macOS)
	$(DOCKER) build -t $(DEMO_IMAGE) --build-arg GO_VERSION=$$(sed -n 's/^go //p' go.mod) - <scripts/demo.Dockerfile
	$(DOCKER) run --rm --privileged -v "$(CURDIR)":/src -w /src $(DEMO_IMAGE) demo.tape

clean: ## Remove ./devdash and dist/
	rm -rf devdash dist
