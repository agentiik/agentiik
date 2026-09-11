# agentiik

The core, as one Go module: the graph evaluator, the container driver, the controller,
the HTTP API, the runner and the `agk` command line. They live together because the
evaluator and the driver have to stay importable libraries, which is what makes
`agk run --local` the same code path as a server run.

Images are published to `ghcr.io/agentiik/api`, `ghcr.io/agentiik/controller` and
`ghcr.io/agentiik/runner`, signed, and carry the same OCI annotations required of any
brick. The workflow format comes from [`agentiik/schemas`](https://github.com/agentiik/schemas),
pinned by version; nothing here redefines those shapes.

Nothing is implemented yet. What the code is written against is the specification at
<https://agentiik.github.io/docs>, and that is also where the documentation lives — this
README is the only one this repository keeps.

## Licence

AGPL-3.0-or-later, see [LICENSE](LICENSE). A brick is not a derivative work of this
engine; [LICENSING.md](https://github.com/agentiik/.github/blob/main/LICENSING.md) says so in as many words, and sets out why the
organisation's repositories are not licensed uniformly.

## Contributing

[CONTRIBUTING.md](https://github.com/agentiik/.github/blob/main/CONTRIBUTING.md). Contributions are accepted under the Developer
Certificate of Origin 1.1, with a `Signed-off-by` line, not under a contributor licence
agreement.
