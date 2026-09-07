BINARY := courier
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all build lint test fmt tidy run clean

all: tidy lint test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BINARY) ./cmd/courier

# Mandatory lint step. CI and `make all` must pass this.
lint:
	golangci-lint run ./...

test:
	go test ./...

fmt:
	golangci-lint fmt

tidy:
	go mod tidy

# Run the daemon in the foreground against the local config.
run: build
	./$(BINARY) serve

clean:
	rm -f $(BINARY)
