package driver

import (
	"fmt"
	"os"
	"path/filepath"
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
	d.cfg.Policy.Helper = realHelper(t)

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
	// The loopback is listed first, so a listing without it is a listing that never ran,
	// and then no ethernet in it would be no evidence of anything.
	if !listsTheLoopback(log) {
		t.Errorf("the container listed no loopback interface, so nothing here says what it had: %s", log)
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
	d.cfg.Policy.Helper = realHelper(t)

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

// listsTheLoopback says whether the container's listing of its interfaces, each line
// prefixed LINK, names the loopback.
func listsTheLoopback(log string) bool {
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "LINK ") && strings.Contains(line, " lo:") {
			return true
		}
	}
	return false
}

// noWayOut is the probe a container runs to try to leave the host: an HTTP request and a
// bare TCP connection, each to a literal address so that nothing depends on a resolver. It
// says REACHED THE INTERNET for each one that got out, and exits 0 either way. Their own
// errors are left on standard error, so that the log says why each one did not.
//
// It first makes sure both tools are there, and exits 3 naming the one that is not. An if
// around a command the image does not have is false, which reads exactly like a connection
// that was refused, so a probe with no wget in its image passed as a network that held.
var noWayOut = []string{
	`for tool in wget nc; do command -v "$tool" >/dev/null || { echo "NO $tool IN THE IMAGE" >&2; exit 3; }; done`,
	`if wget -q -T 3 -O /dev/null http://1.1.1.1/; then echo "REACHED THE INTERNET" >&2; fi`,
	`if nc -w 3 -z 1.1.1.1 443; then echo "REACHED THE INTERNET" >&2; fi`,
	`exit 0`,
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
		Script:    noWayOut,
	}

	result, err := d.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running: %s", err)
	}
	t.Logf("the container said:\n%s", written.String())
	// A probe that could not run is not a network that held, so a missing tool ends
	// here, as a failure that names it.
	if result.State != agk.TaskSucceeded {
		t.Fatalf("the probe ended %s with exit %d: %s", result.State, result.ExitCode, written.String())
	}
	if strings.Contains(written.String(), "REACHED THE INTERNET") {
		t.Errorf("a container on network: internal reached the internet: %s", written.String())
	}
}

// TestTheNoWayOutProbeTellsAMissingToolFromANetworkThatHeld holds the probe itself, on this
// machine's shell with stand-ins for its two tools, because what was wrong with it was the
// script and not the network. A stand-in answers as the real tool would: 1 for a connection
// that did not get out, 0 for one that did.
//
// Each connection gets out on its own in a case of its own. With only one of them, the other
// line could be dropped from the probe, or never say what it found, and this would still pass
// while the probe tried one way out where it claims two.
func TestTheNoWayOutProbeTellsAMissingToolFromANetworkThatHeld(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tools map[string]int
		code  int
		said  string
	}{
		{name: "without wget", tools: map[string]int{"nc": 1}, code: 3, said: "NO wget IN THE IMAGE"},
		{name: "without nc", tools: map[string]int{"wget": 1}, code: 3, said: "NO nc IN THE IMAGE"},
		{name: "with both and no way out", tools: map[string]int{"wget": 1, "nc": 1}, code: 0},
		{name: "with both and wget getting out", tools: map[string]int{"wget": 0, "nc": 1}, code: 0, said: "REACHED THE INTERNET"},
		{name: "with both and nc getting out", tools: map[string]int{"wget": 1, "nc": 0}, code: 0, said: "REACHED THE INTERNET"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			for tool, code := range tc.tools {
				stand := fmt.Sprintf("#!/bin/sh\nexit %d\n", code)
				if err := os.WriteFile(filepath.Join(bin, tool), []byte(stand), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// The stand-ins and nothing else, so that a tool this machine happens to
			// have is not found in their place.
			script := append([]string{"PATH='" + bin + "'"}, noWayOut...)

			code, _, stderr := runProgram(t, scriptTask(nil, script, nil), nil)
			if code != tc.code {
				t.Errorf("the probe exited %d, want %d: %s", code, tc.code, stderr)
			}
			if tc.said != "" && !strings.Contains(stderr, tc.said) {
				t.Errorf("the probe said %q, and it has to say %q", stderr, tc.said)
			}
			if tc.said == "" && stderr != "" {
				t.Errorf("the probe said %q where nothing got out and nothing was missing", stderr)
			}
		})
	}
}
