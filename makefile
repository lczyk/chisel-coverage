.SUFFIXES:

SRCS := $(shell find ./cmd ./internal -name '*.go') version.go VERSION

# Lint tools run via `go run`, pinned so a new release cannot fail the build
# unannounced. staticcheck reads a toolchain's export data, so v0.8.x is the
# floor for the Go running here.
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.1
DEADCODE    := golang.org/x/tools/cmd/deadcode@v0.48.0

help:  ## Show this help
	@awk 'BEGIN {FS = ":.*?## "} /^##@/ {printf "\n\033[1m%s\033[0m\n", substr($$0, 5)} /^[a-zA-Z0-9_.\/-]+:.*?## / {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

##@ build

.PHONY: build
build: ./bin/chisel-coverage  ## Build the binary into ./bin

./bin/chisel-coverage: $(SRCS) makefile go.mod go.sum
	mkdir -p ./bin
	go build -o ./bin/chisel-coverage ./cmd/chisel-coverage

.PHONY: install
install: ./bin/chisel-coverage  ## Symlink the binary into ~/.local/bin
	mkdir -p $(HOME)/.local/bin
	ln -sf "$(PWD)/bin/chisel-coverage" "$(HOME)/.local/bin/chisel-coverage"

##@ checks

.PHONY: test
test:  ## Run the unit tests with the race detector (real chisel ones skip without `chisel`)
	@if command -v gotest >/dev/null 2>&1; then \
		gotest -race ./...; \
	else \
		go test -race ./...; \
	fi

.PHONY: e2e
e2e:  ## Run the end-to-end test (needs `chisel` + network; override CHISEL=..)
	CHISEL_COVERAGE_E2E=1 go test -tags e2e -count=1 -v ./e2e/...

.PHONY: lint
lint:  ## go vet + staticcheck + deadcode + gofmt check (no writes)
	go vet ./...
	go run $(STATICCHECK) ./...
	@out=$$(go run $(DEADCODE) -test ./...); \
	if [ -n "$$out" ]; then echo "Unreachable code:"; echo "$$out"; exit 1; fi
	@out=$$(gofmt -s -l ./cmd ./internal ./e2e version.go); \
	if [ -n "$$out" ]; then echo "Unformatted files:"; echo "$$out"; exit 1; fi

.PHONY: format
format:  ## gofmt the tree in place
	gofmt -s -w ./cmd ./internal ./e2e version.go

.PHONY: cover
cover:  ## Coverage profile + HTML report (cover.out, cover.html)
	go test -coverpkg=./... -coverprofile=cover.out -race ./...
	go tool cover -func=cover.out
	go tool cover -html=cover.out -o cover.html

.PHONY: verify
verify: lint test  ## Pre-commit gate: lint + test
	@echo "All checks passed."

.PHONY: clean
clean:  ## Remove build artefacts
	rm -f ./bin/chisel-coverage
	rm -f ./cover.out ./cover.html
