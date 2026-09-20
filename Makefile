GO ?= go
VERSION ?= $(shell git describe --tags --match 'v*' --always --dirty 2>/dev/null || echo dev)

.PHONY: build test lint vet fmt snapshot

build:
	$(GO) build -trimpath -ldflags "-s -w -X github.com/iugu/cli/internal/cli.Version=$(VERSION)" -o bin/iugu ./cmd/iugu

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -l . && test -z "$$(gofmt -l .)"

lint: vet
	@command -v staticcheck >/dev/null && staticcheck ./... || echo "staticcheck not installed (go install honnef.co/go/tools/cmd/staticcheck@latest)"

# Local release rehearsal: every archive, checksums, changelog; no publishing, no signing. The SBOM step needs
# syft (brew install syft) and is skipped without it; CI has cosign and syft.
snapshot:
	TAP_GITHUB_TOKEN=unused goreleaser release --snapshot --clean --skip=publish,sign$$(command -v syft >/dev/null || echo ,sbom)
