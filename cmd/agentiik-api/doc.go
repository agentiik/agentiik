// Command agentiik-api is the API of an installation, as a program of its own: it "serves every
// route, the built-in object store and the secret providers", and it has the verbs that prepare
// what it serves on.
//
//	agentiik-api serve                 serve every route
//	agentiik-api migrate               apply the migrations and create the application role
//	agentiik-api init                  prepare an installation, and bring it in line with its settings
//	agentiik-api bus-init DIR          create the installation's NATS operator and accounts
//	agentiik-api bus-credential DIR    mint the control plane a new bus credential
//	agentiik-api audit-verify FILE     verify an export of the audit log
//	agentiik-api namespace create NAME create a namespace
//	agentiik-api namespace remove NAME remove a namespace that holds nothing
//
// Every rule it follows is somebody else's. Package api authorises and answers, package secret
// seals and reads values, package artifact signs and keeps objects, package bus mints credentials
// and writes the bus identity, package db provisions the schema and the role, and package
// internal/config reads the installation's settings. This package opens what they are given and
// wires them together, and adds two things of its own: the interim operator, and the warning that
// the control plane's bus credential is running out.
//
// # Why a program apart from the controller
//
// "The API and the controller are two programs rather than one because only the API may link the
// secret store." This is the one package besides api that secret/boundary_test.go lets reach the
// store, and it reaches it by calling secret.Attach, which fills the API's options with the
// providers. The controller's program is held off it by the same test.
//
// # serve
//
// It reads its environment through config.ReadAPI, then parses the master key and checks the env
// prefixes, which only the API may do; every setting that refuses the start is named on the same
// start, and nothing is opened. It then opens the database as the application role, connects to the
// bus with the control plane's credential, which creates the streams and every runner pool's
// consumer, and serves every route built so far on AGK_LISTEN: runs and versions, cancelling a run,
// a step's log stream, the secret declarations, the runners, their pools and join tokens, the bus
// credential, and the built-in object store at /objects on AGK_PUBLIC_URL. It takes no argument,
// since a flag would be a second way to say what the environment says.
//
// It serves plain HTTP, to the TLS terminator in front on a network only the terminator reaches,
// unless AGK_TLS_CERT_FILE and AGK_TLS_KEY_FILE name a certificate and its key: then it serves TLS
// itself, 1.3 where the client speaks it and 1.2 at the least. The certificate is read once, at
// start, so a renewed one is served from the next restart.
//
// AGK_PROXY_URL names a proxy on this host that terminates TLS in front of it. The API is then
// reached at that URL, and serves plain HTTP on the loopback at AGK_LISTEN's port, leaving
// AGK_PUBLIC_URL and the certificate unread, so that one environment, a Compose file's, serves
// either way and setting that one variable chooses.
//
// At SIGINT or SIGTERM it stops taking connections, ends the log streams at once so that their
// readers resume at another API, and finishes the requests being answered, for up to thirty
// seconds, then cuts what is left.
//
// The control plane's bus credential expires. From fourteen days before, the API says so once a
// day, and says so again when it has; it goes on serving past it, for the reason watchCredential
// gives, and still gives runners their bus credentials, but creates no runner pool.
//
// # The interim operator
//
// A v0.2.0 installation has no principals, so it has one operator: a token whose SHA-256 is in the
// file AGK_OPERATOR_TOKEN_FILE names, identified as one principal allowed every permission at every
// scope, and everyone else denied. Both halves are in operator.go, so that v0.3.0's bootstrap token
// deletes them in one place. Package api's default stays api.DenyAll.
//
// # migrate
//
// It reads config.ReadMigration and runs db.Provision as the role AGK_MIGRATE_DATABASE_URL names:
// the migrations, then the NOSUPERUSER NOBYPASSRLS role AGK_DATABASE_URL names, with the password
// in AGK_DATABASE_PASSWORD_FILE. Running it again applies nothing and changes nothing.
//
// # init
//
// init prepares an installation in the directory AGK_INIT_DIR names, whose subdirectories are what
// each service mounts, and brings it back in line with its settings at every run, so that a Compose
// file runs it before every other service at every start. It makes the certificate for
// AGK_INIT_HOST, ECDSA P-256 and valid 825 days, and makes it again where the host changed or it
// expired, but never over one a person put there, which it refuses to start on instead; the master
// key, the presign key and the database password, once; the bus identity, once, as bus-init does,
// renewing the control plane's credential from when the API would warn of it, and the bus's
// configuration; the hash of the operator token AGK_OPERATOR_TOKEN holds, or of one it mints and
// prints once where none is set and none was stored; the migration, as migrate does, as the role
// AGK_MIGRATE_DATABASE_URL names; the namespace AGK_INIT_NAMESPACE names, as namespace create does;
// and a join token of the pool default for the runner beside it, issued through the database since
// the API is not serving yet. Each service is given its own copy of what it reads, owned by uid
// 65532 where init runs as root, but for the bus's, which runs as root, and the runner's
// certificate, which anybody may read.
//
// It is the one program that takes a secret as a value: the operator token, which a person sets
// once in the file Docker Compose reads, and of which init writes the hash alone.
//
// # namespace
//
// namespace create NAME creates a namespace, and namespace remove NAME removes one that holds no
// workflow, run or secret, refusing one that does and saying what it holds. v0.2.0 has no route
// that makes either change, so this verb stands in for v0.3.0's until they do. It reads
// AGK_DATABASE_URL and AGK_DATABASE_PASSWORD_FILE and connects as that role, the one the API
// connects as, so it runs where the API runs with the API's environment; the name is held to what the API holds a namespace
// to, reserved words refused. Each change is recorded in the audit log in its own transaction, as
// namespace.create or namespace.delete by the operator, and a namespace created again is recorded
// unchanged and left as it was, so that an installation script can run it every time.
//
// # bus-init and bus-credential
//
// bus-init writes the installation's bus identity in DIR, once: accounts.conf for the NATS server to
// include, account.seed for the API, and control-plane.creds for the API and the controller, valid
// ninety days. bus-credential mints a new control-plane.creds in DIR from the account seed, leaving
// the operator and the accounts alone, so the streams and the tasks queued on them stay; the API and
// the controller read it at their next start. Both refuse a DIR its group or anybody else may write
// to. They take the directory as their one argument rather than from the environment, since they
// are run once by a person, and print the paths to name in the settings.
//
// # audit-verify
//
// It checks the chain of an export of the audit log, as whatever received it outside the
// installation wrote it down, one entry per line, some perhaps twice. It reads that file and nothing
// else, no setting and no database, since it is run away from the installation and most of all when
// the installation's own host is in question. It prints how far the chain holds, or where it breaks
// and exits 1.
//
// # Exit codes
//
// 0 once a verb has done its work, or serve was stopped by SIGINT or SIGTERM. 1 where the
// configuration refused the start or the verb failed, which says why on standard error. 2 where it
// was given no verb, one it does not have, or arguments the verb does not take.
//
// # What it ships as
//
// A static binary, CGO_ENABLED=0, and an image, build/api.Dockerfile, holding that same file and
// the certificates it verifies the database and the bus with, run as a user that is not root, with
// serve as its default verb. static_test.go builds both and checks each property on what was built.
package main
