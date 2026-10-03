# syntax=docker/dockerfile:1

# VeriDB audit viewer.
#
# The MCP server itself is not containerised: it runs as a stdio child of the
# agent (pi) on the host. Only the viewer is packaged, so that the audit trail
# can be reached from outside through a tunnel.

FROM golang:1.26-bookworm AS build

WORKDIR /src

# Dependencies first so a source-only edit does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# modernc.org/sqlite is pure Go, so a static binary needs no cgo and the runtime
# image needs no libc.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags="-s -w" \
      -o /out/veridb-viewer ./cmd/veridb-viewer

# /data is pre-created with the runtime user's ownership so a named volume
# mounted there inherits permissions the non-root user can write to.
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/veridb-viewer /usr/local/bin/veridb-viewer
COPY --from=build --chown=65532:65532 /out/data /data

# The JSONL trail veridb writes is mounted read-only at /audit; the viewer's own
# SQLite store lives in the writable volume at /data.
ENV AUDIT_FILE=/audit/veridb-audit.jsonl \
    AUDIT_DB=/data/audit.db \
    VIEWER_ADDR=:8080 \
    VIEWER_CACHE_TTL=5s \
    VIEWER_INTERVAL=2s

EXPOSE 8080

USER nonroot:nonroot

HEALTHCHECK --interval=15s --timeout=5s --start-period=5s --retries=5 \
  CMD ["/usr/local/bin/veridb-viewer", "-healthcheck"]

ENTRYPOINT ["/usr/local/bin/veridb-viewer"]
