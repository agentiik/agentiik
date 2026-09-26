# The runner's image, ghcr.io/agentiik/runner: the static agent the release ships as a binary, the
# static helper a script step is given at /agk/bin/agk, the certificates the agent verifies the
# API, the bus, the object store and the registry with, and nothing else.
#
# It is built from those binaries rather than compiling its own, so that the image and the bare
# metal install are one program and not two builds that could differ. The build context holds both
# for each architecture, built as cmd/agk-runner/static_test.go and cmd/agk-helper/static_test.go
# build and check them:
#
#   for arch in amd64 arm64; do
#     for cmd in agk-runner agk-helper; do
#       CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags='-s -w' \
#         -o $cmd-linux-$arch ./cmd/$cmd
#     done
#   done
#   docker buildx build --platform linux/amd64,linux/arm64 -f build/runner.Dockerfile \
#     --build-arg VERSION=0.2.0 --build-arg REVISION=$(git rev-parse HEAD) .
#
# .github/workflows/release.yml builds it so and pushes it for both architectures: as X.Y.Z and
# vX.Y.Z at a release tag, as latest too when that is the highest release, and as dev at every
# commit to main.
#
# Run as the Compose file runs it: as root, which serve drops, in the host's user namespace, with
# cap_drop ALL and cap_add CHOWN, FOWNER, DAC_OVERRIDE, SETUID and SETGID, the daemon socket
# mounted, and the work root mounted at the path it has on the host, since the daemon resolves
# every bind source there. serve gives the agent's directories to agentiik, takes the group that
# owns the socket, drops to agentiik and starts itself again, so neither a directory nor a group is
# prepared on the host or named in the Compose file. Nothing is mounted for secrets: a task's
# values are on a tmpfs volume of its own, which the daemon makes in memory and removes with the
# task.

# The certificates and setcap come from alpine:3.21, which is the one image the tests already name
# and pull, pinned to the minor for the reason the Go version is. This stage runs on the builder's
# own platform, since nothing it does depends on the target's: a file capability is the same bytes
# on either architecture, and running setcap under emulation would only be slower.
FROM --platform=$BUILDPLATFORM alpine:3.21 AS prepare

ARG TARGETARCH

RUN apk add --no-cache libcap-setcap

# The agent holds CAP_CHOWN, CAP_FOWNER and CAP_DAC_OVERRIDE as file capabilities, the way the
# systemd unit gives them with AmbientCapabilities. A process that sets its user from root to
# agentiik loses every capability it held, and so does one the runtime starts as agentiik, since
# the runtime sets no ambient ones; the file's are what the exec that follows grants again, within
# the bounding set cap_add leaves, and CAP_SETUID and CAP_SETGID are not among them. Without the
# three the agent refuses a remapped daemon, since it could not own a task's directory inside the
# range. The effective bit makes the kernel refuse the exec outright where the bounding set lacks
# one of the three, as cap_drop ALL alone leaves it.
COPY agk-runner-linux-${TARGETARCH} /out/usr/local/bin/agk-runner
RUN setcap cap_chown,cap_fowner,cap_dac_override=ep /out/usr/local/bin/agk-runner

# The account the agent runs as, by name as the page's Compose file and the unit name it, and at
# 65532 as every image of this project runs. serve started as root drops to it, and join run as
# root gives it runner.env and the key, and both find it here; scratch has no /etc/passwd.
#
# /tmp is where the standard library stages a file when nothing names another place, as the
# artifact store does before it knows a file's digest, and scratch has none.
RUN mkdir -p /out/etc && mkdir -m 1777 /out/tmp && \
    printf 'root:x:0:0:root:/:/sbin/nologin\nagentiik:x:65532:65532:Agentiik runner:/var/lib/agentiik:/sbin/nologin\n' > /out/etc/passwd && \
    printf 'root:x:0:\nagentiik:x:65532:\n' > /out/etc/group

FROM scratch

ARG TARGETARCH
ARG VERSION=unknown
ARG REVISION=unknown

# The OCI annotations every image published under ghcr.io/agentiik carries, as a brick must.
LABEL org.opencontainers.image.title="agk-runner" \
      org.opencontainers.image.description="The Agentiik runner: takes tasks from its pool and runs each brick as a container on the host's Docker daemon." \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/agentiik/agentiik" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later"

COPY --from=prepare /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=prepare /out/ /

# The helper where a bare metal host installs it too, runner.HelperPath. serve lays a copy down
# under the work root and binds that, since the daemon would not find a path of this image.
COPY agk-helper-linux-${TARGETARCH} /usr/local/lib/agentiik/agk-helper

# Root, by number, since what serve does before it serves takes root: giving the work root to
# agentiik, where Docker created it as root's, and taking the group of a socket whose number
# differs from host to host. serve never serves as root: it drops to 65532 before it asks the API
# or the daemon anything, or refuses the start. Run with --user 65532 instead, it serves as that
# account at once, and the socket's group is then the runtime's to give.
USER 0:0

# serve by default, so that the image runs the agent; join and version are given as the command
# instead. It binds no port, since a runner host opens none.
ENTRYPOINT ["/usr/local/bin/agk-runner"]
CMD ["serve"]
