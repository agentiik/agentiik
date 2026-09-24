// Command agentiik-api is the API of an installation, as a program of its own: it "serves every
// route, the built-in object store and the secret providers", and it has the verbs that prepare
// what it serves on.
//
//	agentiik-api serve                 serve every route
//	agentiik-api migrate               apply the migrations and create the application role
//	agentiik-api bus-init DIR          create the installation's NATS operator and accounts
//	agentiik-api bus-credential DIR    mint the control plane a new bus credential
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
// start, and nothing is opened. It then opens the database as the application role, connects to
// the bus with the control plane's credential, which creates the streams, and serves every route
// built so far on AGK_LISTEN: runs and versions, cancelling a run, a step's log stream, the secret
// declarations, the runners, their pools and join tokens, the bus credential, and the built-in
// object store at /objects on AGK_PUBLIC_URL. It takes no argument, since a flag would be a second way to say what
// the environment says.
//
// At SIGINT or SIGTERM it stops taking connections, ends the log streams at once so that their
// readers resume at another API, and finishes the requests being answered, for up to thirty
// seconds, then cuts what is left.
//
// The control plane's bus credential expires. From fourteen days before, the API says so once a
// day, and says so again when it has; it goes on serving past it, for the reason watchCredential
// gives, but gives no runner a bus credential, so every runner loses the bus within the hour.
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
