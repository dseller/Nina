BIN     := bin/nina
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: all build test race vet lint fmt check clean run spec

all: check build

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/nina

test:
	go test ./...

# The race detector needs cgo and a C toolchain; CI runs this on Linux.
race:
	CGO_ENABLED=1 go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

check: vet test

# Validate the example configuration the way CI would.
run: build
	$(BIN) run -c examples/nina.yaml --log-format text

spec: build
	$(BIN) spec build -c examples/nina.yaml

clean:
	rm -rf bin dist
