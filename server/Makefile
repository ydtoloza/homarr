GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
ROOT := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))

.PHONY: build test vet tidy run docker

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/homarr-go ./cmd/homarr-go

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

run:
	HOMARR_DATA_DIR=./appdata HOMARR_LOG_LEVEL=debug $(GO) run ./cmd/homarr-go

# Build context is the repo root (server/Dockerfile copies server/ sources).
docker:
	docker build -f $(ROOT)Dockerfile -t homarr-go:local $(ROOT)..
