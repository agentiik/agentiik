# The static helper, as the release build puts it here

This directory carries the binaries of `cmd/agk-helper`, built for the architectures a
container may run on natively:

    agk-helper-linux-amd64  built as  agk-linux-amd64
    agk-helper-linux-arm64  built as  agk-linux-arm64

Each is built `CGO_ENABLED=0 GOOS=linux GOARCH=<arch>`, so that it is statically linked and
can be mounted read-only at `/agk/bin/agk` inside an image this project does not control.

    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o cmd/agk/internal/helper/bin/agk-linux-arm64 ./cmd/agk-helper
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o cmd/agk/internal/helper/bin/agk-linux-amd64 ./cmd/agk-helper

This file is committed and the binaries are not. `//go:embed` refuses a directory it matches
nothing in, so a build of this module on a machine that has never built the helper has to find
something here; this is that something. A build that skipped the helper stage therefore
carries no binary, `Extract` answers that there is none, `driver.Policy.Helper` stays empty and
the driver binds nothing, which is what the page means by "a convenience, never a requirement".
