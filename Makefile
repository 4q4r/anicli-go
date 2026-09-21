.PHONY: build test lint tidy release docker-build load parity goldens-update build-matrix notices

# Cross-compile every goreleaser target (CGO off). Regression guard:
# platform-only APIs (e.g. syscall.Kill) must never sneak back into
# portable files — see PR12 review C1.
BUILD_MATRIX := linux/amd64 linux/arm64 windows/amd64 darwin/amd64 darwin/arm64

build-matrix:
	@set -e; for target in $(BUILD_MATRIX); do \
		os=$${target%/*}; arch=$${target#*/}; \
		echo "==> building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build ./...; \
	done

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
#   go test -race -tags=load -run '^TestLoad' -count=1 ./internal/{api,storage,download,loadtest,buffered,torrent}/
# (latency SLOs will exceed under instrumentation — expected; only the
# absence of DATA RACE reports matters there).
load:
	go test -tags=load -run '^TestLoad' -count=1 -v ./internal/api/ ./internal/storage/ ./internal/download/ ./internal/loadtest/ ./internal/buffered/ ./internal/torrent/

# Live G1 gate: probes all 17 providers against real sites.
parity:
	go run ./cmd/parity all

# Regenerate the API contract goldens (review the diff!).
goldens-update:
	go test ./internal/regression -update -count=1

# List the direct dependency inventory for THIRD-PARTY-NOTICES.md:
# versions refresh from go.mod; licenses live in each module's
# LICENSE/COPYING file in the module cache. Manually reconcile the
# table after dependency changes.
notices:
	@go list -m -f '{{if and (not .Indirect) (ne .Path "github.com/an0nx/anicli-go")}}{{.Path}} {{.Version}}{{end}}' all
	@echo "--> reconcile THIRD-PARTY-NOTICES.md (module cache: $$(go env GOMODCACHE))"
