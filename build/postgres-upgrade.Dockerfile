# The image that upgrades PostgreSQL's data between major versions, ghcr.io/agentiik/postgres-upgrade:
# the binaries of PostgreSQL 17 and 18 side by side, which pg_upgrade needs and no official image
# carries, and postgres-upgrade.sh as its entry point. The Compose file of agentiik/deploy runs it
# before PostgreSQL at every docker compose up, with the directory holding the cluster mounted at
# /data, and it upgrades /data/postgres where an older major version wrote it, keeping the old one.
#
# The build context is the directory holding the script, which is this one in the repository:
#
#   docker buildx build --platform linux/amd64,linux/arm64 -f build/postgres-upgrade.Dockerfile \
#     --build-arg VERSION=0.3.0 --build-arg REVISION=$(git rev-parse HEAD) build
#
# .github/workflows/release.yml builds it so and pushes it for both architectures: as X.Y.Z and
# vX.Y.Z at a release tag, as latest too when that is the highest release, and as dev at every
# commit to main. A later major version is one more package here: the script upgrades to the newest
# one the image carries, from any other it carries.

# Alpine 3.24 carries postgresql17 and postgresql18 both, and the official postgres:17-alpine and
# postgres:18-alpine are built on it too. Pinned to the minor for the reason every other alpine here
# is: the packages move with its patch releases, never to another major version.
FROM alpine:3.24

ARG VERSION=unknown
ARG REVISION=unknown

# The OCI annotations every image published under ghcr.io/agentiik carries, as a brick must.
LABEL org.opencontainers.image.title="postgres-upgrade" \
      org.opencontainers.image.description="Upgrades a PostgreSQL data directory from an older major version with pg_upgrade, keeping the old one beside it." \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/agentiik/agentiik" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later"

# Each version installs to /usr/libexec/postgresqlNN, where the script finds it. su-exec drops from
# root to postgres, which Alpine's packages create as uid and gid 70, the official image's own: the
# script renames directories as root and runs PostgreSQL as that account, which owns the data. The
# full ICU data, as the official image installs it, so that a database sorting with an ICU collation
# is analysed here as it will be read there.
RUN apk add --no-cache postgresql17 postgresql18 su-exec icu-data-full && \
    test "$(id -u postgres):$(id -g postgres)" = 70:70

# The locale the official image gives initdb, so that the new cluster starts with its defaults.
ENV LANG=en_US.utf8

COPY --chmod=0755 postgres-upgrade.sh /usr/local/bin/postgres-upgrade

# Root, by number: renaming a cluster beside it writes in the directory holding it, which is the
# person's or root's. PostgreSQL itself never runs as root, since the script runs it as postgres.
USER 0:0

ENTRYPOINT ["/usr/local/bin/postgres-upgrade"]
