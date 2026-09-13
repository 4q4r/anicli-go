# Multi-stage build: static Go binary in, distroless runtime out.
# The image runs `anicli serve` (HTTP API face) as the nonroot user,
# port 8765. No shell, no package manager, no cgo.

FROM golang:1.27-alpine AS builder

WORKDIR /src

# Cache the module layer first.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static build with the release version triple injected (same seams
# goreleaser uses; date stamp from SOURCE_DATE_EPOCH when provided).
ARG VERSION=dev
ARG COMMIT=none
ENV CGO_ENABLED=0
RUN BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) && \
    go build -trimpath \
      -ldflags="-s -w \
        -X github.com/an0nx/anicli-go/internal/cli.Version=${VERSION} \
        -X github.com/an0nx/anicli-go/internal/cli.Commit=${COMMIT} \
        -X github.com/an0nx/anicli-go/internal/cli.Date=${BUILD_DATE}" \
      -o /out/anicli ./cmd/anicli

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/anicli /anicli

# Optional: settings.toml baked in at build time.
# COPY settings.toml /config/settings.toml

EXPOSE 8765

USER nonroot:nonroot

ENTRYPOINT ["/anicli"]
CMD ["serve"]
