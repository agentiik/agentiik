package driver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
)

// The ulimits runner.toml sets are the ones a brick's process runs under, and the capabilities it
// allows are a permission and never a grant: a container on a host that allows NET_BIND_SERVICE
// still holds no capability at all, since nothing a workflow or a manifest writes asks for one. Read
// inside the container, from /proc/self, which is what the process is actually held to rather than
// what the driver sent.
func TestRunnerTomlUlimitsReachTheContainerAndAllowedCapabilitiesGrantNone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.toml")
	text := `allow_cap_add = ["NET_BIND_SERVICE"]

[ulimits]
nofile = { soft = 2048, hard = 8192 }
nproc = { soft = 300, hard = 300 }
`
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := LoadPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	// Lifted as realDriver lifts them, for the daemon of a laptop.
	policy.RequireUsernsRemap = RemapLifted
	policy.RequireSecretsTmpfs = SecretsTmpfsLifted
	policy.SecretsDir = ""
	policy.StopGrace = 2 * time.Second
	d, image := realDriverWith(t, policy)

	task := graph.Task{
		ID:        agk.NewTaskID("01JMZ8V1P9C4", "limits", 1, agk.Shard{}),
		Run:       "01JMZ8V1P9C4",
		Namespace: "finance",
		Step:      "limits",
		Attempt:   1,
		Image:     image,
		// Each line fails the step with its own code, so that the exit code says which limit
		// the container was not held to.
		Script: []string{
			`limit() { awk -v what="$1" 'index($0, what) == 1 { print $(NF-2), $(NF-1) }' /proc/self/limits; }`,
			`test "$(limit 'Max open files')" = "2048 8192" || exit 31`,
			`test "$(limit 'Max processes')" = "300 300" || exit 32`,
			`test "$(awk '/^CapBnd:/ { print $2 }' /proc/self/status)" = 0000000000000000 || exit 33`,
			`printf '{"meta":{"run_id":"%s","step":"%s","port":"out","attempt":1,"count":0,"produced_at":"2026-01-01T00:00:00Z"},"items":[]}' "$AGK_RUN_ID" "$AGK_STEP" > /agk/out/ports/out.json`,
		},
		Outputs: []agk.Port{"out"},
		Network: graph.NetworkNone,
		Timeout: graph.Duration(30 * time.Second),
	}
	result, err := d.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running: %s", err)
	}
	if result.State != agk.TaskSucceeded {
		why := map[int]string{
			31: "nofile is not the 2048 and 8192 runner.toml sets",
			32: "nproc is not the 300 runner.toml sets",
			33: "the container holds a capability, and allow_cap_add allows one without anything asking for it",
		}[result.ExitCode]
		t.Fatalf("the state is %s with exit code %d: %s", result.State, result.ExitCode, strings.TrimSpace(why))
	}
}
