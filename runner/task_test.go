package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/dockertest"
	"github.com/agentiik/agentiik/internal/fixtures"
)

// envelopesFor is an envelope for every input port of a message, holding the items the message
// counts on it.
func envelopesFor(m bus.TaskMessage) map[agk.Port]agk.Envelope {
	out := map[agk.Port]agk.Envelope{}
	for _, in := range m.Inputs {
		e := agk.Empty(agk.RunID(m.RunID), "upstream", agk.Port(in.Port), 1, time.Date(2026, 9, 10, 6, 0, 0, 0, time.UTC))
		for i := range in.Items {
			e.Items = append(e.Items, agk.NewItem(map[string]any{"n": i}))
		}
		e.Meta.Count = len(e.Items)
		out[agk.Port(in.Port)] = e
	}
	return out
}

// Every task message the corpus holds valid, answered by the corpus's redemption as the API
// answers it, assembles into the task the driver runs: the redemption answers the message, its
// secrets decode, and the message reads back as a graph.Task with every field it carries.
func TestEveryValidTaskMessageWithItsRedemptionAssemblesIntoATask(t *testing.T) {
	cases, err := fixtures.TaskMessages()
	if err != nil {
		t.Fatal(err)
	}
	valid := 0
	for _, c := range cases {
		if !c.Valid {
			continue
		}
		valid++
		t.Run(filepath.Base(c.File), func(t *testing.T) {
			var m bus.TaskMessage
			readFixture(t, c.File, &m)
			file := "fixtures/wire/valid/grant-redemption.json"
			if len(m.Secrets) > 1 {
				file = "fixtures/wire/valid/grant-redemption-secret-mount-with-a-dot.json"
			}
			r := answering(redemptionFixture(t, file), m)

			if err := r.answers(m); err != nil {
				t.Fatalf("the redemption does not answer the message: %s", err)
			}
			values, err := decodeSecrets(r)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range m.Secrets {
				if _, err := values.Value(t.Context(), s.Name); err != nil {
					t.Errorf("secret %s: %s", s.Name, err)
				}
			}
			task, err := TaskOf(m, envelopesFor(m))
			if err != nil {
				t.Fatalf("the message does not read back as a task: %s", err)
			}
			if string(task.ID) != m.IdempotencyKey || task.Namespace != m.Namespace || task.Image != m.Image {
				t.Errorf("the task is %s in %s running %s", task.ID, task.Namespace, task.Image)
			}
			if task.Workflow+"@"+task.Commit != m.Workflow {
				t.Errorf("the workflow reads back as %s at %s, from %s", task.Workflow, task.Commit, m.Workflow)
			}
			if got := task.Deadline.UTC().Format(time.RFC3339Nano); got != m.Deadline {
				t.Errorf("the deadline reads back as %s, from %s", got, m.Deadline)
			}
			if task.Network.String() != m.Network || len(task.Inputs) != len(m.Inputs) || len(task.Outputs) != len(m.Outputs) || len(task.Secrets) != len(m.Secrets) {
				t.Errorf("the task reads back as %+v", task)
			}
			if len(m.Script) > 0 && !reflect.DeepEqual(task.Script, m.Script) {
				t.Errorf("the script reads back as %q", task.Script)
			}
		})
	}
	if valid < 5 {
		t.Fatalf("read %d valid task messages, and the corpus holds five", valid)
	}
}

// The base64 value of the corpus decodes to the bytes it stands for, which is what a container
// is given and what its log is masked against, and not to its text.
func TestASecretIsDecodedFromTheEncodingItTravelledIn(t *testing.T) {
	r := redemptionFixture(t, "fixtures/wire/valid/grant-redemption-secret-mount-with-a-dot.json")
	values, err := decodeSecrets(r)
	if err != nil {
		t.Fatal(err)
	}
	key, err := values.Value(t.Context(), "client-key")
	if err != nil {
		t.Fatal(err)
	}
	if string(key) != "-----BEGIN PRIVATE KEY-----" {
		t.Errorf("the base64 value decodes to %q", key)
	}
	billing, _ := values.Value(t.Context(), "billing")
	if string(billing) != "bk_live_7Qm2rXt9vZa4" {
		t.Errorf("the utf-8 value reads as %q", billing)
	}
	if _, err := values.Value(t.Context(), "payroll"); err == nil {
		t.Error("a secret the redemption gave no value for was answered")
	}

	r.Secrets[1].Value = "not base64 at all!"
	if _, err := decodeSecrets(r); !errors.Is(err, ErrAnswerUnusable) {
		t.Errorf("a value that does not decode answered %v", err)
	} else if strings.Contains(err.Error(), "not base64 at all") {
		t.Errorf("the refusal quotes the value: %s", err)
	}
}

