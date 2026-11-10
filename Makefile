GO ?= go
BINARY := bin/dirspace
LINUX_AMD64 := bin/dirspace-linux-amd64
LINUX_ARM64 := bin/dirspace-linux-arm64
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 1.2.1)
LDFLAGS := -s -w -X github.com/YEDEDD/dirspace/internal/cli.Version=$(VERSION)

.PHONY: all build build-linux build-linux-arm64 test test-race vet fmt fmt-check check clean install uninstall perf

all: check build

build:
	@mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/dirspace

build-linux:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(LINUX_AMD64) ./cmd/dirspace
	@echo "Linux amd64: $(LINUX_AMD64)"

build-linux-arm64:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(LINUX_ARM64) ./cmd/dirspace
	@echo "Linux arm64: $(LINUX_ARM64)"

fmt:
	gofmt -w cmd internal

fmt-check:
	@out="$$(gofmt -l cmd internal)"; \
	if [ -n "$$out" ]; then echo "需要 gofmt:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

test:
	$(GO) test ./... -count=1

test-race:
	$(GO) test -race ./... -count=1

check: fmt-check vet test

install: build
	sudo scripts/install.sh

uninstall:
	sudo scripts/uninstall.sh

perf:
	bash tests/perf/run.sh

clean:
	rm -rf bin
