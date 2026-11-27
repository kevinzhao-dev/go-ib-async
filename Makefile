.PHONY: all build vet lint fmt test test-race test-shuffle test-live test-cover cover-check fuzz fuzz-all bench smoke ci clean

# Default: build + vet + test
all: build vet test

build:
	go build ./...

# go vet, including the integration-tagged files so they keep compiling
vet:
	go vet ./...
	go vet -tags=integration ./...

# Formatting and module hygiene (same checks as the CI lint job)
lint: vet
	@unformatted=$$(gofmt -l .); if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	go mod tidy -diff

fmt:
	gofmt -w .

# Unit + E2E tests (no IB Gateway required, safe for CI)
test:
	go test ./...

test-race:
	go test -race -count=1 ./...

# Random test order to catch hidden dependencies between tests
test-shuffle:
	go test -race -count=1 -shuffle=on ./...

# Integration tests (requires IB Gateway running)
# Usage: make test-live IB_PORT=4002
#        IB_E2E_TRADING=1 make test-live   (also places paper orders)
test-live:
	go test -tags=integration -v -race -count=1 -run TestIntegration .

test-cover:
	go test -cover ./...

# Coverage report (coverage.out/.txt/.html) + gate: COVERAGE_MIN, default 90
cover-check:
	./scripts/coverage.sh

# Live smoke test
smoke:
	go run ./cmd/smoketest

fuzz:
	go test -run='^$$' -fuzz=FuzzDecodeMessage -fuzztime=10s ./protocol/

# Every fuzz target for FUZZTIME (default 10s) each
fuzz-all:
	./scripts/fuzz.sh

bench:
	go test -bench=. -benchmem ./protocol/

# Everything CI runs
ci: build lint test-shuffle cover-check

clean:
	go clean -testcache