// A message whose key says one task and whose fields say another is refused, and so is one
// naming no commit or carrying a deadline that is not an instant.
func TestAMessageThatContradictsItselfIsRefused(t *testing.T) {
	var base bus.TaskMessage
	readFixture(t, "fixtures/wire/valid/task-message.json", &base)
	for name, change := range map[string]func(*bus.TaskMessage){
		"another attempt":  func(m *bus.TaskMessage) { m.Attempt = 3 },
		"another step":     func(m *bus.TaskMessage) { m.Step = "archive" },
		"another run":      func(m *bus.TaskMessage) { m.RunID = "01JMZ8V1P9C5" },
		"no shard":         func(m *bus.TaskMessage) { m.Shard = nil },
		"another shard":    func(m *bus.TaskMessage) { m.Shard = &bus.Shard{Index: 4, Of: 8} },
		"no commit":        func(m *bus.TaskMessage) { m.Workflow = "monthly-invoicing" },
		"an empty commit":  func(m *bus.TaskMessage) { m.Workflow = "monthly-invoicing@" },
		"a local deadline": func(m *bus.TaskMessage) { m.Deadline = "2026-09-10 06:12" },
		"a bare timeout":   func(m *bus.TaskMessage) { m.Timeout = "600" },
		"another network":  func(m *bus.TaskMessage) { m.Network = "host" },
		"a key not a key":  func(m *bus.TaskMessage) { m.IdempotencyKey = "invoice" },
		"an item too many": func(m *bus.TaskMessage) { m.Inputs[0].Items = 2 },
		"a port it lacks":  func(m *bus.TaskMessage) { m.Inputs[0].Port = "orders" },
		"a port it misses": func(m *bus.TaskMessage) { m.Inputs = nil },
	} {
		t.Run(name, func(t *testing.T) {
			m := base
			m.Inputs = append([]bus.Input(nil), base.Inputs...)
			envelopes := envelopesFor(m)
			change(&m)
			if _, err := TaskOf(m, envelopes); err == nil {
				t.Error("the message was read back as a task")
			}
		})
	}
	if _, err := TaskOf(base, envelopesFor(base)); err != nil {
		t.Fatalf("the message itself is refused: %s", err)
	}
}

