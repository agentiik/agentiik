package driver

import (
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
)

// What a real container can actually write, asked of the kernel rather than of the
// create body. A ReadOnly field that went on the wire and a mount the container cannot
// write to are two different claims, and only the second is the one the figure makes.
func TestARealContainerCannotWriteWhatTheContractCallsReadOnly(t *testing.T) {
	d, image := realDriver(t)
	d.cfg.Secrets = secretSource{"bearer": "s3cr3t-value"}

	var written strings.Builder
	d.cfg.Logs = &sinkFor{b: &written}

	task := graph.Task{
		ID:        agk.NewTaskID("01JMZ8V1P9C4", "probe", 1, agk.Shard{}),
		Run:       "01JMZ8V1P9C4",
		Workflow:  "finance/monthly-invoicing@a3f9c1e",
		Namespace: "finance",
		Commit:    "a3f9c1e",
		Step:      "probe",
		Attempt:   1,
		Image:     image,
		Inputs: map[agk.Port]agk.Envelope{
			"in": {Items: []agk.Item{agk.NewItem(map[string]any{"url": "https://example.test"})}},
		},
		Secrets: []graph.SecretMount{{Name: "bearer", Mount: "/agk/secrets/bearer"}},
		Outputs: []agk.Port{"out"},
		Network: graph.NetworkNone,
		Script: []string{
			// Each one is a path the figure marks (ro), plus the root filesystem
			// itself. A write that lands is named; a write that is refused is the
			// contract holding.
			`for p in /agk/repo/probe /agk/run.json /agk/params.json /agk/secrets/bearer /agk/in/in/envelope.json /probe /etc/probe /usr/bin/probe; do
			     if echo x 2>/dev/null >>"$p"; then echo "WRITABLE $p" >&2; fi
			 done`,
			// The two the table says are writable have to be.
			`echo x > /tmp/probe || echo "NOT WRITABLE /tmp" >&2`,
			`echo x > /agk/out/files/probe || echo "NOT WRITABLE /agk/out" >&2`,
			// network: none is no network at all, so nothing resolves and nothing
			// routes. The loopback interface is the only one there is.
			`ip -o link show 2>/dev/null | sed 's/^/LINK /' >&2 || true`,
		},
	}

	result, err := d.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running: %s", err)
	}
	log := written.String()
	t.Logf("the container said:\n%s", log)

	if result.State != agk.TaskSucceeded {
		t.Fatalf("the probe ended %s with exit %d", result.State, result.ExitCode)
	}
	if strings.Contains(log, "WRITABLE") {
		t.Errorf("a path the contract calls read-only took a write: %s", log)
	}
	if strings.Contains(log, "NOT WRITABLE") {
		t.Errorf("a path the table calls writable refused a write: %s", log)
	}
	for _, ethernet := range []string{"eth0", "ens", "enp"} {
		if strings.Contains(log, ethernet) {
			t.Errorf("a container on network: none has %s: %s", ethernet, log)
		}
	}
	// The value the container read is masked before anything is written.
	if strings.Contains(log, "s3cr3t-value") {
		t.Errorf("the secret value reached the log: %s", log)
	}
}

// The secret the container is given reads as itself inside the container and as [masked]
// in the log, which is the whole of what masking promises and all it promises.
func TestARealContainerReadsItsSecretAndTheLogDoesNot(t *testing.T) {
	d, image := realDriver(t)
	d.cfg.Secrets = secretSource{"bearer": "s3cr3t-value"}

	var written strings.Builder
	d.cfg.Logs = &sinkFor{b: &written}

	task := graph.Task{
		ID:        agk.NewTaskID("01JMZ8V1P9C4", "redeem", 1, agk.Shard{}),
		Run:       "01JMZ8V1P9C4",
		Namespace: "finance",
		Step:      "redeem",
		Attempt:   1,
		Image:     image,
		Secrets:   []graph.SecretMount{{Name: "bearer", Mount: "/agk/secrets/bearer"}},
		Outputs:   []agk.Port{"out"},
		Network:   graph.NetworkNone,
		Script: []string{
			`test "$(cat /agk/secrets/bearer)" = "s3cr3t-value" || exit 1`,
			`echo "authorising with $(cat /agk/secrets/bearer)" >&2`,
			// The environment carries no value, because "a process environment is
			// readable by its children and ends up in diagnostic dumps".
			`env | grep -q s3cr3t-value && exit 2`,
			`exit 0`,
		},
	}

	result, err := d.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running: %s", err)
	}
	if result.State != agk.TaskSucceeded {
		t.Fatalf("the probe ended %s with exit %d: %s", result.State, result.ExitCode, written.String())
	}
	if strings.Contains(written.String(), "s3cr3t-value") {
		t.Errorf("the secret value reached the log: %s", written.String())
	}
	if !strings.Contains(written.String(), maskToken) {
		t.Errorf("the log carries no mask where the container printed the value: %s", written.String())
	}
}

// network: internal is "a network with no outbound route, for steps that only talk to a
// sidecar service". Asked of the kernel: the container has an interface, and nothing it
// sends leaves the host.
func TestARealContainerOnTheInternalPostureHasNoWayOut(t *testing.T) {
	d, image := realDriver(t)

	var written strings.Builder
	d.cfg.Logs = &sinkFor{b: &written}

	task := graph.Task{
		ID:        agk.NewTaskID("01JMZ8V1P9C4", "walled", 1, agk.Shard{}),
		Run:       "01JMZ8V1P9C4",
		Namespace: "finance",
		Step:      "walled",
		Attempt:   1,
		Image:     image,
		Outputs:   []agk.Port{"out"},
		Network:   graph.NetworkInternal,
		// A literal address, so that nothing here depends on a resolver.
		Script: []string{
			`if wget -q -T 3 -O /dev/null http://1.1.1.1/ 2>/dev/null; then echo "REACHED THE INTERNET" >&2; fi`,
			`if nc -w 3 -z 1.1.1.1 443 2>/dev/null; then echo "REACHED THE INTERNET" >&2; fi`,
			`exit 0`,
		},
	}

	result, err := d.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running: %s", err)
	}
	if result.State != agk.TaskSucceeded {
		t.Fatalf("the probe ended %s with exit %d: %s", result.State, result.ExitCode, written.String())
	}
	if strings.Contains(written.String(), "REACHED THE INTERNET") {
		t.Errorf("a container on network: internal reached the internet: %s", written.String())
	}
}
