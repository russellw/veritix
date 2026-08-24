BINARY      := veritix
PKG         := github.com/russellw/veritix
BUILD_DIR   := bin
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(PKG)/internal/buildinfo.Version=$(VERSION) \
	-X $(PKG)/internal/buildinfo.Commit=$(COMMIT) \
	-X $(PKG)/internal/buildinfo.Date=$(DATE)

# DuckDB is a C++ library, so CGO is required. The prebuilt static libraries
# ship with the Go module, so no system DuckDB install is needed.
export CGO_ENABLED := 1

# The web interface lives in web/ and is built by Vite into web/dist, which
# web/embed.go embeds. Node is a build-time requirement only: the shipped binary
# has no Node in it and needs none to run. pnpm is pinned through corepack, so
# every machine and every CI run uses the same integrity-checked package
# manager. See docs/frontend-stack.md.
WEB  := web
PNPM := corepack pnpm

# The product page. Static files served by Caddy on the belunaro box; it is not
# part of any binary and nothing builds it. See site/README.md.
SITE := site

.PHONY: all
all: lint test build

# build embeds whatever is currently in web/dist — the committed placeholder on
# a clean checkout, which produces a working API and a binary that says the
# interface is missing. `make release` is the one that ships an interface.
.PHONY: build
build:
	@mkdir -p $(BUILD_DIR)
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BUILD_DIR)/$(BINARY) ./cmd/veritix

.PHONY: release
release: web build

.PHONY: install
install:
	go install -trimpath -ldflags '$(LDFLAGS)' ./cmd/veritix

.PHONY: test
test:
	go test ./...

# The race detector matters most for the engine and the agent loop, both of
# which fan out across goroutines.
.PHONY: test-race
test-race:
	go test -race ./...

# Score the deterministic auditor against the fixture's own defect manifest.
# It is a make target rather than a script because it takes a second and needs
# no model: with llm.provider unset this measures the checks, which is a thing
# CI can do on every commit. Scoring a model is the same command with --llm and
# --runs, and that is minutes to hours — see docs/eval.md.
.PHONY: eval
eval: build
	./$(BUILD_DIR)/$(BINARY) eval testdata/dirty-retail --log-level warn

.PHONY: cover
cover:
	go test -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: lint
lint:
	go vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed; ran go vet only"; \
	fi

.PHONY: fmt
fmt:
	go fmt ./...
	@command -v gofumpt >/dev/null 2>&1 && gofumpt -l -w . || true

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: docker
docker:
	docker build -t veritix:$(VERSION) -f deploy/Dockerfile .

# The build asserts what it can about the binary; distroless has no shell, so
# everything about the image that ships has to be asserted by running it. The
# embedded zone database is the one that cannot be checked anywhere else at
# all — every other machine has a system zoneinfo that answers first.
.PHONY: docker-smoke
docker-smoke: docker
	IMAGE=veritix:$(VERSION) scripts/smoke-image.sh

# ── web interface ──────────────────────────────────────────────────────────

# The lockfile is the source of truth, the way go.sum is. An install that would
# have to change it fails instead.
.PHONY: web-install
web-install:
	cd $(WEB) && $(PNPM) install --frozen-lockfile

.PHONY: web
web: web-install
	cd $(WEB) && $(PNPM) build
	@# Vite's emptyOutDir wipes the placeholder that keeps //go:embed compiling
	@# on a checkout with no build in it.
	@touch $(WEB)/dist/.gitkeep

.PHONY: web-dev
web-dev: web-install
	cd $(WEB) && $(PNPM) dev

.PHONY: web-check
web-check: web-install
	cd $(WEB) && $(PNPM) typecheck
	cd $(WEB) && $(PNPM) audit --audit-level=high

# ── supply chain ───────────────────────────────────────────────────────────

# The Go modules are deliberately not vendored: the tree is 728 MB, 572 MB of it
# opaque prebuilt DuckDB libraries whose diffs nobody can review. These two are
# what stands in for it — go.sum and the checksum transparency log for
# integrity, govulncheck for known holes. See docs/frontend-stack.md §6.
.PHONY: audit
audit: web-check
	go mod verify
	@command -v govulncheck >/dev/null 2>&1 \
		&& govulncheck ./... \
		|| echo "govulncheck not installed; install: go install golang.org/x/vuln/cmd/govulncheck@latest"

# ── browser tests ──────────────────────────────────────────────────────────

# e2e/ is its own pnpm workspace with its own lockfile, so that Playwright and
# its browser download never enter the shipped interface's dependency tree.
E2E := e2e

.PHONY: e2e-install
e2e-install:
	cd $(E2E) && $(PNPM) install --frozen-lockfile
	@# Playwright's install script is blocked like every other one, so the
	@# browser download is run here, deliberately and visibly, instead.
	cd $(E2E) && $(PNPM) install-browser

# Builds, serves on a throwaway data directory, runs the tests, tears it down.
.PHONY: e2e
e2e: e2e-install
	./$(E2E)/run-local.sh

# Against a server that is already running, for a quicker loop.
.PHONY: e2e-test
e2e-test:
	cd $(E2E) && $(PNPM) test

# The product page at veritix.belunaro.com. Static files, no build step, and
# nothing but HTML on that host — site/README.md says why that matters and how
# the vhost was set up the first time. Caddy serves whatever is on disk, so
# there is nothing to restart.
.PHONY: site-deploy
site-deploy:
	tar cz -C $(SITE) --exclude=README.md --exclude=veritix.Caddyfile . | ssh vps 'cat > /tmp/veritix-site.tgz'
	ssh vps 'sudo mkdir -p /srv/www/veritix && sudo tar xz -C /srv/www/veritix -f /tmp/veritix-site.tgz && \
	         sudo chown -R root:root /srv/www/veritix && rm /tmp/veritix-site.tgz'
	curl -fsSI https://veritix.belunaro.com/ | head -1

.PHONY: clean
clean:
	rm -rf $(BUILD_DIR) coverage.out $(WEB)/dist/assets $(WEB)/dist/index.html
