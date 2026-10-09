# syntax=docker/dockerfile:1
#
# The SDK's example pack image. Build it from the SDK repository root (the
# build context must be the root so the worker package is in reach):
#
#   docker buildx build --platform linux/amd64 --provenance=false --sbom=false \
#     -f examples/hello/Dockerfile -t ghcr.io/<org>/hello:0.1.0 .
#
# The layout rules this file follows are in docs/image-layout.md. The one that
# matters most: manifest.json is copied LAST so it sits in the topmost layer,
# which is the first place the console looks for it.

# ── Stage 1: build ────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS builder
WORKDIR /src
# GOWORK=off: the SDK's go.work also lists the nested tool modules, which this
# image does not need and the context does not carry.
ENV GOWORK=off
COPY go.mod go.sum ./
RUN go mod download
COPY gen/ ./gen/
COPY worker/ ./worker/
COPY examples/hello/main.go ./examples/hello/main.go
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /out/hello ./examples/hello

# ── Stage 2: runtime ──────────────────────────────────────────────────────
# Distroless static, nonroot: no shell, no package manager.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/hello /hello
# The OpenAPI fragment the manifest points at (manifest.openapi_path).
COPY examples/hello/openapi.json /stagehand/openapi.json
# To ship a UI bundle, add its COPY line HERE, before the manifest:
#   COPY dist/ui/ /stagehand/ui/
# The manifest stays the last file layer (docs/image-layout.md).
COPY examples/hello/manifest.json /stagehand/manifest.json
ENTRYPOINT ["/hello"]
