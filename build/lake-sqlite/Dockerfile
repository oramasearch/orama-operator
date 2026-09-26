# lake-sqlite — the web service behind lake's sqlite service kind (docs/lake/lake-sqlite.md). One
# static binary over one SQLite database, built with modernc.org/sqlite (pure Go, BSD-3-Clause), so
# CGO stays off and the runtime is distroless/static: no shell, no libc, running as the numeric
# nonroot user 65532. Base images are digest-pinned, the same builder and runtime as the operator's
# own Dockerfile.
#
# Build from the repository root, for linux/amd64:
#   docker buildx build --platform linux/amd64 -f build/lake-sqlite/Dockerfile \
#     -t oramasearch/lake-sqlite:<tag> .
FROM golang:1.23@sha256:60deed95d3888cc5e4d9ff8a10c54e5edc008c6ae3fba6187be6fb592e19e8c0 AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /workspace
# Module downloads first, so a source-only change reuses the layer.
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download
COPY cmd/lake-sqlite/ cmd/lake-sqlite/
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/lake-sqlite ./cmd/lake-sqlite

FROM gcr.io/distroless/static:nonroot@sha256:d29e660cc75a5b6b1334e03c5c81ccf9bc0884a002c6000dbf0fb96034814478
COPY --from=builder /out/lake-sqlite /lake-sqlite
USER 65532:65532
EXPOSE 8080
# The database lives on the mounted volume; the StatefulSet mounts it at /data.
ENV LAKE_SQLITE_PATH=/data/main.db
ENTRYPOINT ["/lake-sqlite"]
