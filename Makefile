# The binary must be named like its plugin folder (plugins/netbird/netbird).
NAME := netbird
GO ?= go

.PHONY: build test e2e release clean

build:
	mkdir -p dist/$(NAME)
	$(GO) build -trimpath -o dist/$(NAME)/$(NAME) .

test:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "run gofmt"; exit 1; }
	$(GO) vet ./...
	$(GO) test -race ./...

# Runs the sync against a real NetBird server in Docker.
e2e:
	test/run.sh

# Assets follow the plugin store's naming: last segment of the plugin ID + _os_arch.
release:
	rm -rf dist/release && mkdir -p dist/release
	@for arch in amd64 arm64; do \
		echo "building linux_$$arch"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath -ldflags="-s -w" -o dist/release/$(NAME)_linux_$$arch . || exit 1; \
	done
	cd dist/release && shasum -a 256 $(NAME)_* > SHA256SUMS

clean:
	rm -rf dist
