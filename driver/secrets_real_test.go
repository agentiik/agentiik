package driver

import (
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/docker"
)

// What the fake daemon plays, held against the daemon of this machine: the holder is the real
// helper, the volume keeps what it wrote until the task's container holds it, the container
// finds its value on a tmpfs mounted read-only, noexec, nosuid and nodev, and neither the
// volume nor the holder outlives the task.
func TestARealSecretIsOnATmpfsVolumeThatGoesWithTheTask(t *testing.T) {
	d, image := realDriver(t)
	d.cfg.Secrets = secretSource{"bearer": "s3cr3t-value"}
	d.cfg.Policy.Helper = realHelper(t)
	var written strings.Builder
	d.cfg.Logs = &sinkFor{b: &written}

	task := graph.Task{
		ID:        agk.NewTaskID("01JMZ8V1P9C4", "volume", 1, agk.Shard{}),
		Run:       "01JMZ8V1P9C4",
		Namespace: "finance",
		Step:      "volume",
		Attempt:   1,
		Image:     image,
		Secrets:   []graph.SecretMount{{Name: "bearer", Mount: "/agk/secrets/bearer"}},
		Outputs:   []agk.Port{"out"},
		Network:   graph.NetworkNone,
		Script: []string{
			`test "$(cat /agk/secrets/bearer)" = "s3cr3t-value" || exit 3`,
			// The mount point, the filesystem and the flags, as the kernel has them.
			`awk '$5 == "/agk/secrets" { for (i = 7; $i != "-"; i++); print "MOUNT", $(i+1), $6 }' /proc/self/mountinfo >&2`,
			`echo x 2>/dev/null > /agk/secrets/other && echo "WRITABLE" >&2`,
			`exit 0`,
		},
	}

	result, err := d.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running: %s", err)
	}
	log := written.String()
	if result.State != agk.TaskSucceeded {
		t.Fatalf("the task ended %s with exit %d, and it exits 3 where its value is not at /agk/secrets/bearer: %s", result.State, result.ExitCode, log)
	}
	var mount string
	for _, line := range strings.Split(log, "\n") {
		if _, rest, ok := strings.Cut(line, "MOUNT "); ok {
			mount = rest
		}
	}
	fstype, options, _ := strings.Cut(mount, " ")
	if fstype != "tmpfs" {
		t.Errorf("/agk/secrets is %q, and it is a tmpfs: %s", mount, log)
	}
	flags := strings.Split(options, ",")
	for _, want := range []string{"ro", "noexec", "nosuid", "nodev"} {
		found := false
		for _, f := range flags {
			found = found || f == want
		}
		if !found {
			t.Errorf("/agk/secrets is mounted %s, without %s", options, want)
		}
	}
	if strings.Contains(log, "WRITABLE") {
		t.Errorf("the container could write under /agk/secrets")
	}

	cli, err := docker.Dial(d.cli.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	label := docker.Filters{}.Add("label", LabelSecrets+"="+string(task.ID))
	if left, err := cli.VolumeList(t.Context(), label); err != nil || len(left) != 0 {
		t.Errorf("the secrets volume survived the task: %v %v", left, err)
	}
	if left, err := cli.ContainerList(t.Context(), label); err != nil || len(left) != 0 {
		t.Errorf("the holder survived the task: %v %v", left, err)
	}
}
