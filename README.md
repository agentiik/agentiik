# agentiik

The core of Agentiik, as one Go module: the graph evaluator, the container driver, the controller, the task bus, the HTTP API, the state in PostgreSQL, and the `agk` command line.

The specification is the documentation at <https://agentiik.github.io/docs>; where it and this code disagree, the documentation is right. The workflow file, the brick manifest, the envelope and the task message are shapes owned by [`agentiik/schemas`](https://github.com/agentiik/schemas) and vendored under `internal/fixtures` at the version in `internal/fixtures/testdata/SCHEMAS_VERSION`.

## Packages

| Package | What it is |
| --- | --- |
| `agk` | The vocabulary: envelope, item, port, step, run, task, the `agk://` URI, the size limits and the exit-code table. |
| `artifact` | The content-addressed store, scoped per namespace, and the presigned URLs a runner reaches it with. |
| `brick` | The two edges of a container: what a step is given under `/agk/in/`, and what is collected from `/agk/out/`. |
| `schema` | JSON Schema 2020-12 for what a user writes: workflow inputs and brick parameters. |
| `graph` | The evaluator. It decides what runs next and never executes anything. |
| `driver` | One task as one container, through the Docker Engine API. The only package that reaches a daemon. |
| `controller` | The one active decider: elected, fenced, woken by the API, and sweeping anyway. |
| `bus` | The task bus on NATS JetStream, and the short-lived credentials a runner reaches it with. |
| `db` | PostgreSQL: the schema, its migrations, and a handle that makes the namespace impossible to forget. |
| `api` | The HTTP boundary, where every request is authorised, deny by default. |
| `secret` | The built-in secret store, on envelope encryption. |
| `version` | A stored workflow version turned back into a graph. |
| `cmd/agk` | The command line. The loop of `agk run --local` is `cmd/agk/internal/local`. |
| `cmd/agk-helper` | The static helper bound read-only at `/agk/bin/agk` for a script step. |

Under `internal/`: CEL (`expr`), the Engine API client and a fake daemon (`docker`, `dockertest`), a throwaway test database (`dbtest`), credential minting (`token`), identifiers (`ulid`) and the vendored schema fixtures (`fixtures`). Each package's doc comment, in `doc.go` where there is one, says what it is for and what it is not.

The evaluator and the driver stay libraries with no server, bus or database behind them, so that `agk run --local` takes the same code path as a server run instead of a second one that drifts. `graph/boundary_test.go` and `driver/boundary_test.go` hold that.

## Dependencies

Each one records its reason in `go.mod`.

- `cel.dev/cel-go`: the expression language, reached from `graph` alone.
- `github.com/goccy/go-yaml`: YAML 1.2, because YAML 1.1 reads the trigger key `on:` as `true`.
- `github.com/jackc/pgx/v5`: PostgreSQL, since the controller's advisory lock and `LISTEN` need a pinned connection that `database/sql` cannot give.
- `github.com/nats-io/nats.go`, `github.com/nats-io/jwt/v2` and `github.com/nats-io/nkeys`: the bus and its credentials. `github.com/nats-io/nats-server/v2` is a test dependency and ships in nothing.
- `github.com/santhosh-tekuri/jsonschema/v6`: JSON Schema 2020-12.

The Docker Engine API is spoken with the standard library.

## Status

`agk run --local` runs a whole workflow on one machine, which was v0.1.0, and `agk validate`, `agk graph` and `agk brick test` work beside it. v0.2.0, a server running it instead, is in progress: the controller, the bus, the API and the secret store exist as packages and `agk push` sends a version, but there is no server binary yet, so `agk run` without `--local`, `login`, `whoami`, `share`, `grants`, `logs` and `brick init` refuse and say why. The [roadmap](https://agentiik.github.io/docs/roadmap) has the rest, and [CHANGELOG.md](CHANGELOG.md) what each release shipped.

## Building and testing

```
go build ./...
go vet ./...
go test ./... -race
gofmt -l .
```

CI runs the same four, and `gofmt -l .` passes only by printing nothing. Tests that need a service skip without one:

- PostgreSQL: set `AGENTIIK_TEST_DATABASE_URL` to a superuser URL. The tests create their own database and the unprivileged role the code connects as.
- NATS: set `AGENTIIK_TEST_BUS_URL` to a server started with `-js`.
- Docker: found through `DOCKER_HOST`, then the usual socket paths. The tests run their containers from `alpine:3.21`, and one that finds it missing skips rather than pull it. It is pulled from Docker Hub only as the base of a fixture build, or by `agk run --local` in the milestone test, and a test that comes before either still skips, so `docker pull alpine:3.21` first. `AGENTIIK_TEST_REQUIRE_DOCKER=1`, which CI sets, fails a test that would otherwise skip for want of the daemon or the image.

`cmd/agk/milestone_test.go` is the proof of v0.1.0: a workflow with a fan-out and a merge, run twice against the real daemon, producing the same envelopes.

A plain `go build` gives an `agk` with no embedded helper; `--helper <path>` or `$AGK_HELPER` supplies one to a script step, and `cmd/agk/internal/helper/bin/README.md` says how to build the binaries a release embeds.

## Licence

Copyright 2026 François Rousselet. AGPL-3.0-or-later, see [LICENSE](LICENSE). A brick is not a derivative work of the engine, and [LICENSING.md](https://github.com/agentiik/.github/blob/main/LICENSING.md) explains why the organisation's repositories are not licensed alike.

## Contributing

See [CONTRIBUTING.md](https://github.com/agentiik/.github/blob/main/CONTRIBUTING.md). Contributions come under the Developer Certificate of Origin 1.1, with a `Signed-off-by` line, rather than a contributor licence agreement.
