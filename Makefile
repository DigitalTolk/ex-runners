VERSION ?= $(or $(shell git describe --tags --match 'v*' --always --dirty 2>/dev/null | sed 's/^v//'),dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build check test cover vet lint release clean

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

# Cross-compiled release archives: one static binary per OS/arch.
release:
	rm -rf release && mkdir -p release
	for target in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64 windows/arm64 windows/amd64; do \
		os=$${target%/*}; arch=$${target#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		name=ex-runner-$(VERSION)-$$os-$$arch; mkdir -p release/$$name; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o release/$$name/ex-runner$$ext ./cmd/ex-runner || exit 1; \
		cp LICENSE README.md release/$$name/; \
		tar -C release -czf release/$$name.tar.gz $$name && rm -rf release/$$name; \
	done
	cd release && shasum -a 256 *.tar.gz > SHA256SUMS

clean:
	rm -rf dist release coverage.out
