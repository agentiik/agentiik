package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/docker"
)

// vault is a secret source of values written down, which is what agk run --local is and
// what a server runner's grant redemption comes back with.
type vault map[string]string

func (v vault) Value(_ context.Context, name string) ([]byte, error) {
	value, ok := v[name]
	if !ok {
		return nil, errors.New("no grant for this secret")
	}
	return []byte(value), nil
}

// laptop is the policy these tests prepare under: the one of a machine with no tmpfs of
// the runner's own, where a secret value is written under the task's working directory,
// which is the directory newWorkdir is given here.
func laptop() Policy {
	p := DefaultPolicy()
	p.SecretsDir = ""
	p.RequireSecretsTmpfs = SecretsTmpfsLifted
	return p
}

// prepared runs the whole of the host side for one task and hands back what a create
// would have been given, with the working directory removed when the test ends.
func prepared(t *testing.T, task graph.Task, secrets Secrets, repo string) (*given, *workdir) {
	t.Helper()
	store, err := artifact.New(artifact.Dir(t.TempDir()), "finance", agk.DefaultLimits())
	if err != nil {
		t.Fatalf("opening a store: %s", err)
	}
	if task.ID == "" {
		task.ID = agk.NewTaskID("01JMZ8V1P9C4", task.Step, 1, agk.Shard{})
	}
	w, err := newWorkdir(t.TempDir(), task.ID, "")
	if err != nil {
		t.Fatalf("newWorkdir: %s", err)
	}
	t.Cleanup(func() { w.remove() })

	run := agk.Run{ID: "01JMZ8V1P9C4", Workflow: "finance/monthly-invoicing@a3f9c1e", Namespace: "finance", Commit: "a3f9c1e"}
	g, err := prepare(context.Background(), task, w, laptop(), kernel{}, store, run, repo, secrets)
	if err != nil {
		t.Fatalf("prepare: %s", err)
	}
	return g, w
}

// mountAt finds what was bound at one path in the container.
func mountAt(t *testing.T, g *given, target string) docker.Mount {
	t.Helper()
	for _, m := range g.Mounts {
		if m.Target == target {
			return m
		}
	}
	t.Fatalf("nothing is mounted at %s: %s", target, mountTargets(g))
	return docker.Mount{}
}

func mountTargets(g *given) string {
	var out []string
	for _, m := range g.Mounts {
		out = append(out, m.Target)
	}
	return strings.Join(out, ", ")
}

func oneItem(step agk.Step, port agk.Port) agk.Envelope {
	e := agk.Empty("01JMZ8V1P9C4", step, port, 1, time.Date(2026, 9, 10, 6, 0, 0, 0, time.UTC))
	e.Items = []agk.Item{{ID: "01JMZ8V1PC7K3M0", Data: map[string]any{"vat_number": "FR12345678901"}, Files: []agk.File{}}}
	e.Meta.Count = 1
	return e
}

// "Input arrives where it always does: the envelope on standard input and at
// /agk/in/<port>/envelope.json, artifacts beside it." Both, and the same bytes.
func TestTheEnvelopeArrivesOnStandardInputAndUnderItsPort(t *testing.T) {
	task := graph.Task{Step: "invoice", Attempt: 1, Inputs: map[agk.Port]agk.Envelope{"in": oneItem("normalize", "ok")}}
	g, _ := prepared(t, task, nil, "")

	m := mountAt(t, g, brick.InDir+"/in")
	if !m.ReadOnly {
		t.Fatalf("%s is writable: an input a step can write to is an input a retry of that step reads differently", m.Target)
	}
	onDisk, err := os.ReadFile(filepath.Join(m.Source, "envelope.json"))
	if err != nil {
		t.Fatalf("reading the envelope under the mount: %s", err)
	}
	if !bytes.Equal(onDisk, g.Stdin) {
		t.Fatalf("standard input and the file under the mount are not the same document:\n%s\n%s", g.Stdin, onDisk)
	}

	var back agk.Envelope
	if err := json.Unmarshal(g.Stdin, &back); err != nil {
		t.Fatalf("standard input is not an envelope: %s", err)
	}
	if len(back.Items) != 1 || back.Items[0].ID != "01JMZ8V1PC7K3M0" {
		t.Fatalf("the envelope on standard input is not the one the step was given: %s", g.Stdin)
	}
}

