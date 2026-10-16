# syntax=docker/dockerfile:1.28.0@sha256:bb22d9815c728170f72750f4e5b0d672e06176142e1d602c7e66c050100b7e5b
#
# patchbay: one static binary on distroless. The Dockerfile frontend (the
# syntax line) and both base images are pinned by the digest of their
# multi-arch index, so amd64 and arm64 builds (CI) and every rebuild use
# exactly these. Bump tag and digest together:
#   docker buildx imagetools inspect docker/dockerfile:<ver>
#   docker buildx imagetools inspect golang:<ver>-trixie
#   docker buildx imagetools inspect gcr.io/distroless/static-debian13:nonroot
# The Go minor here must match the flake's go_1_26.

FROM --platform=$BUILDPLATFORM golang:1.26.9-trixie@sha256:f06b7122f888680048afb247c4e5f49c0069eb6c4c1ccae0f3f4834527a8d22c AS build

WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-trimpath

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/

ARG TARGETOS TARGETARCH
ARG VERSION=dev COMMIT=unknown DATE=unknown
# Go build tags: empty for every real image. `make e2e-images` passes e2e,
# which adds the e2e test firmware load (docs/plans/release-a.md §2(b)).
ARG GO_TAGS=""
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -tags "${GO_TAGS}" \
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
# The sound prompts in every voice, <voice>/<name>.{ulaw,g722}, rendered by
# `make sounds` (hack/gen-sounds.sh) before the build; the agent installs
# one voice (--sounds-voice) into the sounds volume at start and is not
# ready without it.
COPY build/sounds/ /usr/share/patchbay/sounds/
USER 65532:65532
ENTRYPOINT ["/patchbay"]
