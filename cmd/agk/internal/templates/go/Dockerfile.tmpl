# A static binary on scratch: no shell, no package manager, no libc, and the account the manifest
# declares. Run go mod tidy once before the first build, so that go.sum exists.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/brick .

FROM scratch
COPY brick.yaml /agk/brick.yaml
COPY --from=build /out/brick /usr/local/bin/brick
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/brick"]