// One input port is one envelope, whatever the port is called, which is what makes the
// shorthand worth having for the step that has exactly one.
func TestTheOnlyInputPortIsTheOneOnStandardInput(t *testing.T) {
	task := graph.Task{Step: "invoice", Attempt: 1, Inputs: map[agk.Port]agk.Envelope{"orders": oneItem("normalize", "ok")}}
	if port, ok := stdinPort(task); !ok || port != "orders" {
		t.Fatalf("standard input carries %q, %v", port, ok)
	}
}

// Several ports and one of them called in: standard input carries that one, and the rest
// are under /agk/in. A stream is one document and cannot carry two.
func TestSeveralInputPortsPutInOnStandardInput(t *testing.T) {
	task := graph.Task{Step: "invoice", Attempt: 1, Inputs: map[agk.Port]agk.Envelope{
		"in":     oneItem("normalize", "ok"),
		"prices": oneItem("rates", "out"),
	}}
	port, ok := stdinPort(task)
	if !ok || port != "in" {
		t.Fatalf("standard input carries %q, %v", port, ok)
	}

	g, _ := prepared(t, task, nil, "")
	mountAt(t, g, brick.InDir+"/in")
	mountAt(t, g, brick.InDir+"/prices")
}

// Several ports and none of them called in: nothing on standard input. Choosing one of
// them would make a step's behaviour depend on the alphabet.
func TestSeveralInputPortsWithNoInCarryNothingOnStandardInput(t *testing.T) {
	task := graph.Task{Step: "invoice", Attempt: 1, Inputs: map[agk.Port]agk.Envelope{
		"orders": oneItem("normalize", "ok"),
		"prices": oneItem("rates", "out"),
	}}
	if _, ok := stdinPort(task); ok {
		t.Fatalf("one of two ports was chosen for standard input")
	}
	g, _ := prepared(t, task, nil, "")
	if g.Stdin != nil {
		t.Fatalf("standard input carries %q", g.Stdin)
	}
}

// A step with no inputs reads an empty stream and not an envelope minted here, which
// would carry a meta block naming a step and a port that never produced it.
func TestAStepWithNoInputsCarriesNothingOnStandardInput(t *testing.T) {
	g, _ := prepared(t, graph.Task{Step: "start", Attempt: 1}, nil, "")
	if g.Stdin != nil {
		t.Fatalf("standard input carries %q", g.Stdin)
	}
}

// "The whole tree is mounted read-only at /agk/repo/ in every step."
func TestTheRepositoryTreeIsMountedReadOnly(t *testing.T) {
	repo := t.TempDir()
	g, _ := prepared(t, graph.Task{Step: "normalize", Attempt: 1}, nil, repo)

	m := mountAt(t, g, RepoDir)
	if m.Source != repo || !m.ReadOnly {
		t.Fatalf("%s is bound from %q, read-only %v", RepoDir, m.Source, m.ReadOnly)
	}
}

