# syntax=docker/dockerfile:1.27.1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
#
# patchbay-e2e-tools: the e2e suite's simulated phone (fakephone), one static
# binary on distroless. Build from the repository root:
#   docker build -f test/e2e/fakephone/Dockerfile \
#     -t ghcr.io/christopherjmiller/patchbay-e2e-tools:e2e .
# (`make e2e-images` does this.) The frontend and the bases are the same
# digest-pinned images as the main Dockerfile; bump them together.

FROM --platform=$BUILDPLATFORM golang:1.26.8-trixie@sha256:aae9c439b447d24c93ee483408992857cf39a3fb6ab7fe53affc65d834891fa8 AS build

WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-trimpath

COPY go.mod go.sum ./
COPY test/e2e/fakephone/ test/e2e/fakephone/

ARG TARGETOS TARGETARCH
# go build downloads only the modules fakephone imports.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-s -w" -o /out/fakephone ./test/e2e/fakephone

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3

LABEL org.opencontainers.image.source="https://github.com/ChristopherJMiller/patchbay" \
      org.opencontainers.image.description="patchbay e2e tools: a simulated Cisco 79x5 (fakephone)" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=build /out/fakephone /fakephone
USER 65532:65532
ENTRYPOINT ["/fakephone"]
