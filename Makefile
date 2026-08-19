VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)

.PHONY: build test vet lint run clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o grok-gateway-proxy .

test:
	go test ./...

vet:
	go vet ./...

lint:
	golangci-lint run

run:
	go run .

clean:
	rm -f grok-gateway-proxy
