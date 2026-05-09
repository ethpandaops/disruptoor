# disruptoor
VERSION := $(shell git rev-parse --short HEAD 2>/dev/null || echo "dev")

.PHONY: all build test test-race clean

all: build

build:
	@echo version: $(VERSION)
	CGO_ENABLED=0 go build -trimpath -o bin/ -ldflags="-s -w" ./cmd/disruptoor

test:
	go test ./...

test-race:
	go test -race ./...

clean:
	rm -rf bin/
