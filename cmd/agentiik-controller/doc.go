// Command agentiik-controller is the controller of an installation, as a program of its own: it
// "leads by advisory lock, sweeps, consumes results and publishes tasks".
//
// Every rule it follows is somebody else's. Package controller elects, decides and sweeps,
// package bus/control carries what was decided and hands back what runners answered, package
// version rebuilds a graph from what a push stored, and package internal/config reads the
// installation's settings. This package opens what they are given and runs them, and adds nothing
// a test of theirs would have to know about.
//
// # Why a program apart from the API
//
// "The API and the controller are two programs rather than one because only the API may link the
// secret store." The controller "names which secret a task may have and never sees its value", and
// the sealed values sit in the database it shares with the API: the master key, held by the API
// alone, is what keeps them from it. A process that linked both could open a value whatever its
// configuration said, so the line is drawn at what the binary contains. secret/boundary_test.go
// reads every package of the module, this one included, and fails on any that reaches the store;
// internal/config refuses a controller whose environment names the master key's file.
//
// # Configuration
//
// Only its environment, read by config.ReadController: the database it connects to as the
// application role, the bus and the control plane's credential, the object-store directory,
// AGK_MAX_REQUEUES, AGK_TASK_CEILING, the sink the audit log is exported to, which is the one
// setting it starts without, saying so, where it answers its metrics, AGK_METRICS_LISTEN, with the
// hash of the token a scrape bears in the file AGK_METRICS_TOKEN_FILE names, and AGK_OTLP_ENDPOINT,
// the OpenTelemetry collector every ended run's trace is sent to, where one is named. It takes no
// argument, since a flag would be a second way to say what the environment says and a Compose file,
// a systemd unit and a container platform all set an environment the same way. A setting that
// refuses the start is named, with every other one that does, and nothing is opened.
//
// # Exporting the audit log
//
// "It is exported continuously outside the installation, since the incident's own host may be
// unreadable." The controller that leads sends every entry to the https sink AGK_AUDIT_EXPORT_URL
// names, as package audit's Exporter does, for as long as its term lasts: one controller at a time,
// so that two never race to the sink, and a standby that takes over carries on from the cursor in
// the database. A sink that fails ends nothing; it is said, and tried again.
//
// Each term also begins by verifying the chain in the database, so that an entry changed after it
// was exported and before it was verified is noticed without the copy outside. A break is a warning
// naming the first broken entry, and holds nothing up. The verification carries on from the last
// entry the one before it proved, whichever controller that was, and checks that entry against the
// hash recorded beside it before trusting it, so a long log is not read again from its first entry
// at every failover. An entry before that point is not read again, and a change to it is found by
// comparing with the copy outside.
//
// # Leading, and standing by
//
// "Runs as several instances with one active at a time, elected by a session-level PostgreSQL
// advisory lock." Every instance opens the database and the bus, then waits for the lock, so a
// standby that could not reach either says so when it starts rather than when it is needed. The
// one that holds the lock watches for runs and sweeps on its interval, and takes results off the
// bus; the others try the lock on a loop and take over the instant it frees, which is the moment
// the holder's session ends, however it ended.
//
// Nothing a term does ends it but the fence, a stop, or the loss of what it stands on. A run that
// could not be decided is reported and left to the next sweep, as the sweep already treats one,
// and a result that could not be recorded is reported and delivered again. A write refused by the
// fence is a term that has passed to somebody else, and the listening connection, the result
// consumer or the lock's own session failing is a controller that can no longer hear or decide:
// controller.Lead asks that session on every poll whether it still holds the lock. Each of those
// ends the program rather than the term alone: the lock is released on the way out, a standby
// takes over from the database, where the state lives, and the process's supervisor starts it
// again as a standby. A controller that retried in place would be the partitioned former holder
// the fence exists to refuse.
//
// A controller cut off from the database without a reset is the worst of these, because neither
// end hears anything. Its own side notices within a few polls, as above, and what it sends on the
// way out is bounded, so a stop asked for then still stops. The server's side is asked of the
// server: every connection sets tcp_keepalives_idle, tcp_keepalives_interval and
// tcp_keepalives_count, unless the URL sets them, so that PostgreSQL drops the silent session and
// frees the lock within half a minute rather than the operating system's two hours, and a standby
// takes over. A second SIGINT or SIGTERM ends a process whose way out takes too long.
//
// So does the control plane's bus credential running out, at the instant it does, unless the file
// AGK_BUS_CREDENTIALS_FILE names holds one renewed since, as agentiik-api init renews it. The bus
// refuses the old one from then on, and a controller left running would publish nothing and hear
// nothing while looking alive. A renewed one is taken with no restart: the bus drops the connection
// when the old one expires, and the connection comes back with what the file holds then.
//
// # Metrics
//
// Where AGK_METRICS_LISTEN is set, the program answers GET /metrics there in the Prometheus text
// format, to a request bearing the token, from the moment it starts, standing by or not. It is the
// one program of the installation that exports metrics, and only the instance that leads reports
// any figure: metrics.go says why. A port it cannot listen on refuses the start. The metrics are
// answered in plain HTTP, unless AGK_TLS_CERT_FILE and AGK_TLS_KEY_FILE name a certificate and its
// key, which they are then served over TLS with.
//
// # Exit codes
//
// 0 once stopped by SIGINT or SIGTERM, having released the lock. 1 where the configuration refused
// the start or something ended the program, which says why on standard error. 2 where it was
// given an argument it does not take.
//
// # What it ships as
//
// A static binary, CGO_ENABLED=0, and an image, build/controller.Dockerfile, holding that same
// file, the certificates it verifies the database and the bus with, and an empty
// /var/lib/agentiik/objects for the object store's volume, run as a user that is not root and owns
// that directory. static_test.go builds both and checks each property on what was built rather
// than on the flags passed.
//
// The controller writes in the object store's directory as well as reading it, since it puts every
// task's inputs there, so internal/config refuses a directory the program cannot write in.
package main
