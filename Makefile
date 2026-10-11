.DEFAULT_GOAL := build

GO ?= go
CGO_ENABLED = 1
export CGO_ENABLED
BINARY ?= bin/stupidict
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || printf dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || printf unknown)
BUILD_DATE ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS ?=
ARGS ?=

.PHONY: build run test vet race

build:
	mkdir -p "$(dir $(BINARY))"
	$(GO) build -trimpath -ldflags "$(LDFLAGS) -X 'main.version=$(VERSION)' -X 'main.commit=$(COMMIT)' -X 'main.buildDate=$(BUILD_DATE)'" -o "$(BINARY)" ./cmd/stupidict

run: build
	"$(abspath $(BINARY))" $(ARGS)

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

race:
	CGO_ENABLED=1 $(GO) test -race ./...