// The long form of a selector "relocates a path to wherever a tool insists on finding
// it", which for a tool that will only read /etc/ssl/certs/internal-ca.pem is the only
// way it reads it at all.
func TestASelectorRelocatesAPathWhereATheToolInsistsOnIt(t *testing.T) {
	repo := t.TempDir()
	task := graph.Task{
		Step:    "load",
		Attempt: 1,
		Files: []graph.FileSelector{
			{From: "./sql/**"},
			{From: "./certs/internal-ca.pem", To: "/etc/ssl/certs/internal-ca.pem", Mode: "0444"},
		},
	}
	g, _ := prepared(t, task, nil, repo)

	m := mountAt(t, g, "/etc/ssl/certs/internal-ca.pem")
	if m.Source != filepath.Join(repo, "certs", "internal-ca.pem") {
		t.Fatalf("the relocated path is bound from %s", m.Source)
	}
	if !m.ReadOnly {
		t.Fatalf("a relocated path from the repository is writable")
	}
	// The short form narrows and does not relocate, so it mounts nothing of its
	// own: the whole tree is already there and narrowing "is never a permission
	// boundary".
	for _, mount := range g.Mounts {
		if strings.Contains(mount.Target, "sql") {
			t.Fatalf("a short form selector mounted something at %s", mount.Target)
		}
	}
}

func TestASelectorThatLeavesTheTreeIsRefused(t *testing.T) {
	store, err := artifact.New(artifact.Dir(t.TempDir()), "finance", agk.DefaultLimits())
	if err != nil {
		t.Fatalf("opening a store: %s", err)
	}
	w, err := newWorkdir(t.TempDir(), "01JMZ8V1P9C4/load/1", "")
	if err != nil {
		t.Fatalf("newWorkdir: %s", err)
	}
	defer w.remove()

	task := graph.Task{Step: "load", Attempt: 1, Files: []graph.FileSelector{{From: "../../etc/shadow", To: "/etc/shadow"}}}
	_, err = prepare(context.Background(), task, w, laptop(), kernel{}, store, agk.Run{}, t.TempDir(), nil)
	if err == nil {
		t.Fatalf("a selector reaching outside the repository tree was mounted")
	}
	if !strings.Contains(err.Error(), "load") || !strings.Contains(err.Error(), "repository") {
		t.Fatalf("the refusal names neither the step nor the rule: %s", err)
	}
}

// The run context and the parameters are documents the container reads and never
// writes, at the two paths the contract names.
func TestTheRunContextAndTheParametersAreReadOnlyDocuments(t *testing.T) {
	task := graph.Task{Step: "invoice", Attempt: 1, Params: map[string]any{"endpoint": "https://api.billing.example.com/v2"}}
	g, _ := prepared(t, task, nil, "")

	run := mountAt(t, g, RunPath)
	if !run.ReadOnly {
		t.Fatalf("%s is writable", RunPath)
	}
	var back agk.Run
	readJSON(t, run.Source, &back)
	if back.ID != "01JMZ8V1P9C4" || back.Workflow != "finance/monthly-invoicing@a3f9c1e" {
		t.Fatalf("%s carries %+v", RunPath, back)
	}

	params := mountAt(t, g, ParamsPath)
	if !params.ReadOnly {
		t.Fatalf("%s is writable", ParamsPath)
	}
	var values map[string]any
	readJSON(t, params.Source, &values)
	if values["endpoint"] != "https://api.billing.example.com/v2" {
		t.Fatalf("%s carries %v", ParamsPath, values)
	}
}

// A step with no parameters gets a document with no keys rather than the word null,
// because a brick reading the file expects a document.
func TestAStepWithNoParametersGetsAnEmptyObject(t *testing.T) {
	g, _ := prepared(t, graph.Task{Step: "invoice", Attempt: 1}, nil, "")
	b, err := os.ReadFile(mountAt(t, g, ParamsPath).Source)
	if err != nil {
		t.Fatalf("reading %s: %s", ParamsPath, err)
	}
	if strings.TrimSpace(string(b)) != "{}" {
		t.Fatalf("%s carries %q", ParamsPath, b)
	}
}

