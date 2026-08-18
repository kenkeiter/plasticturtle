GO ?= go
BIN := bin/pt

# Stamped into the binary so `pt --version` reports something traceable rather
# than the literal string "dev".
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: check
check: fmt-check vet build test

.PHONY: fmt
fmt:
	gofmt -w .

.PHONY: fmt-check
fmt-check:
	@out="$$(gofmt -l . )"; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: build
build:
	$(GO) build $(LDFLAGS) -o $(BIN) ./cmd/pt

.PHONY: test
test:
	$(GO) test -race ./...

# Opt-in end-to-end test against a real Tart image. Requires macOS, tart, and
# ghcr.io/cirruslabs/macos-tahoe-base pulled locally. Slow by nature: it boots
# a VM.
.PHONY: integration
integration:
	$(GO) test -race -tags integration -timeout 20m ./...

.PHONY: clean
clean:
	rm -rf bin
