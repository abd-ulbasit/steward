# Build stage. It runs on the build host's platform and cross-compiles for the
# target platform, so multi-arch builds need no emulation. Base images are
# pinned by digest; Dependabot (docker ecosystem) proposes updates.
FROM --platform=$BUILDPLATFORM golang:1.26.10-alpine@sha256:27f2d3b83cfc5803c8078f8e730af0f7c431bf067afaec8f60a36fb763187cba AS builder

# Set by BuildKit for the platform being built (e.g. linux/arm64).
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

# Dependencies first, so source edits do not invalidate the module cache layer.
COPY go.mod go.sum ./
RUN go mod download

# .dockerignore admits only *.go (minus *_test.go), go.mod and go.sum.
COPY . .

# Static binary for the target platform. -trimpath keeps host paths out of the
# binary; GOARCH must follow TARGETARCH, or every image would carry a binary
# for the build host's architecture.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-w -s' -o /manager ./cmd/

# Runtime stage: distroless static, non-root. No shell or package manager;
# CA certificates and tzdata are included in the base.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:d62067879de890a1def9da0e7766b7d20810a5e5555cef8506b752437d7a3e88

COPY --from=builder /manager /manager

# Numeric UID:GID (the distroless "nonroot" user) so runAsNonRoot can verify it.
USER 65532:65532

# No HEALTHCHECK (Trivy DS-0026): Kubernetes ignores it, and the manager
# Deployment probes /healthz and /readyz on :8081 instead.
ENTRYPOINT ["/manager"]
