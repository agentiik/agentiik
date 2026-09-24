// Package docker is the whole conversation with the Docker Engine API over its unix
// socket, written with the standard library, and the only place the wire shapes exist.
//
// It is internal on the precedent internal/expr set for CEL and internal/ulid for the
// identifier mint: only the driver needs an Engine API client, so only the driver pays
// for one, and nothing outside this module comes to depend on the wire types of a version
// we pin. It holds no rule, no contract and no envelope. Keeping it apart is what lets
// every rule of the driver be tested with no Docker anywhere.
//
// # Why the standard library and no dependency
//
// Nothing is recorded in go.mod, because nothing is added to it. The reason is recorded
// here instead, so that the next reader reaching for the official client finds it before
// the work.
//
// What is actually needed is fourteen endpoints of JSON over a unix socket, one hijacked
// stream, two long polls and an eight-byte demultiplexer. That is net/http with a
// DialContext, encoding/json, about sixty lines of hijack (write the request onto a
// net.Conn with Request.Write, read it back with http.ReadResponse, keep the bufio.Reader
// for the stream) and about thirty of frame demultiplexing. It is small, and it is all
// code we would have to understand anyway.
//
// github.com/docker/docker/client is not a client library, it is the daemon's own type
// tree re-exported. Importing it puts the moby module in go.sum for everyone who builds
// any part of this repository, and with it OpenTelemetry's trace, metric and sdk modules
// and the net/http instrumentation, containerd/log, sirupsen/logrus,
// opencontainers/image-spec, distribution/reference, docker/go-connections,
// docker/go-units, moby/term and morikuni/aec. A module whose root doc.go says the
// evaluator and the driver are libraries importable with no server behind them should not
// make agk run --local on a laptop link somebody else's tracing and logging stack.
// agk/stdlib_test.go and graph/boundary_test.go already say what this project thinks of
// paying for a dependency it does not reach, and the second of them refuses
// github.com/docker by name.
//
// Two properties of this particular group settle it rather than the size argument alone.
// The settings table of #settings-applied-to-every-container is written in HostConfig's
// own field names, so the struct the driver marshals is the table, readable next to it,
// with no translation layer between what the page says and what goes on the wire. A
// library's HostConfig carries some ninety fields, of which this product sets twelve and
// deliberately refuses several others, and transcribing the twelve is clearer about what
// the driver does and does not set than passing a struct with seventy-eight zero values
// in it. And the three places the official client would have saved work are the three
// places we need behaviour it does not offer: the pull inspected message by message, the
// wait opened before start, and a demultiplexer whose standard error passes through the
// masker before anything is written.
//
// # Why the version is negotiated and not pinned
//
// The documentation names v1.56 and that is the ceiling here, written once as Ceiling.
// It is not a path prefix compiled in, because a hard pin refuses to run where agk run
// --local has to run: Docker Desktop on the author's own machine answers /_ping with API
// version 1.55 and refuses every path under /v1.56, and its socket is not at
// /var/run/docker.sock either. So the client pings once, speaks min(Ceiling, what the
// daemon offers), and refuses below Floor naming both versions.
//
// Floor is a policy choice and not a technical one. The newest endpoint this package
// needs is the wait with condition=next-exit, which is v1.30, so the number is the oldest
// daemon the project intends to support and it belongs on the site.
//
// # The endpoints spoken
//
// The six the runner section names, GET /containers/{id}/json, POST
// /containers/{id}/stop, GET /containers/json, GET /containers/{id}/archive, GET /images
// /{name}/json, POST /images/create, GET /containers/{id}/stats, the network calls, and
// GET /events.
//
//	GET  /_ping                          negotiate the version, once per daemon
//	GET  /info                           SecurityOptions, for userns and the profiles applied
//	POST /images/create                  pull by digest, a progress stream of JSON lines
//	GET  /images/{name}/json             the image config, for the user it declares
//	POST /containers/create              the settings table, as HostConfig writes it
//	POST /containers/{id}/start          start
//	POST /containers/{id}/attach         hijacked, which is how the envelope reaches stdin
//	POST /containers/{id}/wait           opened before start, condition=next-exit
//	GET  /containers/{id}/logs           after the exit, which is why AutoRemove is false
//	GET  /containers/{id}/archive        /agk/brick.yaml out of the image, /agk/out back
//	GET  /containers/{id}/json           the backstop, and State.StartedAt and FinishedAt
//	GET  /containers/{id}/stats          sampled while it runs, for the usage block
//	GET  /containers/json                adoption and the startup sweep, filtered by label
//	POST /containers/{id}/stop           SIGTERM then SIGKILL after t, the daemon's own
//	POST /containers/{id}/kill            the backstop signal
//	DELETE /containers/{id}              remove
//	POST /networks/create                a network per task
//	DELETE /networks/{id}                remove it with the container
//	GET  /events                         a JSON stream, resumed from since
//
// # Three daemon behaviours handled here
//
// They are handled here rather than left to a caller to discover, because each of them is
// a property of the wire and not a rule of the product.
//
// A pull streams JSON lines and reports failure as an error object inside a 200 that has
// already streamed half its layers. ImagePull reads every message and fails on one, so a
// pull that died is never mistaken for a pull that worked.
//
// The wait is opened before start with condition=next-exit. That ordering makes the
// exit-during-attach race unrepresentable rather than rare, and it is an ordering the
// convenience wrappers invite a caller to get wrong.
//
// Events reconnects with since set to the last event seen, so a dropped stream replays
// its gap instead of losing a die or an oom.
//
// # Two transports on purpose
//
// A pooled http.Transport with a unix DialContext for the unary calls, and a raw net.Conn
// for the hijacked attach, so a hijacked connection is never handed back to the pool.
// There is no client-level timeout anywhere, because attach, wait, logs and events are
// all long-lived; each unary call is bounded by its context instead.
//
// # Layout
//
//	client.go     Dial, the two transports, the version negotiation, the request helpers
//	version.go    Ceiling, Floor, Ping and the refusal that names both versions
//	info.go       Info and SecurityOptions, where userns, seccomp, AppArmor and SELinux are read
//	image.go      ImagePull reading every progress message, and ImageInspect
//	container.go  create, start, wait, logs, inspect, list, stop, kill, remove, archive
//	attach.go     the hijacked upgrade, dialed raw and spoken by hand
//	stream.go     the eight-byte stdcopy frame header, which separates stdout from stderr
//	events.go     the event stream as newline-delimited JSON, resumed from since
//	stats.go      a container's statistics, one sample now or a stream of them
//	network.go    create, list and remove
//	types.go      the Engine API shapes with their wire json tags
//	errors.go     Error, and the three questions a caller asks of one
//
// Its tests: a fake daemon from internal/dockertest on a temporary unix socket covers the
// wire encoding, the frame demultiplexing and the hijack handshake with no Docker
// present, and one file guarded by a skip runs the same calls against a real daemon when
// the socket is there.
package docker
