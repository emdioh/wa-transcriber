VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILDTIME ?= $(shell git log -1 --format=%cI 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.buildTime=$(BUILDTIME)
GOBUILD := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"
PKG := ./cmd/wa-transcriber

.PHONY: build build-arm7 build-arm64 build-amd64 dist test vet docker

build:
	$(GOBUILD) -o wa-transcriber $(PKG)

build-arm7:
	GOOS=linux GOARCH=arm GOARM=7 $(GOBUILD) -o dist/wa-transcriber-linux-armv7 $(PKG)

build-arm64:
	GOOS=linux GOARCH=arm64 $(GOBUILD) -o dist/wa-transcriber-linux-arm64 $(PKG)

build-amd64:
	GOOS=linux GOARCH=amd64 $(GOBUILD) -o dist/wa-transcriber-linux-amd64 $(PKG)

dist: build-arm7 build-arm64 build-amd64

test:
	go test -race ./...

vet:
	go vet ./...

docker:
	docker build --build-arg VERSION=$(VERSION) -t wa-transcriber:$(VERSION) .
