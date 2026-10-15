# syntax=docker/dockerfile:1
#
# patchbay: one static binary on distroless. Both base images are pinned by
# the digest of their multi-arch index, so amd64 and arm64 builds (CI) and
# every rebuild get exactly these bases. Bump tag and digest together:
#   docker buildx imagetools inspect golang:<ver>-trixie
#   docker buildx imagetools inspect gcr.io/distroless/static-debian13:nonroot
# The Go minor here must match the flake's go_1_26.

FROM --platform=$BUILDPLATFORM golang:1.26.8-trixie@sha256:aae9c439b447d24c93ee483408992857cf39a3fb6ab7fe53affc65d834891fa8 AS build

WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-trimpath

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/

ARG TARGETOS TARGETARCH
ARG VERSION=dev COMMIT=unknown DATE=unknown
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
      -ldflags "-s -w \
        -X github.com/ChristopherJMiller/patchbay/internal/version.Version=${VERSION} \
        -X github.com/ChristopherJMiller/patchbay/internal/version.Commit=${COMMIT} \
        -X github.com/ChristopherJMiller/patchbay/internal/version.Date=${DATE}" \
      -o /out/patchbay ./cmd/patchbay

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3

LABEL org.opencontainers.image.source="https://github.com/ChristopherJMiller/patchbay" \
      org.opencontainers.image.description="patchbay: a Kubernetes-native PBX (controller, agent, CLI)" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=build /out/patchbay /patchbay
USER 65532:65532
ENTRYPOINT ["/patchbay"]
