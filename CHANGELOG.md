# Changelog

The releases of `agentiik`.

Every repository of the project carries the same version and is tagged at the same moment, even where nothing changed, so an entry here may say that nothing was built. [Versioning](https://agentiik.github.io/docs#versioning) sets out why, and what a version promises before and after `1.0.0`.

`0.y.z` promises nothing beyond itself: what a release here describes may be gone in the next one.

## v0.1.1, 2026-09-13

This file, and nothing else.

`v0.1.0` was tagged before its changelog was written, and the fix for that is not to move the tag. Within minutes of the push, `sum.golang.org` had recorded the tagged commit of `agentiik` and `bricks` in a public append-only log and `proxy.golang.org` had cached it, so moving `v0.1.0` would have left `go get` serving the old code for ever and made a direct fetch fail with a checksum mismatch that reads as a supply-chain attack. A tag is a name somebody else pins, and a name that quietly comes to mean something else is worse than a second name.

So `v0.1.0` stays exactly where it is, describing exactly what it shipped, and this release adds the description. Every repository gets it at the same version on the same day, as every release here does. From now on a version's entry is merged before its tag is placed, which is written down in the conventions the documentation fixes.

## v0.1.0, 2026-09-12

The engine, as one Go module, and the first release in which a workflow runs.

**What it does.** `agk run --local` runs a whole workflow against the Docker daemon of the machine it is typed on, with no controller, no task bus and no database. A multi-step workflow with a fan-out and a merge runs end to end, and running it again on the same inputs produces the same envelopes. That sentence is the milestone, and it is a test rather than a claim: `cmd/agk/milestone_test.go` builds three images, runs six containers twice against the real daemon, and compares what came back.

**The libraries.**

- `agk` is the vocabulary the documentation uses and the rules it states about it: the envelope and its metadata, items, files, ports, steps, run identifiers, the `agk://` URI, the four size limits, the two refusals they produce, and the exit-code table read off the code a container exited with.
- `artifact` is the content-addressed store. A logical `agk://run/<run>/<step>/<port>/<name>` resolves to `sha256/<digest>`, identical bytes are stored once, and deduplication is scoped per namespace so that two namespaces never share an object.
- `brick` is the two edges of the container as directories: what a step is given under `/agk/in/`, what it leaves under `/agk/out/`, the spill of a value above `inline_max_bytes` into the store, and the manifest an image carries at `/agk/brick.yaml`.
- `schema` validates a workflow's declared inputs as JSON Schema 2020-12, with references resolved against the repository tree the run pinned and never against the network.
- `graph` is the evaluator: it reads the entry point, resolves includes, `extends` and `defaults`, refuses cycles and edges onto ports no step declares, and then answers what may run next. The barrier, the four merge strategies, the fan-out under `max_parallel` and `fail_fast`, `if` and `when`, `retry` and `continue_on_error`, and the run verdict all live here rather than in the driver.
- `driver` runs the container and is the only package that may reach a daemon. One task is one container: resolve and pull, read the manifest, refuse a root user before anything is created, prepare the mounts and the `AGK_*` environment, apply the settings every container gets, give the task a network of its own, open the wait before the start so an exit cannot fall between two calls, enforce the timeout as SIGTERM then SIGKILL, read the exit code against the table, and collect one envelope per declared port with artifacts uploaded and secrets masked out of the log.

**The command line.** `agk validate`, `agk graph`, `agk run --local` and `agk brick test` do their work; `login`, `whoami`, `push`, `share`, `grants`, `logs` and `brick init` each refuse naming what is missing, because a verb the documentation lists and the binary does not know is a binary that looks broken. Five exit codes, each a different thing to do next. `/agk/bin/agk` is a static helper mounted read-only into every container, with `agk items`, `agk emit` and `agk attach`, and it is a convenience rather than a requirement.

**What it deliberately does not have yet.** No controller, no bus, no database, no HTTP API, no runner and no identity: a run is local or it does not happen. `network: egress` is refused rather than opened, because the proxy that would enforce a step's `egress.allow` list does not exist and opening the network and calling it filtered would be worse than refusing. On a platform with no tmpfs the runner can reach, a secret value is written into the task's working directory and removed with the container, and the run says so out loud.
