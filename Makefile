.PHONY: build test lint tidy release docker-build load parity goldens-update

build:
	go build ./...

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run

tidy:
	go mod tidy

# Cross-platform release artifacts (dist/) via goreleaser.
release:
	goreleaser release --clean

# Distroless container image (local tag, no registry push).
docker-build:
	docker build -t anicli:latest .

# Load/stress suite. Runs WITHOUT -race: the SLOs are wall-clock
# honest. For race-safety of the same paths run manually
#   go test -race -tags=load -run '^TestLoad' -count=1 ./internal/{api,storage,download,loadtest}/
# (latency SLOs will exceed under instrumentation — expected; only the
# absence of DATA RACE reports matters there).
load:
	go test -tags=load -run '^TestLoad' -count=1 -v ./internal/api/ ./internal/storage/ ./internal/download/ ./internal/loadtest/

# Live G1 gate: probes all 11 providers against real sites.
parity:
	go run ./cmd/parity all

# Regenerate the API contract goldens (review the diff!).
goldens-update:
	go test ./internal/regression -update -count=1
