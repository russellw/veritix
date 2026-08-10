BINARY      := veritix
PKG         := github.com/russellwallace/veritix
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

.PHONY: all
all: lint test build

.PHONY: build
build:
	@mkdir -p $(BUILD_DIR)
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BUILD_DIR)/$(BINARY) ./cmd/veritix

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

.PHONY: cover
cover:
	go test -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: lint
lint:
	go vet ./...
	@command -v golangci-lint >/dev/null 2>&1 \
		&& golangci-lint run ./... \
		|| echo "golangci-lint not installed; ran go vet only"

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

.PHONY: clean
clean:
	rm -rf $(BUILD_DIR) coverage.out
