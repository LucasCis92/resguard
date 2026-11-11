VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/LucasCis92/resguard/internal/version.Version=$(VERSION)

.PHONY: all build test vet fmt clean

all: build

build:
	go build -ldflags "$(LDFLAGS)" -o bin/resguardd ./cmd/resguardd
	go build -ldflags "$(LDFLAGS)" -o bin/resguardctl ./cmd/resguardctl

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

clean:
	rm -rf bin/
