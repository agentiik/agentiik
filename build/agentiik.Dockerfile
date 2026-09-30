# Agentiik's image, ghcr.io/agentiik/agentiik: the static binaries of the API, which carries the web
# console, and of the controller, the certificates they verify the database, the bus and the audit
# log's sink with, and nothing else.
#
# One image for both, run as two containers by their commands, since the controller never mounts the
# master key the API reads, and a container of its own is what keeps it from reading it:
#
#   docker run ghcr.io/agentiik/agentiik                        agentiik-api serve, the default
#   docker run ghcr.io/agentiik/agentiik agentiik-api init
#   docker run ghcr.io/agentiik/agentiik agentiik-controller
#
# It is built from the binaries rather than compiling its own, so that the image and the binaries are
# the same files and not two builds that could differ. The build context holds each binary for each
# architecture, built as cmd/agentiik-api/static_test.go and cmd/agentiik-controller/static_test.go
# build and check them, with the console built into console/dist first, as console/README.md says,
# since the API carries the console that is there when it is compiled and none where it is not:
#
#   (cd console && npm ci && AGK_VERSION=0.6.0 npm run build)
#   for arch in amd64 arm64; do
#     for cmd in agentiik-api agentiik-controller; do
#       CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags='-s -w' \
#         -o $cmd-linux-$arch ./cmd/$cmd
#     done
#   done
#   docker buildx build --platform linux/amd64,linux/arm64 -f build/agentiik.Dockerfile \
#     --build-arg VERSION=0.6.0 --build-arg REVISION=$(git rev-parse HEAD) .
#
# .github/workflows/release.yml builds it so and pushes it for both architectures: as X.Y.Z and
# vX.Y.Z at a release tag, as latest too when that is the highest release, and as dev at every
# commit to main.

# The certificates come from alpine:3.21, which is the one image the tests already name and pull,
# pinned to the minor for the reason the Go version is. Every path the API and the controller open
# leaves over TLS, and scratch has no roots to verify a certificate against. A private CA is mounted
# over this file, or added to it in an image built from this one.
FROM alpine:3.21 AS certificates

# The directories the object store and the bus identity are mounted at, and the one a git push is
# spooled to while it is checked, made here since scratch has no mkdir, and owned by the user below.
# A named volume mounted over a path the image holds takes that path's owner and mode, so a volume
# mounted at the first is one the API and the controller can write objects in, and one mounted at
# the second is one bus-init can write the identity in, readable and writable by that user alone, as
# bus-init refuses any other.
RUN mkdir -p /var/lib/agentiik/objects /var/lib/agentiik/tmp && mkdir -m 700 /var/lib/agentiik/bus

FROM scratch

ARG TARGETARCH
ARG VERSION=unknown
ARG REVISION=unknown

# The OCI annotations every image published under ghcr.io/agentiik carries, as a brick must.
LABEL org.opencontainers.image.title="agentiik" \
      org.opencontainers.image.description="Agentiik: the API, which serves the web console, and the controller, each run as a container of its own by its command." \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/agentiik/agentiik" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later"

COPY --from=certificates /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY agentiik-api-linux-${TARGETARCH} /usr/local/bin/agentiik-api
COPY agentiik-controller-linux-${TARGETARCH} /usr/local/bin/agentiik-controller
COPY --from=certificates --chown=65532:65532 /var/lib/agentiik /var/lib/agentiik

# Where a command names its program, and the one place the image holds programs in. scratch sets no
# PATH, and a runtime that supplied none of its own would find neither.
ENV PATH=/usr/local/bin

# A user that is not root, by number, since scratch has no /etc/passwd to name one in. serve writes
# nothing but to the object-store directory, bus-init and bus-credential to the bus identity's, and
# the controller to the object-store directory alone, each of which the installation mounts writable
# by this user. serve listens on 8080, AGK_LISTEN's default, which a user that is not root may bind;
# the controller binds no port.
USER 65532:65532
EXPOSE 8080

# Where a git push's pack is written while it is unpacked and judged, up to 2 GiB, and removed once
# the push is answered: scratch has no /tmp, and a pack is too large to hold in memory. It is the
# container's own layer rather than a volume, since nothing in it outlives the request.
ENV TMPDIR=/var/lib/agentiik/tmp

# No entrypoint, so that the command names the program as well as what it does: the API serves when
# none is given, and init, migrate, bus-init, bus-credential, health and the controller are given as
# the command instead.
CMD ["agentiik-api", "serve"]
