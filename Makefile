.PHONY: check test race coverage lint fmt fmt-check tidy-check naming-check duplicate-check build agentctl install clean

GO ?= go
PKG ?= ./...
CMD ?= ./cmd/cli
BIN_DIR ?= bin
BINARY ?= $(BIN_DIR)/agentctl
COVERAGE_FILE ?= coverage.out

check: naming-check fmt-check tidy-check lint duplicate-check build test

test:
	$(GO) test $(PKG)

race:
	$(GO) test -race $(PKG)

coverage:
	$(GO) test -covermode=atomic -coverprofile=$(COVERAGE_FILE) $(PKG)
	$(GO) tool cover -func=$(COVERAGE_FILE)

lint:
	golangci-lint run

fmt:
	$(GO) fmt $(PKG)

fmt-check:
	@test -z "$$(gofmt -l $$(git ls-files '*.go'))" || (echo "Go files need formatting; run make fmt" && gofmt -l $$(git ls-files '*.go') && exit 1)

tidy-check:
	$(GO) mod tidy
	git diff --exit-code -- go.mod go.sum

naming-check:
	bash .git-hooks/check-naming.sh --all

duplicate-check:
	@if command -v dupl >/dev/null 2>&1; then \
		dupl -threshold 100 -plumbing ./pkg ./cmd ./test; \
	else \
		echo "dupl not installed; install with: go install github.com/mibk/dupl@latest"; \
	fi

build: agentctl

agentctl:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BINARY) $(CMD)

install:
	$(GO) install $(CMD)

clean:
	rm -rf $(BIN_DIR) $(COVERAGE_FILE)