// The whole of it against the built-in store: the envelope is fetched through its URL and read
// back, the tree is laid out with its modes, the secret is the task's own and the store is its
// namespace's.
func TestATaskIsAssembledFromWhatItsRedemptionNames(t *testing.T) {
	s := newObjectStore(t)
	in := agk.Empty(storeRun, "fetch", "out", 1, time.Date(2026, 9, 10, 6, 0, 0, 0, time.UTC))
	in.Items = []agk.Item{agk.NewItem(map[string]any{"invoice": "INV-2026-0917"})}
	in.Meta.Count = 1
	m, r := s.taskFor(t, map[agk.Port]agk.Envelope{"in": in}, map[string]file{
		"agentiik.yaml":       {"version: 1\n", "0644"},
		"scripts/build.sh":    {"#!/bin/sh\nmake\n", "0755"},
		"config/rates.json":   {"{}\n", "644"},
		"config/copy-of.json": {"{}\n", "0600"},
	}, []RedeemedSecret{{Name: "billing", Mount: "/agk/secrets/billing", Encoding: "utf-8", Value: "bk_live_7Qm2rXt9vZa4"}})

	work := t.TempDir()
	a, err := Assemble(t.Context(), m, r, Assembly{WorkRoot: work})
	if err != nil {
		t.Fatalf("assembling the task: %s", err)
	}
	t.Cleanup(func() { a.Remove() })

	got := a.Task.Inputs["in"]
	if len(got.Items) != 1 || got.Items[0].Data["invoice"] != "INV-2026-0917" {
		t.Errorf("the input reads back as %+v", got.Items)
	}
	if a.Sources.Store == nil || a.Sources.Store.Namespace() != "finance" {
		t.Error("the task's store is not its namespace's")
	}
	if v, err := a.Sources.Secrets.Value(t.Context(), "billing"); err != nil || string(v) != "bk_live_7Qm2rXt9vZa4" {
		t.Errorf("the secret reads as %q: %v", v, err)
	}
	if filepath.Dir(a.Sources.Repo) != filepath.Join(work, TreesDir) {
		t.Errorf("the tree is laid out at %s, outside %s", a.Sources.Repo, TreesDir)
	}
	if b, err := os.ReadFile(filepath.Join(a.Sources.Repo, "config", "copy-of.json")); err != nil || string(b) != "{}\n" {
		t.Errorf("a file sharing another's bytes reads as %q: %v", b, err)
	}
	if n := s.gets.Load(); n != 4 {
		t.Errorf("the store was asked %d times for one envelope and three distinct files", n)
	}

	for path, want := range map[string]os.FileMode{
		".":                   treeDirMode,
		"config":              treeDirMode,
		"agentiik.yaml":       treeFileMode,
		"scripts/build.sh":    treeExecMode,
		"config/rates.json":   treeFileMode,
		"config/copy-of.json": treeFileMode,
	} {
		info, err := os.Stat(filepath.Join(a.Sources.Repo, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s is %o, want %o", path, got, want)
		}
	}

	if err := a.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.Sources.Repo); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the tree is still there once removed: %v", err)
	}
}