// "A secret is mounted on tmpfs at /agk/secrets/<name>, never injected as an environment
// variable", and the value is asked of the secret source as the container is prepared.
func TestASecretIsMountedWhereTheManifestAsksForIt(t *testing.T) {
	task := graph.Task{
		Step:    "invoice",
		Attempt: 1,
		Secrets: []graph.SecretMount{{Name: "billing", Mount: "/agk/secrets/billing"}},
	}
	g, _ := prepared(t, task, vault{"billing": "sk-live-9f11"}, "")

	m := mountAt(t, g, "/agk/secrets/billing")
	if !m.ReadOnly {
		t.Fatalf("a secret is mounted writable")
	}
	value, err := os.ReadFile(m.Source)
	if err != nil {
		t.Fatalf("reading the value: %s", err)
	}
	if string(value) != "sk-live-9f11" {
		t.Fatalf("the value under the mount is %q", value)
	}
	info, err := os.Stat(m.Source)
	if err != nil {
		t.Fatalf("stat: %s", err)
	}
	if info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("the value is writable: %04o", info.Mode().Perm())
	}
	if info.Mode().Perm()&0o004 == 0 {
		t.Fatalf("the value is %04o, and the account that reads it is the container's and not the runner's", info.Mode().Perm())
	}

	// Masking is "a literal match against the values the task was given", and this
	// is where a task is given them.
	if len(g.Values) != 1 || string(g.Values[0]) != "sk-live-9f11" {
		t.Fatalf("the redeemed values did not come back for the masker: %q", g.Values)
	}
}

// A secret with no mount point of its own lands at the name the documentation gives.
func TestASecretWithNoMountLandsUnderItsName(t *testing.T) {
	task := graph.Task{Step: "invoice", Attempt: 1, Secrets: []graph.SecretMount{{Name: "bearer"}}}
	g, _ := prepared(t, task, vault{"bearer": "token"}, "")
	mountAt(t, g, SecretsDir+"/bearer")
}

// "A mount elsewhere, /run/secrets/bearer out of habit, is refused."
func TestASecretMountedOutsideAgkSecretsIsRefused(t *testing.T) {
	store, _ := artifact.New(artifact.Dir(t.TempDir()), "finance", agk.DefaultLimits())
	w, err := newWorkdir(t.TempDir(), "01JMZ8V1P9C4/invoice/1", "")
	if err != nil {
		t.Fatalf("newWorkdir: %s", err)
	}
	defer w.remove()

	task := graph.Task{Step: "invoice", Attempt: 1, Secrets: []graph.SecretMount{{Name: "bearer", Mount: "/run/secrets/bearer"}}}
	_, err = prepare(context.Background(), task, w, laptop(), kernel{}, store, agk.Run{}, "", vault{"bearer": "token"})
	if err == nil {
		t.Fatalf("a secret was mounted at /run/secrets/bearer")
	}
	for _, want := range []string{"invoice", "bearer", secretMountRule} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %q: %s", want, err)
		}
	}
	// A mount the manifest asked for in the wrong place is the image's failure to
	// honour the contract, which is the band the exit code table reserves for it.
	if !errors.Is(err, ErrContractBroken) {
		t.Fatalf("the refusal is not an instance of the contract being broken: %s", err)
	}
	if charge, ok := Charged(err); !ok || charge != ChargeBrick {
		t.Fatalf("the refusal is charged to %s, decided %v", charge, ok)
	}
}

