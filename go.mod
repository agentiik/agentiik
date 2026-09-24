module github.com/agentiik/agentiik

go 1.27

require (
	// CEL, used by package internal/expr alone and reached only from package graph.
	// The documentation names the language and names the reason: CEL "evaluates in
	// linear time, is mutation free, and not Turing-complete", which is what lets a
	// controller serving every namespace evaluate a tenant's conditions in its own
	// process. This is the implementation the specification is written against, it
	// compiles against a declared environment, which is how the exposed-context table
	// is enforced by the compiler rather than by a guard, and it carries the escaped
	// identifier syntax that gives inputs.in.count a spelling at all.
	cel.dev/cel-go v0.32.0
	// YAML 1.2, used to read the two documents of the language: the workflow entry point
	// in package graph and the brick manifest in package brick. The version of YAML is
	// the reason for the choice rather than the API. A YAML 1.1 parser reads the bare key
	// on: as the boolean true, and on: is how the language spells the trigger block, so
	// such a parser fails every workflow that carries a trigger. It also refuses a
	// duplicate key without being asked, and keeps the line and column a value was
	// written at, which is what lets a refusal point at the text the author wrote.
	github.com/goccy/go-yaml v1.19.2
	// PostgreSQL, used by package db alone. The documentation names the database and
	// names two things database/sql cannot express: the controller is elected by a
	// session-level PostgreSQL advisory lock, held for the life of a connection, and it
	// holds LISTEN while the API wakes it with NOTIFY. A pooled database/sql connection
	// is handed back between statements, which drops the lock and carries the LISTEN to
	// the next caller; this driver pins one. It also takes parameters natively, which is
	// what keeps a namespace a parameter and never a string interpolated into SQL.
	github.com/jackc/pgx/v5 v5.11.0
	// NATS credentials, used by package bus alone, to mint the "short-lived token minted by
	// the API" that a runner reaches the bus with. This is the library the server verifies
	// with, so the claims this module writes and the claims the far end reads are the same
	// structure rather than two readings of a specification. Hand-rolling a JWT for somebody
	// else's verifier is the kind of clever that is wrong once and wrong for ever.
	github.com/nats-io/jwt/v2 v2.8.2
	// The NATS server, used by package bus in tests alone, and never linked into anything
	// this module ships. What a runner credential may do is a permission set an installation
	// depends on, and asserting the claims this module writes would be asserting its own
	// JSON: the property that matters is what a server does with them. The first version of
	// that set allowed $JS.API.> and let a runner create a stream, which is how this
	// dependency came to be here.
	github.com/nats-io/nats-server/v2 v2.15.0
	// NATS, used by package bus alone. The documentation names the broker and names the two
	// properties it is chosen for: "NATS JetStream, with WorkQueue retention, where a
	// message is removed as soon as it has been consumed, which is precisely what work
	// distribution needs", and pull consumers, so that "a runner asks for a batch of tasks
	// when it has room, which makes distribution naturally proportional to each host's real
	// capacity without the controller having to model load". Neither is something a plain
	// subject or a database table gives, and the deployment chapter substitutes SQS on one
	// profile precisely because the contract this fills is narrow enough to state.
	github.com/nats-io/nats.go v1.53.1
	// The NATS key pairs, used by package bus alone. A user credential is an Ed25519 key
	// pair and a JWT naming its public half, and this is what mints one and what signs with
	// an account key. It was already here as an indirect dependency of the client.
	github.com/nats-io/nkeys v0.4.16
	// TOML, used by package driver alone, to read /etc/agentiik/runner.toml. The
	// documentation names the format and writes the file's [hooks] block with arrays of
	// strings and a table, which a line reader cannot parse. A runner reads the file
	// strictly, and this decoder refuses a key its target does not declare and says on
	// which line, which is what turns a misspelled setting into a refusal rather than a
	// default nobody chose. It takes no dependency of its own.
	github.com/pelletier/go-toml/v2 v2.4.3
	// JSON Schema 2020-12, used by package schema, and in tests by packages bus and api,
	// which hold what they put on the wire to the vendored wire.schema.json rather than to a
	// copy of it written in Go. The workflow language defines an input's schema as a 2020-12
	// document, and this implementation is the draft itself rather than an older one; it
	// takes a custom loader, which is how a $ref is resolved against the commit's tree and
	// refused when it leaves it; and it returns a structured error whose keyword and
	// instance location are what a refusal message names.
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3
)

require (
	cel.dev/expr v0.25.1 // indirect
	github.com/antithesishq/antithesis-sdk-go v0.8.0-default-no-op // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/klauspost/compress v1.20.0 // indirect
	github.com/minio/highwayhash v1.0.4 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	github.com/rogpeppe/go-internal v1.16.0 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/exp v0.0.0-20240823005443-9b4947da3948 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	golang.org/x/time v0.16.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20240826202546-f6391c0de4c7 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240826202546-f6391c0de4c7 // indirect
	google.golang.org/protobuf v1.36.10 // indirect
)
