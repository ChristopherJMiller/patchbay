# syntax=docker/dockerfile:1.28.0@sha256:bb22d9815c728170f72750f4e5b0d672e06176142e1d602c7e66c050100b7e5b
#
# patchbay-e2e-tools: the e2e suite's simulated phones (fakephone), one static
# binary on distroless. Build from the repository root:
#   docker build -f test/e2e/fakephone/Dockerfile \
#     -t ghcr.io/christopherjmiller/patchbay-e2e-tools:e2e .
# (`make e2e-images` does this.) The frontend and the bases are the same
# digest-pinned images as the main Dockerfile; bump them together.

FROM --platform=$BUILDPLATFORM golang:1.26.9-trixie@sha256:f06b7122f888680048afb247c4e5f49c0069eb6c4c1ccae0f3f4834527a8d22c AS build

WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-trimpath

COPY go.mod go.sum ./
# fakephone serves the agent's scraped pages built from internal/agent/scrape's
# own types, which bring api/ and internal/ (Dockerfile.dockerignore keeps out
# tests and testdata).
COPY api/ api/
COPY internal/ internal/
COPY test/e2e/fakephone/ test/e2e/fakephone/

ARG TARGETOS TARGETARCH
# go build compiles, and downloads the modules of, only what fakephone imports.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-s -w" -o /out/fakephone ./test/e2e/fakephone

FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99

LABEL org.opencontainers.image.source="https://github.com/ChristopherJMiller/patchbay" \
      org.opencontainers.image.description="patchbay e2e tools: a simulated Cisco 79x5 and Linphone (fakephone)" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=build /out/fakephone /fakephone
USER 65532:65532
ENTRYPOINT ["/fakephone"]
