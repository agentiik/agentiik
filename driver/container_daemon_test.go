package driver

import (
	"context"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// What a container was actually created with, read back off a real socket.
//
// The settings table and the environment table are both assertions about bytes the
// daemon received, not about a struct that was handed to a client: a field with the
// wrong wire name, or one omitted where the table says it is always sent, arrives here
// as a zero value rather than passing because nothing was ever marshalled.
func TestWhatTheContainerIsCreatedWith(t *testing.T) {
	daemon, err := dockertest.NewDaemon()
	if err != nil {
		t.Fatalf("starting a fake daemon: %s", err)
	}
	defer daemon.Close()

	cli, err := docker.Dial(daemon.Socket())
	if err != nil {
		t.Fatalf("dialing the fake daemon: %s", err)
	}
	defer cli.Close()

	task := settingsTask()
	task.Outputs = []agk.Port{"out", "error"}
	task.Inputs = map[agk.Port]agk.Envelope{"in": oneItem("normalize", "ok")}

	g, w := prepared(t, task, nil, t.TempDir())
	host, err := hostConfig(task, DefaultPolicy(), g, networkModeNone)
	if err != nil {
		t.Fatalf("hostConfig: %s", err)
	}
	config := containerConfig(task, "ghcr.io/acme/agk-invoice@sha256:1ab74e", "65532:65532", environment(task, task.Deadline), nil, nil)

	if _, err := cli.ContainerCreate(context.Background(), "", config, host, networkingConfig(networkModeNone)); err != nil {
		t.Fatalf("creating the container: %s", err)
	}

	created := daemon.Created()
	if len(created) != 1 {
		t.Fatalf("%d containers were created", len(created))
	}
	c := created[0]

	// The environment table, as the container reads it.
	for name, want := range map[string]string{
		EnvRunID:    "01JMZ8V1P9C4",
		EnvStep:     "invoice",
		EnvAttempt:  "2",
		EnvShard:    "3/8",
		EnvRepo:     RepoDir,
		EnvOutPorts: "out,error",
	} {
		if got, ok := c.Env(name); !ok || got != want {
			t.Errorf("%s is %q, set %v, want %q", name, got, ok, want)
		}
	}

	// The settings table, as the daemon received it.
	if !c.HostConfig.ReadonlyRootfs {
		t.Errorf("ReadonlyRootfs did not survive the wire")
	}
	if len(c.HostConfig.CapDrop) != 1 || c.HostConfig.CapDrop[0] != "ALL" {
		t.Errorf("CapDrop is %v", c.HostConfig.CapDrop)
	}
	if c.HostConfig.AutoRemove {
		t.Errorf("AutoRemove is true, and the driver removes the container itself")
	}
	if c.HostConfig.NetworkMode != networkModeNone {
		t.Errorf("NetworkMode is %q", c.HostConfig.NetworkMode)
	}
	if !strings.Contains(strings.Join(c.HostConfig.SecurityOpt, " "), "no-new-privileges") {
		t.Errorf("SecurityOpt is %v", c.HostConfig.SecurityOpt)
	}
	if c.HostConfig.PidsLimit == nil || *c.HostConfig.PidsLimit != 128 {
		t.Errorf("PidsLimit did not survive the wire: %v", c.HostConfig.PidsLimit)
	}
	if opts := c.HostConfig.Tmpfs[TmpDir]; !strings.Contains(opts, "noexec") {
		t.Errorf("the %s tmpfs is %q", TmpDir, opts)
	}

	// The mounts, as the contract lays them out: the input read-only, the repository
	// read-only, the output writable, and the working directory behind them.
	want := map[string]bool{brick.InDir + "/in": true, RepoDir: true, RunPath: true, ParamsPath: true, brick.OutDir: false}
	got := map[string]bool{}
	for _, m := range c.HostConfig.Mounts {
		if m.Type != docker.MountBind {
			t.Errorf("%s is a %s and not a bind", m.Target, m.Type)
		}
		got[m.Target] = m.ReadOnly
	}
	for target, readOnly := range want {
		if seen, ok := got[target]; !ok || seen != readOnly {
			t.Errorf("%s is mounted read-only %v, set %v, want read-only %v", target, seen, ok, readOnly)
		}
	}
	if c.Work != w.Out {
		t.Errorf("the container writes into %q, and the working directory is %q", c.Work, w.Out)
	}

	// The identity, as a redelivered task, a Stop and a sweep all resolve it.
	if c.Labels[LabelTask] != string(task.ID) {
		t.Errorf("the container carries %v", c.Labels)
	}
	// Standard input is open, because the envelope on it is half of what a brick is
	// given, and there is no terminal, because one would merge the two output streams.
	if !c.Config.OpenStdin || c.Config.Tty {
		t.Errorf("OpenStdin is %v and Tty is %v", c.Config.OpenStdin, c.Config.Tty)
	}
	if c.Config.User != "65532:65532" {
		t.Errorf("User is %q", c.Config.User)
	}
}