// A mount is one file directly under /agk/secrets/, and the host side is named after its last
// element. So a mount of . would replace the secrets directory with the value and one of .. would
// name its parent, and both are refused as the brick's, while a file name carrying a dot, as a
// key file does, lands where it says.
func TestASecretMountIsAFileUnderAgkSecretsAndNeverItsParent(t *testing.T) {
	for _, mount := range []string{"/agk/secrets/..", "/agk/secrets/.", "/agk/secrets/.netrc", "/agk/secrets/a/b"} {
		store, _ := artifact.New(artifact.Dir(t.TempDir()), "finance", agk.DefaultLimits())
		w, err := newWorkdir(t.TempDir(), "01JMZ8V1P9C4/invoice/1", "")
		if err != nil {
			t.Fatalf("newWorkdir: %s", err)
		}
		task := graph.Task{Step: "invoice", Attempt: 1, Secrets: []graph.SecretMount{{Name: "bearer", Mount: mount}}}
		_, err = prepare(context.Background(), task, w, laptop(), kernel{}, store, agk.Run{}, "", vault{"bearer": "token"})
		w.remove()
		if err == nil {
			t.Errorf("a secret was mounted at %s", mount)
			continue
		}
		if !errors.Is(err, ErrContractBroken) {
			t.Errorf("the mount %s was refused, and not as the contract being broken: %s", mount, err)
		}
	}

	task := graph.Task{Step: "invoice", Attempt: 1, Secrets: []graph.SecretMount{{Name: "bearer", Mount: "/agk/secrets/client.key"}}}
	g, _ := prepared(t, task, vault{"bearer": "token"}, "")
	mountAt(t, g, "/agk/secrets/client.key")
}

func TestTwoSecretsOnOnePathAreRefused(t *testing.T) {
	store, _ := artifact.New(artifact.Dir(t.TempDir()), "finance", agk.DefaultLimits())
	w, err := newWorkdir(t.TempDir(), "01JMZ8V1P9C4/invoice/1", "")
	if err != nil {
		t.Fatalf("newWorkdir: %s", err)
	}
	defer w.remove()

	task := graph.Task{Step: "invoice", Attempt: 1, Secrets: []graph.SecretMount{
		{Name: "billing", Mount: "/agk/secrets/token"},
		{Name: "bearer", Mount: "/agk/secrets/token"},
	}}
	_, err = prepare(context.Background(), task, w, laptop(), kernel{}, store, agk.Run{}, "", vault{"billing": "a", "bearer": "b"})
	if err == nil {
		t.Fatalf("two secrets were mounted at one path")
	}
}

// A task that names secrets and a driver with nowhere to redeem them is a task that
// cannot be run, and it is refused rather than started without the values.
func TestSecretsWithNoSourceAreRefused(t *testing.T) {
	store, _ := artifact.New(artifact.Dir(t.TempDir()), "finance", agk.DefaultLimits())
	w, err := newWorkdir(t.TempDir(), "01JMZ8V1P9C4/invoice/1", "")
	if err != nil {
		t.Fatalf("newWorkdir: %s", err)
	}
	defer w.remove()

	task := graph.Task{Step: "invoice", Attempt: 1, Secrets: []graph.SecretMount{{Name: "billing"}}}
	if _, err := prepare(context.Background(), task, w, laptop(), kernel{}, store, agk.Run{}, "", nil); err == nil {
		t.Fatalf("a task naming a secret was prepared with no secret source")
	}
}

// A grant that will not redeem refuses the task naming the secret, rather than mounting
// an empty file the brick would read as a value.
func TestASecretThatCannotBeRedeemedRefusesTheTask(t *testing.T) {
	store, _ := artifact.New(artifact.Dir(t.TempDir()), "finance", agk.DefaultLimits())
	w, err := newWorkdir(t.TempDir(), "01JMZ8V1P9C4/invoice/1", "")
	if err != nil {
		t.Fatalf("newWorkdir: %s", err)
	}
	defer w.remove()

	task := graph.Task{Step: "invoice", Attempt: 1, Secrets: []graph.SecretMount{{Name: "billing"}}}
	_, err = prepare(context.Background(), task, w, laptop(), kernel{}, store, agk.Run{}, "", vault{})
	if err == nil || !strings.Contains(err.Error(), "billing") {
		t.Fatalf("the refusal does not name the secret: %v", err)
	}
}

