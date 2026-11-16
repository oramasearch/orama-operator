# SPDX-License-Identifier: MIT

# orama-operator — the kubebuilder manager image (the pod that reconciles the
# OramaCloudV1 CRs). Standard kubebuilder multi-stage build: compile the Go binary in the
# toolchain image, ship it on distroless/static:nonroot (no shell, runs as nonroot UID 65532).
# CGO is disabled → a fully static binary that runs on `static`. Base images are digest-pinned
# for reproducibility; the committed api/v1alpha1/zz_generated.deepcopy.go means `go build`
# needs no controller-gen at image-build time. Built via build/images/build.sh (context = this
# repo), NOT by prod CI. `make docker-build` also uses this file.
FROM golang:1.23@sha256:60deed95d3888cc5e4d9ff8a10c54e5edc008c6ae3fba6187be6fb592e19e8c0 AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /workspace
# Cache module downloads: copy manifests first, download, THEN copy source — so a source-only
# change doesn't re-fetch every dependency.
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/
# docs/control-plane is a Go package (the embedded deployment contract from #142).
COPY docs/control-plane/ docs/control-plane/
# Static build (no CGO) so the binary runs on distroless/static. -a rebuilds all packages.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -a -o manager ./cmd
# The mesh's node projector (docs/infrastructure/mesh.md) ships in the same image: the operator's
# node-local half, run by the mesh DaemonSet beside zetta-tunnel's agent.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o mesh-projector ./cmd/mesh-projector

FROM gcr.io/distroless/static:nonroot@sha256:d29e660cc75a5b6b1334e03c5c81ccf9bc0884a002c6000dbf0fb96034814478
WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=builder /workspace/mesh-projector .
USER 65532:65532
ENTRYPOINT ["/manager"]
