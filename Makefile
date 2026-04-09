.PHONY: build test test-race test-live test-cover vet lint fuzz bench clean

# Default: build + vet + test
all: build vet test

build:
	go build ./...

vet:
	go vet ./...

# Unit tests only (no IB Gateway required, safe for CI)
test:
	go test -tags='!integration' ./...

test-race:
	go test -race -count=1 -tags='!integration' ./...

# Integration tests (requires IB Gateway running)
# Usage: make test-live IB_PORT=4002
test-live:
	go test -tags=integration -v -race -count=1 -run TestIntegration .

# Live smoke test
smoke:
	go run ./cmd/smoketest

test-cover:
	go test -cover -tags='!integration' ./...

fuzz:
	go test -fuzz=FuzzDecodeMessage -fuzztime=10s ./protocol/

bench:
	go test -bench=. -benchmem ./protocol/

clean:
	go clean -testcache
