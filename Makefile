BINARY := ghpr
PACKAGE := ./cmd/ghpr
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X github.com/mpepping/ghpr/internal/cmd.version=$(VERSION) \
	-X github.com/mpepping/ghpr/internal/cmd.commit=$(COMMIT) \
	-X github.com/mpepping/ghpr/internal/cmd.date=$(DATE)

.PHONY: build test lint fmt clean install

build:
	go build -buildvcs=false -ldflags "$(LDFLAGS)" -o $(BINARY) $(PACKAGE)

test:
	go test ./...

lint:
	go vet ./...
	@test -z "$$(gofmt -s -l .)" || (echo "Go code is not formatted:"; gofmt -s -d .; exit 1)

fmt:
	gofmt -s -w .

install:
	go install -buildvcs=false -ldflags "$(LDFLAGS)" $(PACKAGE)

clean:
	rm -f $(BINARY)
