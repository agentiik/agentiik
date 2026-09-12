// Command agk-helper is the static binary mounted read-only at /agk/bin/agk. Inside a
// container it is spelled agk, and the three verbs are the three the documentation names:
// agk items, agk emit and agk attach.
//
// It is a separate program and not a mode of the agk command line. Two reasons, and the
// second is the serious one. It has to be linux for the container's architecture and
// statically linked, so on a macOS laptop it can never be the same file the operator is
// typing, which means a second build whatever else is decided. And it is mounted inside an
// image this project does not control: building it out of cmd/agk would put a Docker Engine
// API client, a CEL compiler, a JSON Schema evaluator and a YAML reader inside every
// container that runs a script step. There is no socket in there to reach, so it could not
// use the client, but the rule that the daemon socket is never mounted inside a brick is not
// honoured by shipping the client in beside the brick and trusting the absence of the
// socket.
//
// It is a program a script invokes and never a library a brick links. That is deliberate:
// the core is AGPL, and "a brick is not a derivative work of the engine" survives a mount
// of a separate executable in a way it would not survive a Go API that a brick imported. So
// this grows a command line and never a package surface.
//
// # What it may link, and this is a test rather than a claim
//
// Package agk and the standard library. Nothing else. Package agk reaches nothing outside
// the module but internal/ulid, which is itself the standard library, so the closure is the
// standard library plus two packages of this module.
//
// Not brick, which names the contract's paths but pulls a YAML parser in behind them. The
// three paths are spelled as constants here instead, and a test in this package imports
// brick and asserts they are equal, so the duplication is held by a test and the shipped
// binary's closure stays clean.
//
// Not net, net/http or anything that dials: this reads files the container already has and
// writes files the container already owns, and a binary with a socket in it mounted into
// every sandbox is a thing somebody will one day point at. Not os/exec. Not os/user, because
// scratch has no /etc/passwd. None of graph, driver, schema, internal/docker or
// internal/expr: those are the engine, and the engine has no business inside the thing it is
// running.
//
// A test reads this package's whole non-test import closure and fails it on every one of
// those, on the precedent graph/boundary_test.go already set. A second test builds the
// binary and asserts through debug/elf that the result is a linux ELF for the expected
// machine with no PT_INTERP, which is what static means spelled as something a machine can
// check. CGO_ENABLED=0, so it runs in scratch, in distroless, in musl alpine and in glibc
// debian alike, and a dynamic loader it cannot find is an exec that fails with no message
// anybody can read.
//
// # What it must not need
//
// No daemon, no socket, no network, no artifact store, no credential, no grant, no registry,
// no configuration file, no knowledge of the graph, no shell, no writable root filesystem,
// no resolvable user account, and no file outside /agk. Everything it needs is already in
// the container: the envelope at /agk/in/<port>/envelope.json, the writable /agk/out, and
// AGK_RUN_ID, AGK_STEP, AGK_ATTEMPT and AGK_OUT_PORTS. It never uploads anything.
//
// A missing variable is refused naming the variable, because an envelope whose meta
// disagrees with what the container was given is refused by brick.Collect anyway, and the
// refusal should come from the tool that could still have been told.
//
// # The three verbs
//
//	agk items [--port <port>]
//
// Writes the items of the input envelope as JSON Lines, one item per line, read through
// agk.Decode. That is what makes the documented pipe work: agk items | jq -r
// '.data.vat_number' gives one value per item. The port is implied when one is mounted and
// named when several are, and a refusal lists the ports it found rather than guessing.
//
//	agk emit <port> [--from <file>|-] [--filter <expr>] [--id <field>]
//
// Writes /agk/out/ports/<port>.json as a valid envelope through agk.Envelope.Encode, with
// meta taken from the environment the container was given. A JSON array becomes its
// elements, a JSON Lines file becomes its lines, an object becomes one item, and a scalar is
// refused because data is an object and wrapping one would invent the key it sits under,
// which is the reading graph already took for a port the inputs keyword feeds.
//
// --id names the field an item's identity is derived from. Without it the identity is minted
// through agk.NewItem, which is correct and is also the one thing that makes a script step
// produce different envelopes on an identical rerun, so a script that wants to be
// reproducible names a field.
//
// A second emit on the same port appends to what is there. A script that emits in a loop is
// the ordinary case, and a silent overwrite would lose the earlier batch.
//
//	agk attach <file> --port <port> [--item <id>|--all] [--name <name>] [--media-type <type>]
//
// Copies the file under /agk/out/files/, hashes it, and adds the five-member files[] entry
// to the item, with the uri written as agk://run/$AGK_RUN_ID/$AGK_STEP/<port>/<name>. That
// is exactly the address the driver will verify the digest of when it uploads, so the helper
// states a fact and the runner is still the one that checks it: an entry that disagrees with
// the bytes is refused rather than carried.
//
// It refuses rather than guesses: a port with several items and no --item, a port whose file
// does not exist yet, because agk emit writes it first, and a name that is not one path
// segment.
//
// # --filter, which is a reading and not a quotation
//
// The documentation's example writes --filter '.valid' and --filter '.valid | not'. It does
// not say the expression is jq, and it says in the same breath that this helper "is a
// convenience, never a requirement; jq and a redirect do the same job".
//
// So --filter accepts those two forms, a field path and a field path piped into not, and
// refuses anything else naming what it supports and pointing at jq. It is not called jq
// anywhere in its help or its refusals, because a subset parser answering to that name would
// be the worst of the available answers: it would promise a language and deliver two
// expressions of it.
//
// The alternative was taking a pure-Go jq as a dependency, which would put a full
// expression evaluator inside every container that runs a script step, for a convenience the
// page itself says is never required. That is a decision worth raising rather than taking
// quietly: if full jq in --filter is wanted, the honest route is that dependency recorded in
// go.mod with its reason, or a pull request against the page dropping --filter, and which of
// the three it is belongs to François.
//
// # How it is built, in one place
//
// Two binaries, one per architecture a brick might be, each statically linked and each named
// for the platform it is for:
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o agk-helper-linux-amd64 ./cmd/agk-helper
//	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o agk-helper-linux-arm64 ./cmd/agk-helper
//
// CGO_ENABLED=0 is what makes it static. The other two are about what gets carried: -s -w
// drops the DWARF and the symbol table, a third of the file, which no debugger will ever
// attach to inside an ephemeral container, and a Go panic still prints its function names and
// line numbers without them, because the runtime walks a table of its own that neither flag
// touches. -trimpath keeps the build machine's directory layout out of a binary that is bound
// into images this project does not control, and makes the two builds reproducible.
//
// Those are the two names cmd/agk/internal/helper embeds, so the release build writes them
// into its bin/ and a build that skipped this stage embeds nothing, binds nothing, and is the
// runner with no helper to offer that driver.Policy.Helper already documents. The same two
// commands are what static_test.go runs, so the property the release relies on is the
// property the test checked rather than a flag somebody remembered to pass.
//
// # Layout
//
//	main.go      the three verbs and the dispatch
//	items.go     agk items
//	emit.go      agk emit
//	attach.go    agk attach
//	filter.go    the two forms, and the refusal that names them
//	contract.go  the paths and the variables this side of the mount, as constants, held
//	             equal to brick's and to the driver's by a test
//	envelope.go  reading one, writing one whole, and what two items may not share
//
// Every verb is handed the directory it works under, the two streams it writes on, standard
// input, the environment and the clock, so that a test runs it
// against a temporary tree laid out as /agk/in and /agk/out with no Docker in reach: all
// three are file reading and file writing, and a program whose only input is a directory and
// an environment can be tested as one. Two tests need a daemon and skip without one. The
// first builds an image with nothing in it at all, FROM scratch carrying this one file, and
// runs the three verbs in it, which is what static was for. The second goes through the
// driver, so that the mount, the environment table and the collection are the real ones
// rather than this package's reading of them.
//
// # Two things the page and the merged code disagree about, raised rather than patched
//
// The verb is spelled agk, and nothing puts /agk/bin on PATH. The environment table does not
// name the variable and the driver does not set it, so a step written exactly as the page
// writes it, agk items | jq -r '.data.vat_number', fails with "agk: not found" until either
// the driver prepends /agk/bin to PATH for a script step or the page writes the path out.
// Both are one line, and which of the two it is belongs to François; the real test here
// carries the PATH line with that reason written beside it, so the documented spelling is
// what is under test.
//
// And the roadmap says this is mounted "into every container" while the driver binds it for a
// script step alone. The driver's reading holds: the page's sentence lives in
// #running-a-script-instead-of-a-brick, a brick is an image that honours the contract on its
// own, and putting a binary of ours inside one would be an Agentiik library after all. That
// is worth one line on the page rather than a change to merged code.
package main
