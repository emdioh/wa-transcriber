VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILDTIME ?= $(shell git log -1 --format=%cI 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)

# Self-update public key (ed25519, base64): the first non-comment line of
# release/update-pubkey.txt, whitespace stripped. The file is committed but may
# hold only the placeholder comment, in which case an empty -X compiles
# self-update out (Docker image, dev builds). A missing file is treated the
# same as empty.
UPDATE_PUBKEY ?= $(shell sed -e 's/#.*//' release/update-pubkey.txt 2>/dev/null \
	| tr -d '[:space:]')

LDFLAGS := -s -w -X main.version=$(VERSION) -X main.buildTime=$(BUILDTIME) \
	-X main.updatePubKey=$(UPDATE_PUBKEY)
GOBUILD := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"
PKG := ./cmd/wa-transcriber

.PHONY: build build-arm7 build-arm64 build-amd64 dist test vet docker sign-manifest

build:
	$(GOBUILD) -o wa-transcriber $(PKG)

build-arm7:
	mkdir -p dist
	GOOS=linux GOARCH=arm GOARM=7 $(GOBUILD) -o dist/wa-transcriber-linux-armv7 $(PKG)

build-arm64:
	mkdir -p dist
	GOOS=linux GOARCH=arm64 $(GOBUILD) -o dist/wa-transcriber-linux-arm64 $(PKG)

build-amd64:
	mkdir -p dist
	GOOS=linux GOARCH=amd64 $(GOBUILD) -o dist/wa-transcriber-linux-amd64 $(PKG)

dist: build-arm7 build-arm64 build-amd64

test:
	go test -race ./...

vet:
	go vet ./...

docker:
	docker build --build-arg VERSION=$(VERSION) -t wa-transcriber:$(VERSION) .

# Sign the manifest of an already published release, offline, and upload the
# detached signature. CI publishes the unsigned dist/manifest.json; the
# private key never leaves the owner's machine, so this runs by hand:
#
#   make sign-manifest VERSION=v0.2.0 KEY=$HOME/wat-update.key
#
# Needs the gh CLI (authenticated) and the private key from wat-keygen.
#
# The body is one shell line (joined with ';') held in a define; make runs
# each define line in a separate shell, so everything that must share
# variables stays on one line. Backticks and '$$' keep the make-escaped
# dollars out of linters that misread recipe text.
define SIGN_MANIFEST_SH
set -eu; \
if [ -z "$(VERSION)" ]; then echo "error: VERSION is unset; usage: make sign-manifest VERSION=v0.2.0 KEY=$$HOME/wat-update.key" >&2; exit 1; fi; \
if [ -z "$(KEY)" ]; then echo "error: KEY is unset; usage: make sign-manifest VERSION=v0.2.0 KEY=$$HOME/wat-update.key" >&2; exit 1; fi; \
if [ ! -f "$(KEY)" ]; then echo "error: key file $(KEY) not found; run 'go run ./tools/wat-keygen -out $$HOME/wat-update.key' once" >&2; exit 1; fi; \
if ! command -v gh >/dev/null 2>&1; then echo "error: gh CLI not found; install it and run 'gh auth login'" >&2; exit 1; fi; \
tmp=`mktemp -d`; trap 'rm -rf "$$tmp"' EXIT; \
repo=`gh repo view --json nameWithOwner -q .nameWithOwner`; \
echo "downloading manifest.json from release $(VERSION)"; \
gh release download "$(VERSION)" --repo "$$repo" --pattern manifest.json --dir "$$tmp"; \
echo "signing with $(KEY)"; \
go run ./tools/wat-sign -key "$(KEY)" -in "$$tmp/manifest.json" -out "$$tmp/manifest.json.ed25519"; \
echo "uploading manifest.json.ed25519 to release $(VERSION)"; \
gh release upload "$(VERSION)" --repo "$$repo" --clobber "$$tmp/manifest.json.ed25519"; \
echo "done: release $(VERSION) now has a signed manifest"
endef

sign-manifest:
	@$(SIGN_MANIFEST_SH)
