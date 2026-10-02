VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build pi test clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o dist/chokominto ./cmd/chokominto

# The binary for a Raspberry Pi 4 or 5 running a 64-bit OS.
pi:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/chokominto-linux-arm64 ./cmd/chokominto

test:
	go vet ./...
	go test ./...

clean:
	rm -rf dist
