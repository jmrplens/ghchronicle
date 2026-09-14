# syntax=docker/dockerfile:1@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32

# Every base image is pinned by version and by digest. The version says what a
# reader is looking at; the digest is what makes a rebuild of the same commit
# pull the same bytes, because a tag can be pushed again under the same name.
#
# The build stage runs on the machine doing the build, whatever the target, and
# cross-compiles for the platform being built. Without --platform a multi-arch
# build would run the whole Go toolchain under emulation for every foreign
# architecture, which is slower by an order of magnitude and buys nothing: the
# binary is pure Go and compiles the same either way.
FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine3.24@sha256:cf6fca6641884b8433441b2b0652976f975e1d0fdd26d177eaaf8596087f3125 AS build

WORKDIR /src

# Dependencies change far less often than the source, so resolving them in
# their own layer keeps rebuilds cheap. The cache mounts keep the module and
# build caches across builds without writing either into a layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

# All three are optional. version.go embeds the VERSION file, so an image built
# with no build arguments at all still reports the release it was cut from
# rather than "dev"; these add the commit and the date, which only the caller
# knows. `make docker-build` passes them.
ARG VERSION=""
ARG COMMIT=""
ARG BUILD_DATE=""

# Set by BuildKit from --platform, or from the builder's own platform when the
# build names none.
ARG TARGETOS
ARG TARGETARCH

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${BUILD_DATE}" \
    -o /ghchronicle ./cmd/ghchronicle

# Distroless ships no shell and no package manager, so an attacker who reaches
# code execution here has nothing to pivot with; the `static` variant suffices
# because CGO is disabled above. The `nonroot` tag bakes in uid 65532, which
# keeps the container off root even when the orchestrator sets no
# securityContext of its own. Debian 13 because it is the only release the
# distroless project still publishes: the -debian12 tags are no longer updated.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7

# ARG is stage-scoped, so the three have to be declared again to reach the
# labels below.
ARG VERSION=""
ARG COMMIT=""
ARG BUILD_DATE=""

# What a registry shows about the image, and what links it back to the commit
# it was built from. The released images get the same set from the labels in
# .goreleaser.yaml, since docker/Dockerfile.goreleaser has no LABEL block of its
# own; reword both together.
LABEL org.opencontainers.image.title="ghchronicle" \
    org.opencontainers.image.description="Collects everything GitHub will tell you about an account, and keeps it with the date it happened" \
    org.opencontainers.image.source="https://github.com/jmrplens/ghchronicle" \
    org.opencontainers.image.url="https://github.com/jmrplens/ghchronicle" \
    org.opencontainers.image.documentation="https://github.com/jmrplens/ghchronicle/tree/main/docs" \
    org.opencontainers.image.licenses="MIT" \
    org.opencontainers.image.vendor="jmrplens" \
    org.opencontainers.image.version="${VERSION}" \
    org.opencontainers.image.revision="${COMMIT}" \
    org.opencontainers.image.created="${BUILD_DATE}"

COPY --from=build /ghchronicle /ghchronicle

# The `nonroot` user of the base image, by number rather than by name. A name is
# resolved against the image's /etc/passwd, which the host cannot see, so a
# Kubernetes pod with runAsNonRoot refuses to start a container whose user is
# not numeric: it has no way to prove the name is not root.
USER 65532:65532

# Prometheus exporter.
EXPOSE 9605

ENTRYPOINT ["/ghchronicle"]
