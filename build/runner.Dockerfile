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
# Run as the page's Compose sample runs it: as agentiik, in the group that owns the daemon socket,
# in the host's user namespace, with cap_drop ALL and cap_add CHOWN, FOWNER and DAC_OVERRIDE, and
# with the work root, the secrets tmpfs and /etc/agentiik mounted at the paths they have on the
# host, since the daemon resolves every bind source there.

# The certificates and setcap come from alpine:3.21, which is the one image the tests already name
# and pull, pinned to the minor for the reason the Go version is. This stage runs on the builder's
# own platform, since nothing it does depends on the target's: a file capability is the same bytes
# on either architecture, and running setcap under emulation would only be slower.
FROM --platform=$BUILDPLATFORM alpine:3.21 AS prepare

ARG TARGETARCH

RUN apk add --no-cache libcap-setcap

# The agent holds CAP_CHOWN, CAP_FOWNER and CAP_DAC_OVERRIDE as file capabilities, the way the
# systemd unit gives them with AmbientCapabilities. For a user that is not root, what cap_add
# grants the runtime's own process is lost at the exec of a program without file capabilities,
# since the runtime sets no ambient ones; the file's are what the exec keeps, within the bounding
# set cap_add leaves. Without them the agent refuses a remapped daemon, since it could not own a
# task's directory inside the range. The effective bit makes the kernel refuse the exec outright
# where the bounding set lacks one of the three, as cap_drop ALL alone leaves it.
COPY agk-runner-linux-${TARGETARCH} /out/usr/local/bin/agk-runner
RUN setcap cap_chown,cap_fowner,cap_dac_override=ep /out/usr/local/bin/agk-runner

# The account the agent runs as, by name as the page's Compose file and the unit name it, and at
# 65532 as every image of this project runs. join, run as root to write runner.env and the key,
# gives them to agentiik, and finds that account here; scratch has no /etc/passwd to find it in.
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

# A user that is not root, by number, so that a runtime that holds images to a numeric user can
# tell. serve refuses root anyway.
USER 65532:65532

# serve by default, so that the image runs the agent; join and version are given as the command
# instead. It binds no port, since a runner host opens none.
ENTRYPOINT ["/usr/local/bin/agk-runner"]
CMD ["serve"]