// An envelope whose bytes are not its digest, or that holds another count of items than the
// controller published, and a tree file whose bytes are not its digest, are each refused as
// ErrNotAsNamed, and leave no tree behind.
func TestATamperedInputOrTreeFileIsRefused(t *testing.T) {
	in := agk.Empty(storeRun, "fetch", "out", 1, time.Date(2026, 9, 10, 6, 0, 0, 0, time.UTC))
	in.Items = []agk.Item{agk.NewItem(map[string]any{"invoice": "INV-2026-0917"})}
	in.Meta.Count = 1

	for name, tamper := range map[string]func(*testing.T, *objectStore, *bus.TaskMessage, *Redemption){
		"an envelope with other bytes": func(t *testing.T, s *objectStore, m *bus.TaskMessage, r *Redemption) {
			other := in
			other.Items = []agk.Item{agk.NewItem(map[string]any{"invoice": "INV-2026-0918"})}
			var b strings.Builder
			if _, err := other.Encode(&b); err != nil {
				t.Fatal(err)
			}
			hex, _ := hexOf(m.Inputs[0].Digest)
			s.tamper(t, hex, []byte(b.String()))
		},
		"an envelope of another count": func(t *testing.T, s *objectStore, m *bus.TaskMessage, r *Redemption) {
			m.Inputs[0].Items = 2
		},
		"a tree file with other bytes": func(t *testing.T, s *objectStore, m *bus.TaskMessage, r *Redemption) {
			s.tamper(t, r.Tree[0].SHA256, []byte("version: 2\n"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newObjectStore(t)
			m, r := s.taskFor(t, map[agk.Port]agk.Envelope{"in": in}, map[string]file{"agentiik.yaml": {"version: 1\n", "0644"}}, nil)
			tamper(t, s, &m, &r)
			work := t.TempDir()
			_, err := Assemble(t.Context(), m, r, Assembly{WorkRoot: work})
			if !errors.Is(err, ErrNotAsNamed) {
				t.Fatalf("assembling answered %v", err)
			}
			if left, _ := os.ReadDir(filepath.Join(work, TreesDir)); len(left) != 0 {
				t.Errorf("a refused task left its tree behind: %v", left)
			}
		})
	}
}

// Assembling is the inverse of messageOf on the fields the wire carries, which bus/control holds
// with the controller's own writer; here, a task written by hand survives the message it would
// be published as.
func TestATaskSurvivesTheMessageItIsWrittenAs(t *testing.T) {
	var m bus.TaskMessage
	readFixture(t, "fixtures/wire/valid/task-message-script-step.json", &m)
	task, err := TaskOf(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := graph.Task{
		ID: "01JMZ8V1P9C4XQ7K2N4D6F8H0A/build/1", Run: "01JMZ8V1P9C4XQ7K2N4D6F8H0A",
		Workflow: "monthly-invoicing", Namespace: "finance", Commit: "a3f9c1e",
		Step: "build", Attempt: 1, Image: m.Image,
		Script: m.Script, BeforeScript: m.BeforeScript, AfterScript: m.AfterScript, Shell: m.Shell,
		Params:    map[string]any{"currency": "EUR"},
		Outputs:   []agk.Port{"ok"},
		Files:     []graph.FileSelector{{From: "config/rates.json", To: "/agk/files/rates.json", Mode: "0444"}},
		Resources: graph.Resources{CPU: "1", Memory: "512Mi", PIDs: 256},
		Network:   graph.NetworkNone, RunsOn: []string{"arch=amd64"},
		Timeout:    graph.Duration(10 * time.Minute),
		Deadline:   time.Date(2026, 9, 10, 6, 10, 0, 0, time.UTC),
		Idempotent: true, CacheKey: "finance/sha256:1ab74e66/9f2c1d07",
	}
	if !reflect.DeepEqual(task, want) {
		t.Errorf("the task reads back as\n%+v\nwant\n%+v", task, want)
	}
}

// brickManifest is the manifest the fake daemon's brick carries.
const brickManifest = `apiVersion: agentiik.dev/v1
kind: Brick
metadata:
  name: invoice
  version: 1.0.0
spec:
  runtime:
    user: "65532:65532"
`

// taskLogs keeps each task's log apart, as a runner holding several tasks does.
type taskLogs struct {
	mu   sync.Mutex
	logs map[agk.TaskID]*strings.Builder
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func (l *taskLogs) OpenLog(_ context.Context, task agk.TaskID) (io.WriteCloser, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.logs == nil {
		l.logs = map[agk.TaskID]*strings.Builder{}
	}
	b := &strings.Builder{}
	l.logs[task] = b
	return nopWriteCloser{&lockedWriter{mu: &l.mu, b: b}}, nil
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *strings.Builder
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (l *taskLogs) of(task agk.TaskID) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.logs[task]; ok {
		return b.String()
	}
	return ""
}

// serverDriver is a driver as a server runner opens it, on a fake daemon: no store, no tree and no
// secret source of its own, so that a task answered from anything but its sources fails.
func serverDriver(t *testing.T, run func(dockertest.Container) (int, error)) (*driver.Docker, *taskLogs) {
	t.Helper()
	daemon, err := dockertest.NewDaemon(dockertest.With(dockertest.Options{
		Run:    run,
		Images: map[string]dockertest.Image{"ghcr.io/acme/agk-invoice@" + imageDigest: {Digest: imageDigest, Manifest: []byte(brickManifest)}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { daemon.Close() })
	// The fake daemon does not remap and the test has no tmpfs of its own to name, so both
	// floors are lifted, the way an operator lifts them on a machine that is not an
	// installation; neither is what this is about.
	policy := driver.DefaultPolicy()
	policy.RequireUsernsRemap = driver.RemapLifted
	policy.RequireSecretsTmpfs = driver.SecretsTmpfsLifted
	policy.SecretsDir = ""
	policy.StopGrace = 200 * time.Millisecond
	logs := &taskLogs{}
	d, err := driver.New(driver.Config{
		Socket:   daemon.Socket(),
		WorkRoot: t.TempDir(),
		Policy:   policy,
		Logs:     logs,
		Host:     installed{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, logs
}

// A value that is not text travels as base64. The container is given the bytes it stands for, and
// a container printing them has them masked in its log, which a masker holding the base64 text
// would miss.
func TestABase64SecretIsMountedAndMaskedInItsDecodedForm(t *testing.T) {
	value := []byte("ks\xff\xfe-keystore-pass-7Qm2")
	encoded := base64.StdEncoding.EncodeToString(value)

	var mounted []byte
	d, logs := serverDriver(t, func(c dockertest.Container) (int, error) {
		m, ok := c.Mount("/agk/secrets/keystore")
		if !ok {
			return 1, errors.New("nothing is mounted at /agk/secrets/keystore")
		}
		b, err := os.ReadFile(m.Source)
		if err != nil {
			return 1, err
		}
		mounted = b
		fmt.Fprintf(c.Stderr, "opening the keystore with %s\n", b)
		fmt.Fprintf(c.Stderr, "and as it travelled, %s\n", encoded)
		return 0, nil
	})

	s := newObjectStore(t)
	m, r := s.taskFor(t, nil, map[string]file{"agentiik.yaml": {"version: 1\n", "0644"}},
		[]RedeemedSecret{{Name: "keystore", Mount: "/agk/secrets/keystore", Encoding: "base64", Value: encoded}})
	a, err := Assemble(t.Context(), m, r, Assembly{WorkRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Remove() })

	result, err := d.Run(a.Context(t.Context()), a.Task)
	if err != nil {
		t.Fatalf("running the task: %s", err)
	}
	if result.State != agk.TaskSucceeded {
		t.Fatalf("the task is %s", result.State)
	}
	if !bytes.Equal(mounted, value) {
		t.Errorf("the container was given %q, and the value is %q", mounted, value)
	}
	log := logs.of(a.Task.ID)
	if strings.Contains(log, string(value)) || strings.Contains(log, "-keystore-pass-7Qm2") {
		t.Errorf("the decoded value reached the log:\n%s", log)
	}
	if !strings.Contains(log, "opening the keystore with [masked]") {
		t.Errorf("the log does not show the value masked:\n%s", log)
	}
}

// An artifact whose bytes are not its digest is refused as the driver lays the inputs down, before
// the container is created, which is the one check of an input Assemble leaves to the driver.
func TestATamperedArtifactIsRefusedBeforeAnyContainerExists(t *testing.T) {
	var created atomic.Int64
	d, _ := serverDriver(t, func(dockertest.Container) (int, error) {
		created.Add(1)
		return 0, nil
	})
	s := newObjectStore(t)
	pdf := []byte("%PDF-1.7 the whole of an invoice")
	digest := s.put(t, pdf)
	in := agk.Empty(storeRun, "fetch", "out", 1, time.Date(2026, 9, 10, 6, 0, 0, 0, time.UTC))
	item := agk.NewItem(map[string]any{"invoice": "INV-2026-0917"})
	item.Files = []agk.File{{
		Name: "invoice.pdf", URI: agk.URI{Run: storeRun, Step: "fetch", Port: "out", Name: "invoice.pdf"},
		MediaType: "application/pdf", Size: int64(len(pdf)), SHA256: digest,
	}}
	in.Items = []agk.Item{item}
	in.Meta.Count = 1
	m, r := s.taskFor(t, map[agk.Port]agk.Envelope{"in": in}, map[string]file{"agentiik.yaml": {"version: 1\n", "0644"}}, nil)
	s.tamper(t, digest, []byte("%PDF-1.7 somebody else's invoice"))

	a, err := Assemble(t.Context(), m, r, Assembly{WorkRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("assembling the task: %s", err)
	}
	t.Cleanup(func() { a.Remove() })
	_, err = d.Run(a.Context(t.Context()), a.Task)
	if err == nil {
		t.Fatal("a task whose artifact is not its digest ran")
	}
	if !strings.Contains(err.Error(), "holds sha256 "+sha([]byte("%PDF-1.7 somebody else's invoice"))) {
		t.Errorf("the task was refused for something else: %s", err)
	}
	if n := created.Load(); n != 0 {
		t.Errorf("%d containers ran for a task whose artifact is not its digest", n)
	}
}
