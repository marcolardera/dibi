VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build run test test-servers autopilot clean

## build: build the release binary into bin/dibi
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/dibi ./cmd/dibi

run: build
	./bin/dibi

## test: unit tests plus SQLite/DuckDB/CSV/Parquet integration tests
test:
	go vet ./...
	go test ./...

## test-servers: also run integration tests against the Docker databases
## (see README for the containers)
test-servers:
	DIBI_TEST_SERVERS=1 go test ./...

## autopilot: build the UI end-to-end driver (never shipped)
autopilot:
	go build -tags autopilot -o bin/dibi-autopilot ./cmd/dibi

clean:
	rm -rf bin
