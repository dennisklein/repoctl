# Build, test and lint repoctl. CI runs the same targets.
GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/dennisklein/repoctl/internal/config.Version=$(VERSION:v%=%)

.PHONY: all build test e2e lint generate check-generate contract clean

all: build test lint

build:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/repoctl ./cmd/repoctl

test:
	$(GO) test ./...

# The end-to-end tests run the story with the fake mock; they need rpmbuild,
# createrepo_c, rpmsign and gpg.
e2e:
	$(GO) test -tags e2e -count=1 ./internal/e2e/...

lint:
	golangci-lint run ./...

generate:
	$(GO) generate ./...

# Fails when the generated contract types differ from the schemas.
check-generate: generate
	git diff --exit-code -- internal/api

# Validates the contract examples against the schemas.
contract:
	$(GO) test -run 'TestExamples|TestSchemaCopies' ./internal/api/

clean:
	rm -rf bin
