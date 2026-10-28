VERSION ?= $(or $(shell git describe --tags --match 'v*' --always --dirty 2>/dev/null | sed 's/^v//'),dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build check test cover vet lint release release-darwin release-other release-sums clean

build: ## build ./dist/ex-runner for this machine
	go build -trimpath -ldflags "$(LDFLAGS)" -o dist/ex-runner ./cmd/ex-runner

# What CI runs: lint, cross-OS vet, then the race-enabled suite under the
# 100% coverage gate.
check: lint vet cover

test:
	go test -race ./...

# The coverage gate: 100% of statements outside the process entrypoint
# (see .testcoverage.yml).
cover:
	go test -race -coverprofile=coverage.out -covermode=atomic ./...
	go run github.com/vladopajic/go-test-coverage/v2@v2.18.8 --config=.testcoverage.yml

vet:
	go vet ./...
	GOOS=windows go vet ./...
	GOOS=linux go vet ./...
	GOOS=darwin go vet ./...

lint:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "gofmt needed" && exit 1)
	golangci-lint run ./...

# Release archives: ex-runner-<version>-<os>-<arch>.tar.gz plus SHA256SUMS
# (the desktop app and the brew formula depend on these names).
#
# macOS builds use cgo for the Secure Enclave (internal/devicekey), so they
# need a Mac with Xcode's command line tools: Intel is cross-built with
# `clang -arch x86_64`. The minimum macOS is pinned to Go's own (13.0) — the
# SDK's default would be the build machine's version. Everything else is a
# static CGO_ENABLED=0 binary and builds anywhere. CI runs the two halves on
# macOS and Linux and merges them with release-sums; `make release` does it
# all on one Mac.
DARWIN_TARGETS := darwin/arm64 darwin/amd64
OTHER_TARGETS  := linux/arm64 linux/amd64 windows/arm64 windows/amd64
MACOS_MIN      := 13.0

# $(call package,os,arch): build one target into release/<name>.tar.gz.
# The build's environment comes before the call.
define package
	name=ex-runner-$(VERSION)-$(1)-$(2); ext=; [ $(1) = windows ] && ext=.exe; \
	mkdir -p release/$$name && \
	GOOS=$(1) GOARCH=$(2) go build -trimpath -ldflags "$(LDFLAGS)" -o release/$$name/ex-runner$$ext ./cmd/ex-runner && \
	cp LICENSE README.md release/$$name/ && \
	tar -C release -czf release/$$name.tar.gz $$name && rm -rf release/$$name
endef

release:
	rm -rf release
	$(MAKE) release-darwin release-other VERSION=$(VERSION)
	$(MAKE) release-sums

release-darwin:
	@test "$$(go env GOHOSTOS)" = darwin || (echo "release-darwin needs macOS (cgo + Security.framework)" && exit 1)
	mkdir -p release
	for target in $(DARWIN_TARGETS); do \
		arch=$${target#*/}; clangarch=$$arch; [ $$arch = amd64 ] && clangarch=x86_64; \
		export CGO_ENABLED=1 CC="clang -arch $$clangarch" \
			CGO_CFLAGS="-O2 -g -mmacosx-version-min=$(MACOS_MIN)" CGO_LDFLAGS="-O2 -g -mmacosx-version-min=$(MACOS_MIN)"; \
		$(call package,darwin,$$arch) || exit 1; \
	done

release-other:
	mkdir -p release
	for target in $(OTHER_TARGETS); do \
		os=$${target%/*}; arch=$${target#*/}; \
		export CGO_ENABLED=0; \
		$(call package,$$os,$$arch) || exit 1; \
	done

release-sums:
	cd release && shasum -a 256 *.tar.gz > SHA256SUMS

clean:
	rm -rf dist release coverage.out
