GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test lint vet fmt snapshot e2e

build:
	$(GO) build -trimpath -ldflags "-s -w -X github.com/iugu-private/platform2-cli/internal/cli.Version=$(VERSION)" -o bin/iugu ./cmd/iugu

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -l . && test -z "$$(gofmt -l .)"

lint: vet
	@command -v staticcheck >/dev/null && staticcheck ./... || echo "staticcheck not installed (go install honnef.co/go/tools/cmd/staticcheck@latest)"

snapshot:
	goreleaser release --snapshot --clean --skip=publish,sign
