# syntax=docker/dockerfile:1.28.0@sha256:bb22d9815c728170f72750f4e5b0d672e06176142e1d602c7e66c050100b7e5b
#
# patchbay: one static binary, in two images:
#   - the last stage (the default target): patchbay on distroless, which
#     runs the controller, the agent and the file server
#     (ghcr.io/christopherjmiller/patchbay);
#   - --target stream-bridge: the same binary on Alpine with its ffmpeg, for
#     `patchbay stream-bridge`
#     (ghcr.io/christopherjmiller/patchbay-stream-bridge;
#     docs/plans/release-b.md §2(f)).
# The Dockerfile frontend (the syntax line) and every base image are pinned
# by the digest of their multi-arch index, so amd64 and arm64 builds (CI)
# and every rebuild use exactly these. Bump tag and digest together:
#   docker buildx imagetools inspect docker/dockerfile:<ver>
#   docker buildx imagetools inspect golang:<ver>-trixie
#   docker buildx imagetools inspect alpine:<ver>
#   docker buildx imagetools inspect gcr.io/distroless/static-debian13:nonroot
# The Go minor here must match the flake's go_1_26.

FROM --platform=$BUILDPLATFORM golang:1.26.10-trixie@sha256:909771fef53e3dcb0950255f0c9a8b4728c3b841e61509c7b29b098de7ee02d4 AS build

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

# The stream bridge: patchbay plus Alpine's ffmpeg, which decodes the live
# sources (a tuner's MPEG-TS, Icecast radio) that `patchbay stream-bridge`
# plays to callers. ffmpeg runs as a separate process; patchbay links
# nothing of it. Alpine builds it with --enable-gpl and --enable-version3
# (GPL-3.0 or later), never with --enable-nonfree, which would make the
# image unredistributable: the build fails unless ffmpeg's -buildconf says
# --enable-gpl and not --enable-nonfree. ffmpeg's patch level follows
# Alpine's 3.24 branch at build time; the published digest pins what runs,
# and the bridge reports `ffmpeg -version`. The corresponding sources are
# Alpine's (aports at the package's commit, and its distfiles;
# docs/streams.md). This stage runs apk on the target platform, so an arm64
# build needs QEMU (CI's setup-qemu-action). `make
# image-check-stream-bridge` checks the result (test/image).
FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS stream-bridge

RUN apk add --no-cache ffmpeg ca-certificates \
 && ffmpeg -hide_banner -buildconf | grep -q -- '--enable-gpl' \
 && ! ffmpeg -hide_banner -buildconf | grep -q -- '--enable-nonfree'

LABEL org.opencontainers.image.source="https://github.com/ChristopherJMiller/patchbay" \
      org.opencontainers.image.description="patchbay stream bridge: live audio sources to callers over AudioSocket (patchbay + ffmpeg)" \
      org.opencontainers.image.licenses="Apache-2.0 AND GPL-3.0-or-later"

COPY --from=build /out/patchbay /patchbay
USER 65532:65532
ENTRYPOINT ["/patchbay"]

# patchbay itself: the last stage, so it is the default target (`docker
# build .`) and the bridge's ffmpeg never reaches it.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:516757106de35205efb70b91ba03f6f1298424a0678c603255cf2852a7000087

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
