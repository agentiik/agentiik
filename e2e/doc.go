// Package e2e stands up a test installation and runs workflows on it end to end: the programs this
// checkout builds, each started the way an installation starts it, with nothing faked between them.
//
//	PostgreSQL          postgres:17-alpine, migrated by agentiik-api migrate, reached on its socket
//	the bus             nats:2-alpine, on the configuration agentiik-api bus-init wrote, with
//	                    JetStream and TLS
//	the API             agentiik-api serve, a process of this machine, behind a TLS terminator the
//	                    test runs, which is the installation's public URL
//	the controller      agentiik-controller, a process of this machine, sharing the API's object
//	                    store directory, which no runner sees
//	the registry        registry:2 on 127.0.0.1, holding the fixture bricks by digest
//	runner a, runner b  each an agent in the runner image, joined with a join token of its own,
//	                    driving a docker:dind daemon of its own
//
// Stand builds it for one test and takes it down when the test ends, printing every component's
// log first where the test failed. It skips unless AGENTIIK_E2E=1.
//
// # Running it
//
// On Linux, with a Docker daemon that may run privileged containers, as the e2e job of
// .github/workflows/e2e.yml does:
//
//	AGENTIIK_E2E=1 go test ./e2e -count=1 -v -timeout 20m
//
// It pulls postgres:17-alpine, nats:2-alpine, registry:2, docker:29-dind and alpine:3.21 the first
// time, builds the four programs and the runner image, and takes a few minutes. It starts two
// Docker daemons, which is heavy for a laptop: the job is what runs it on every push.
//
// Linux alone, because the daemons resolve every bind against the host they run on. Each runner's
// work root and secrets tmpfs are named volumes its agent and its daemon both mount at the same
// paths, and the agents, the daemons and the registry share the host's network, so that 127.0.0.1
// is the API, the bus and the registry to every one of them. A daemon in a virtual machine, which
// is what Docker is on macOS, shares neither with the machine running the test.
//
// # What it gives up, and why
//
// Each runner.toml writes require_userns_remap = false, because a docker:dind daemon does not
// remap user namespaces and the floor would refuse it. The installation tests what a server run
// does; the remapped floor is tested by the userns job of test.yml, on a daemon remapped as the
// page installs one.
//
// The agents run in the runner image, the container form, rather than as processes of this
// machine: /etc/agentiik/runner.env, runner.toml and /var/lib/agentiik/runner.key are fixed paths,
// and two agents on one filesystem would share them.
//
// The daemons make no bridge and write no firewall rule, since on the host's network the bridge
// and the rules would be the host's. A brick runs on network: none, which needs neither.
//
// # What the smoke test holds
//
// A one-step workflow pushed and started with the operator token runs to succeeded, and each runner
// holds AGK_API, its runner.env and its key, and no database URL, store credential, bus credential
// at rest or repository, and reaches objects only through presigned URLs. That is the first half of
// the fact v0.2.0 holds; the test's comment says how each part is read.
package e2e
