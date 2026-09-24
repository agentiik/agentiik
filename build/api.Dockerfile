# The API's image, ghcr.io/agentiik/api: the static binary the release ships beside it, the
# certificates it verifies the database and the bus with, and nothing else.
#
# It is built from that binary rather than compiling one of its own, so that the image and the
# binary are one file and not two builds that could differ. The build context holds the binary for
# each architecture, built as cmd/agentiik-api/static_test.go builds and checks it:
#
#   for arch in amd64 arm64; do
#     CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags='-s -w' \
#       -o agentiik-api-linux-$arch ./cmd/agentiik-api
#   done
#   docker buildx build --platform linux/amd64,linux/arm64 -f build/api.Dockerfile \
#     --build-arg VERSION=0.2.0 --build-arg REVISION=$(git rev-parse HEAD) .

# The certificates come from alpine:3.21, which is the one image the tests already name and pull,
# pinned to the minor for the reason the Go version is. Every path the API opens leaves over TLS,
# and scratch has no roots to verify a certificate against. A private CA is mounted over this file,
# or added to it in an image built from this one.
FROM alpine:3.21 AS certificates

# The directories the object store and the bus identity are mounted at, made here since scratch has
# no mkdir, and owned by the user below. A named volume mounted over a path the image holds takes
# that path's owner and mode, so a volume mounted at the first is one the API can write objects in,
# and one mounted at the second is one bus-init can write the identity in, readable and writable by
# that user alone, as bus-init refuses any other.
RUN mkdir -p /var/lib/agentiik/objects && mkdir -m 700 /var/lib/agentiik/bus

FROM scratch

ARG TARGETARCH
ARG VERSION=unknown
ARG REVISION=unknown

# The OCI annotations every image published under ghcr.io/agentiik carries, as a brick must.
LABEL org.opencontainers.image.title="agentiik-api" \
      org.opencontainers.image.description="The Agentiik API: serves every route, the built-in object store and the secret providers." \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/agentiik/agentiik" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later"

COPY --from=certificates /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY agentiik-api-linux-${TARGETARCH} /agentiik-api
COPY --from=certificates --chown=65532:65532 /var/lib/agentiik /var/lib/agentiik

# A user that is not root, by number, since scratch has no /etc/passwd to name one in. The API
# writes nothing but to the object-store directory, which the installation mounts writable by this
# user, and listens on 8080, AGK_LISTEN's default, which a user that is not root may bind.
USER 65532:65532
EXPOSE 8080

# serve by default, so that the image runs the API; migrate, bus-init and bus-credential are given
# as the command instead.
ENTRYPOINT ["/agentiik-api"]
CMD ["serve"]
