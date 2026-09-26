// Package e2e stands up a test installation and runs workflows on it end to end: the programs this
// checkout builds, in the images build/*.Dockerfile describes, each started the way the Compose file
// of github.com/agentiik/deploy starts it, with nothing faked between them.
//
//	PostgreSQL          postgres:17-alpine, reached on its socket alone
//	init                agentiik-api init from the API's image, as root, on a volume per service:
//	                    the certificate, the keys, the bus identity and nats.conf, the migration,
//	                    the namespace, the operator token's hash and a join token of the pool default
//	the bus             nats:2-alpine, on the nats.conf init wrote, on the host's network
//	the API             agentiik-api serve from its image, behind a TLS terminator the test runs,
//	                    which is AGK_PROXY_URL and the installation's public URL; agentiik-api health
//	                    says when it is ready, as the Compose file's health check does
//	the controller      agentiik-controller from its image, sharing the API's objects volume, which
//	                    no runner sees, and its bus volume read only, where the API renews the
//	                    control plane's credential
//	the registry        registry:2 on the installation's network, holding the fixture bricks by
//	                    digest
//	runner a, runner b  each an agent in the runner image, started as root with the capabilities
//	                    the Compose file gives it, joining on its own with a join token of its own
//	                    in AGK_RUNNER_JOIN_TOKEN and AGK_RUNNER_LABELS, driving a docker:dind daemon
//	                    of its own
//
// A test that asks for a runner of the pool default, JoinDefault, has it join as the Compose file's
// runner does: with init's runner volume as its /etc/agentiik, and the join token init wrote there
// in AGK_RUNNER_JOIN_TOKEN_FILE.
//
// Stand builds it for one test and takes it down when the test ends, printing every component's
// log first where the test failed. It skips unless AGENTIIK_E2E=1.
//
// # Running it
//
// On Linux, with a Docker daemon that may run privileged containers (every container is started
// in the host's user namespace, so a remapped daemon runs them too), and nothing on the ports 4222
// and 8222 the bus listens on, as the e2e job of .github/workflows/e2e.yml does:
//
//	AGENTIIK_E2E=1 go test ./e2e -count=1 -v -timeout 25m
//
// It pulls postgres:17-alpine, nats:2-alpine, registry:2, docker:29-dind and alpine:3.21 the first
// time, builds the programs and the three images, and takes a few minutes. It starts two Docker
// daemons, which is heavy for a laptop: the job is what runs it on every push.
//
// Linux alone, because the daemons resolve every bind against the host they run on. Each runner's
// work root is a named volume its agent and its daemon both mount at the same path, and the
// agents, the API and the bus join the host's network, where 127.0.0.1 is the API's terminator and
// the bus. A daemon in a virtual machine, which is what Docker is on macOS, shares neither with the
// machine running the test.
//
// # What it gives up, and why
//
// runner.toml writes require_userns_remap = false and nothing else, as the Compose file writes it by
// default, because a docker:dind daemon does not remap user namespaces and the floor would refuse
// it. The installation tests what a server run does; the remapped floor is tested by the userns job
// of test.yml, on a daemon remapped as the page installs one.
//
// The terminator's certificate is signed by an authority of the test's own, which each runner
// trusts as the Compose file has it trust AGENTIIK_CA, in a directory SSL_CERT_DIR names beside
// init's certificate. The API, the controller and the runners trust the bus's certificate, init's,
// through SSL_CERT_DIR too.
//
// The runners' daemons are on a network of the installation's own with the registry, which they
// pull from by its name with --insecure-registry, since a registry with a certificate would be
// one more authority for three daemons to trust. This machine's daemon does not resolve that name,
// so a brick is built here and pushed by runner a's daemon, which then forgets it, and whichever
// runner takes a task pulls the brick by digest.
//
// # What the smoke test holds
//
// A one-step workflow pushed and started with the operator token runs to succeeded, and each runner
// holds AGK_API, its runner.env and its key, and no database URL, store credential, bus credential
// at rest or repository, leaves no secrets volume on its daemon, and reaches objects only through
// presigned URLs and the signed upload policy. That is the first half of the fact v0.2.0 holds;
// the test's comment says how each part is read.
//
// # What the other two hold
//
// A runner killed mid-step: its agent is sent SIGKILL while its daemon runs the middle step of
// three, and the daemon is left up. The dispatch is declared lost three heartbeat intervals after
// the runner last spoke of it, the key is requeued under a new task_id that the other runner
// redeems and runs, and the run succeeds with nobody touching it. The tasks table is read as the
// database's superuser, since no route answers every dispatch of a key, and each daemon's events
// say which containers it created, since a runner removes each once its task has ended.
//
// One workflow run locally and on a server: v0.1.0's milestone fixture, its images pushed to the
// registry, run with agk run --local on this machine's daemon and again on the installation with
// the same inputs and the same secret, and every declared output compared through internal/diff
// under diff.Default.
package e2e
