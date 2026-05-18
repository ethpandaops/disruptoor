# disruptoor
VERSION := $(shell git rev-parse --short HEAD 2>/dev/null || echo "dev")
RELEASE ?= $(VERSION)

.PHONY: all build test test-race clean

all: build

build:
	@echo version: $(RELEASE)
	CGO_ENABLED=0 go build -trimpath -o bin/ -ldflags="-s -w -X main.version=$(RELEASE)" ./cmd/disruptoor

test:
	go test ./...

test-race:
	go test -race ./...

clean:
	rm -rf bin/
