# The controller's image, ghcr.io/agentiik/controller: the static binary the release ships beside
# it, the certificates it verifies the database and the bus with, and nothing else.
#
# It is built from that binary rather than compiling one of its own, so that the image and the
# binary are one file and not two builds that could differ. The build context holds the binary for
# each architecture, built as cmd/agentiik-controller/static_test.go builds and checks it:
#
#   for arch in amd64 arm64; do
#     CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags='-s -w' \
#       -o agentiik-controller-linux-$arch ./cmd/agentiik-controller
#   done
#   docker buildx build --platform linux/amd64,linux/arm64 -f build/controller.Dockerfile \
#     --build-arg VERSION=0.2.0 --build-arg REVISION=$(git rev-parse HEAD) .

# The certificates come from alpine:3.21, which is the one image the tests already name and pull,
# pinned to the minor for the reason the Go version is. Every path the controller has leaves over
# TLS, and scratch has no roots to verify a certificate against. A private CA is mounted over
# this file, or added to it in an image built from this one.
FROM alpine:3.21 AS certificates

# The directory the object store is mounted at, made here since scratch has no mkdir, and owned by
# the user below. A named volume mounted over a path the image holds takes that path's owner, so a
# volume mounted here is one the controller can write in, where one mounted anywhere else is
# root's and every task's inputs would be refused.
RUN mkdir -p /var/lib/agentiik/objects

FROM scratch

ARG TARGETARCH
ARG VERSION=unknown
ARG REVISION=unknown

# The OCI annotations every image published under ghcr.io/agentiik carries, as a brick must.
LABEL org.opencontainers.image.title="agentiik-controller" \
      org.opencontainers.image.description="The Agentiik controller: leads by advisory lock, sweeps, consumes results and publishes tasks." \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/agentiik/agentiik" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later"

COPY --from=certificates /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY agentiik-controller-linux-${TARGETARCH} /agentiik-controller
COPY --from=certificates --chown=65532:65532 /var/lib/agentiik/objects /var/lib/agentiik/objects

# A user that is not root, by number, since scratch has no /etc/passwd to name one in. The
# controller writes nothing but to the object-store directory, which the installation mounts
# writable by this user, and binds no port.
USER 65532:65532

ENTRYPOINT ["/agentiik-controller"]
