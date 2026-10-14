VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build run test test-servers autopilot notices windows windows-image windows-notices windows-native clean

## build: build the release binary into bin/dibi
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/dibi ./cmd/dibi

run: build
	./bin/dibi

## notices: write the third-party notices of bin/dibi into
## bin/THIRD_PARTY_NOTICES.txt (shipped with the release)
notices:
	build/notices.sh $(VERSION) bin/THIRD_PARTY_NOTICES.txt

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

## windows: cross-compile bin/dibi.exe (static) in Docker, see build/windows
WINDOWS_IMAGE := dibi-windows-build
WINDOWS_RUN = docker run --rm -u $$(id -u):$$(id -g) -v $(CURDIR):/src -w /src \
	-v $$(go env GOMODCACHE):/gomod -e GOMODCACHE=/gomod \
	-e GOCACHE=/src/bin/.windows-cache -e HOME=/tmp $(WINDOWS_IMAGE)
windows: windows-image
	mkdir -p bin/.windows-cache
	$(WINDOWS_RUN) go build -trimpath -tags windowsqtstatic \
		-ldflags "$(LDFLAGS) -H windowsgui" -o bin/dibi.exe ./cmd/dibi

## windows-image: build the Docker image for the Windows builds (the first
## run builds Qt from source and takes about 20 minutes)
windows-image:
	docker build -t $(WINDOWS_IMAGE) build/windows

## windows-notices: write the third-party notices of bin/dibi.exe into
## bin/THIRD_PARTY_NOTICES-windows.txt
windows-notices: windows-image
	mkdir -p bin/.windows-cache
	$(WINDOWS_RUN) build/notices.sh $(VERSION) bin/THIRD_PARTY_NOTICES-windows.txt

## windows-native: build bin/dibi.exe (static) on Windows, from the MSYS2
## UCRT64 shell (see the README for the packages to install)
QT_STATIC ?= /ucrt64/qt6-static
windows-native:
	sh build/windows/qt-pkgconfig.sh "$$(cygpath -m $(QT_STATIC))" bin/.qt-pkgconfig
	PKG_CONFIG_LIBDIR="$$(cygpath -m $(CURDIR)/bin/.qt-pkgconfig)" \
		CGO_ENABLED=1 CGO_LDFLAGS_ALLOW='.*' \
		go build -trimpath -tags windowsqtstatic \
		-ldflags "$(LDFLAGS) -H windowsgui" -o bin/dibi.exe ./cmd/dibi

clean:
	rm -rf bin