// /agk/out is a bind and not a tmpfs, because a tmpfs is unmounted when the container
// stops and an output written to one is gone before anything can collect it. /tmp is the
// tmpfs, sized, with the three flags the settings table names.
func TestTheWritablePathsAreABindAndASizedTmpfs(t *testing.T) {
	g, w := prepared(t, graph.Task{Step: "invoice", Attempt: 1}, nil, "")

	out := mountAt(t, g, brick.OutDir)
	if out.Type != docker.MountBind {
		t.Fatalf("%s is a %s: a tmpfs is unmounted when the container stops, and the outputs go with it", brick.OutDir, out.Type)
	}
	if out.ReadOnly {
		t.Fatalf("%s is read-only, and it is where the brick writes", brick.OutDir)
	}
	if out.Source != w.Out {
		t.Fatalf("%s is bound from %s and not from the working directory %s", brick.OutDir, out.Source, w.Out)
	}

	options, ok := g.Tmpfs[TmpDir]
	if !ok {
		t.Fatalf("%s is not a tmpfs: %v", TmpDir, g.Tmpfs)
	}
	for _, flag := range []string{"noexec", "nosuid", "nodev", "size="} {
		if !strings.Contains(options, flag) {
			t.Fatalf("the %s tmpfs is mounted %q, with no %s", TmpDir, options, flag)
		}
	}
}

// "/agk/bin/agk is mounted read-only: a static helper for scripts that want to be precise
// rather than lucky." It is the runner that has the binary, so it is the runner policy
// that names it, and a runner that names none binds none.
func TestTheStaticHelperIsMountedForAScriptStepWhereTheRunnerHasOne(t *testing.T) {
	helper := filepath.Join(t.TempDir(), "agk")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("writing the helper: %s", err)
	}
	p := DefaultPolicy()
	p.Helper = helper

	script := graph.Task{Step: "check", Attempt: 1, Script: []string{"agk items | jq -r .data"}}
	mounts, err := helperBind(script, p)
	if err != nil {
		t.Fatalf("helperBind: %s", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("%d mounts for a script step on a runner that has the helper", len(mounts))
	}
	if mounts[0].Target != BinPath || mounts[0].Source != helper || !mounts[0].ReadOnly {
		t.Errorf("the helper is bound %+v, and it is %s read-only from what the policy names", mounts[0], BinPath)
	}

	// A brick is an image that honours the contract on its own, and the
	// documentation offers the helper to a script.
	brickStep := graph.Task{Step: "fetch", Attempt: 1}
	if mounts, err := helperBind(brickStep, p); err != nil || len(mounts) != 0 {
		t.Errorf("a brick step was given %d helper mounts, and the helper is offered to a script step", len(mounts))
	}

	// A runner with no helper binds none rather than refusing the step: it is "a
	// convenience, never a requirement".
	if mounts, err := helperBind(script, DefaultPolicy()); err != nil || len(mounts) != 0 {
		t.Errorf("a runner that names no helper bound %d: %v", len(mounts), err)
	}
}

// A helper the policy names and the host does not have refuses the task, because the
// daemon would otherwise create a directory at the source and bind that, so the step
// would find a directory where it was promised a program.
func TestAStaticHelperThatIsNotThereRefusesTheStep(t *testing.T) {
	p := DefaultPolicy()
	p.Helper = filepath.Join(t.TempDir(), "agk")

	_, err := helperBind(graph.Task{Step: "check", Attempt: 1, Script: []string{"agk items"}}, p)
	if err == nil {
		t.Fatal("a helper that is not on this host was accepted")
	}
	var f *Fault
	if !errors.As(err, &f) || f.Step != "check" || f.Charge != ChargePlatform {
		t.Fatalf("the refusal is %v, and it names the step and charges the runtime", err)
	}
	if !strings.Contains(err.Error(), BinPath) || !strings.Contains(err.Error(), PolicyPath) {
		t.Errorf("the refusal reads %q, and it names %s and %s", err, BinPath, PolicyPath)
	}
}

func readJSON(t *testing.T, path string, into any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %s", path, err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatalf("reading %s: %s", path, err)
	}
}
