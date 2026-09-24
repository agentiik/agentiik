package local

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// This file is the one that needs a daemon that is actually there. It is skipped where there
// is none, so that a machine with nothing installed still runs the rest, and it runs where
// there is one, failing rather than skipping where AGENTIIK_TEST_REQUIRE_DOCKER is 1, exactly
// as driver/real_test.go does.
//
// What it holds that the fake driver cannot is that the loop, the working directory and the
// four things the driver asks for work against real containers: the tree is bound at
// /agk/repo, a secret supplied on the command line is readable at /agk/secrets/<name>, a
// fan-out is three containers and not one, a merge concatenates what they published, and
// every envelope that came back was collected by the driver out of /agk/out/ports.

// realSession opens a session on the daemon of this machine, or ends the test through
// dockertest.Unavailable.
func realSession(t *testing.T, tree string) (*Session, Layout) {
	t.Helper()
	socket, ok := dockertest.Socket()
	if !ok {
		dockertest.Unavailable(t, "no Docker daemon on this machine")
	}
	cli, err := docker.Dial(socket)
	if err != nil {
		dockertest.Unavailable(t, "the daemon at %s did not answer: %v", socket, err)
	}
	_, err = cli.ImageInspect(t.Context(), realImage)
	cli.Close()
	if err != nil {
		dockertest.Unavailable(t, "%s is not on this machine: docker pull %s", realImage, realImage)
	}

	layout, err := NewLayout(filepath.Join(tree, DefaultDir))
	if err != nil {
		t.Fatalf("the layout: %s", err)
	}
	session, err := Open(t.Context(), Daemon{
		Socket:   socket,
		Announce: func(s string) { t.Log(s) },
	}, layout)
	if err != nil {
		dockertest.Unavailable(t, "opening a session on %s: %v", socket, err)
	}
	t.Cleanup(func() { session.Close() })
	return session, layout
}

// realImage is the base image every step of the fixture runs in. It is pinned and small, and
// it is the one already on this machine, so nothing is pulled; CI pulls it before the tests.
const realImage = "alpine:3.21"

// realWorkflow is a fan-out and a merge, in script steps, with one step that is given a
// secret.
//
// Every identity it writes is derived from the payload it came from, so two runs over the same
// inputs publish the same items. The standard-output shorthand is avoided for that reason: its
// item identity is minted and a minted identity is a different envelope every time.
const realWorkflow = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
secrets: [billing_api]
outputs:
  charged: { from: { step: collect, port: out } }
  signed: { from: { step: sign, port: out } }
steps:
  seed:
    image: alpine:3.21
    outputs: [out]
    script:
      - |
        test -d /agk/repo
        test -f /agk/repo/agentiik.yaml
        printf '{"meta":{"run_id":"%s","step":"%s","port":"out","attempt":%s,"count":3,"produced_at":"2026-01-01T00:00:00Z"},"items":[{"id":"order-1","data":{"n":1},"files":[]},{"id":"order-2","data":{"n":2},"files":[]},{"id":"order-3","data":{"n":3},"files":[]}]}' \
          "$AGK_RUN_ID" "$AGK_STEP" "$AGK_ATTEMPT" > /agk/out/ports/out.json
  fan:
    image: alpine:3.21
    needs:
      - { step: seed, port: out, as: in }
    strategy: { fan_out: item }
    outputs: [out]
    script:
      - |
        id=$(sed -n 's/.*{"id":"\([^"]*\)".*/\1/p' /agk/in/in/envelope.json)
        printf '{"meta":{"run_id":"%s","step":"%s","port":"out","attempt":%s,"count":1,"produced_at":"2026-01-01T00:00:00Z"},"items":[{"id":"charged-%s","data":{"from":"%s"},"files":[]}]}' \
          "$AGK_RUN_ID" "$AGK_STEP" "$AGK_ATTEMPT" "$id" "$id" > /agk/out/ports/out.json
  collect:
    image: alpine:3.21
    needs:
      - { step: fan, port: out, as: in }
    outputs: [out]
    script:
      - |
        n=$(grep -o '{"id":"' /agk/in/in/envelope.json | wc -l | tr -d ' ')
        printf '{"meta":{"run_id":"%s","step":"%s","port":"out","attempt":%s,"count":1,"produced_at":"2026-01-01T00:00:00Z"},"items":[{"id":"charged-all","data":{"charged":%s},"files":[]}]}' \
          "$AGK_RUN_ID" "$AGK_STEP" "$AGK_ATTEMPT" "$n" > /agk/out/ports/out.json
  sign:
    image: alpine:3.21
    secrets: [billing_api]
    outputs: [out]
    script:
      - |
        if echo x >> /agk/secrets/billing_api 2>/dev/null; then
          echo "the secret is bound read-write" >&2
          exit 1
        fi
        bytes=$(wc -c < /agk/secrets/billing_api | tr -d ' ')
        printf '{"meta":{"run_id":"%s","step":"%s","port":"out","attempt":%s,"count":1,"produced_at":"2026-01-01T00:00:00Z"},"items":[{"id":"signature","data":{"secret_bytes":%s},"files":[]}]}' \
          "$AGK_RUN_ID" "$AGK_STEP" "$AGK_ATTEMPT" "$bytes" > /agk/out/ports/out.json
`

func TestARealFanOutAndMergeRunsThroughTheLoop(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "agentiik.yaml"), []byte(realWorkflow), 0o644); err != nil {
		t.Fatalf("writing the entry point: %s", err)
	}
	session, layout := realSession(t, tree)

	wf, err := graph.Load(os.DirFS(tree), "agentiik.yaml", nil)
	if err != nil {
		t.Fatalf("loading the workflow: %s", err)
	}
	g, err := session.Resolve(t.Context(), wf)
	if err != nil {
		t.Fatalf("resolving the graph: %s", err)
	}

	out, err := session.Run(t.Context(), Request{
		Graph:   g,
		Tree:    tree,
		Secrets: map[string][]byte{"billing_api": []byte("sk-1")},
		Events:  func(e Event) { t.Logf("%s %s %s %s", e.At, e.Step, e.State, e.Verdict) },
	})
	if err != nil {
		t.Fatalf("running the graph: %s", err)
	}
	if out.Run.State != agk.Succeeded {
		t.Fatalf("the run is %s: %+v", out.Run.State, out.Failures)
	}

	// Three containers and not one. A run that quietly collapsed a fan-out to a single
	// container would otherwise pass everything below.
	if n := len(out.State.Steps["fan"].Shards); n != 3 {
		t.Fatalf("the fan-out ran %d shards, want one per item", n)
	}

	charged, ok := out.Outputs["charged"]
	if !ok || len(charged.Items) != 1 {
		t.Fatalf("the output charged is %+v", charged)
	}
	// A number read out of an envelope is a json.Number, because package agk decodes with
	// UseNumber so that a payload travels back out as it came in.
	if got := fmt.Sprint(charged.Items[0].Data["charged"]); got != "3" {
		t.Errorf("the merge handed collect %s items, want the three the fan-out published", got)
	}
	if got := charged.Items[0].ID; got != "charged-all" {
		t.Errorf("the item is %s, want the identity the script derived", got)
	}

	// The secret was mounted exactly as a server run mounts one: a file at
	// /agk/secrets/<name>, read-only, carrying the bytes the command line supplied.
	signed, ok := out.Outputs["signed"]
	if !ok || len(signed.Items) != 1 {
		t.Fatalf("the output signed is %+v", signed)
	}
	if got := fmt.Sprint(signed.Items[0].Data["secret_bytes"]); got != "4" {
		t.Errorf("the container read %s bytes at /agk/secrets/billing_api, want the 4 of sk-1", got)
	}

	// Everything a run writes is where the layout says it is.
	for _, path := range []string{
		layout.RunFile(out.Run.ID),
		layout.State(out.Run.ID),
		filepath.Join(layout.Outputs(out.Run.ID), "charged.json"),
		filepath.Join(layout.Outputs(out.Run.ID), "signed.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s is not there: %s", path, err)
		}
	}
	// One log per task, named by the task, which is what a failure report points at.
	for _, shard := range out.State.Steps["fan"].Shards {
		id := agk.NewTaskID(out.Run.ID, "fan", shard.Attempt, shard.Shard)
		path, err := layout.Log(id)
		if err != nil {
			t.Fatalf("naming the log of %s: %s", id, err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the log of %s is not at %s: %s", id, path, err)
		}
	}
	// And nothing a task was given survives under the work root, because the driver removes
	// a task's directory with its container: "no residue of one namespace survives into the
	// next task on that host". The empty parents it was nested under are left behind, which
	// is a directory and not a residue; a file is the thing to fail on, because the files are
	// the envelopes, the parameters and the secret values.
	//
	// The record of the keys that ended is the one file meant to outlive them, and it is
	// passed over: it holds a key, a state and a moment, and the driver's own test holds that
	// it never holds a payload.
	keys := filepath.Join(layout.WorkRoot(), driver.KeysDir)
	filepath.WalkDir(layout.WorkRoot(), func(path string, d fs.DirEntry, err error) error {
		if path == keys {
			return fs.SkipDir
		}
		if err != nil || d.IsDir() {
			return nil
		}
		t.Errorf("%s survived the run: a task's working directory is removed with its container", path)
		return nil
	})
}

func TestARealScriptThatFailsIsReportedWithItsCodeAndItsLog(t *testing.T) {
	const doc = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: one, namespace: finance }
steps:
  only:
    image: alpine:3.21
    outputs: [out]
    script:
      - |
        echo "the last thing the container said" >&2
        exit 17
`
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "agentiik.yaml"), []byte(doc), 0o644); err != nil {
		t.Fatalf("writing the entry point: %s", err)
	}
	session, _ := realSession(t, tree)

	wf, err := graph.Load(os.DirFS(tree), "agentiik.yaml", nil)
	if err != nil {
		t.Fatalf("loading the workflow: %s", err)
	}
	g, err := session.Resolve(t.Context(), wf)
	if err != nil {
		t.Fatalf("resolving the graph: %s", err)
	}
	out, err := session.Run(t.Context(), Request{Graph: g, Tree: tree})
	if err != nil {
		t.Fatalf("running the graph: %s", err)
	}
	if out.Run.State != agk.Failed {
		t.Fatalf("the run is %s, want failed", out.Run.State)
	}
	if len(out.Failures) != 1 {
		t.Fatalf("%d failures reported: %+v", len(out.Failures), out.Failures)
	}
	f := out.Failures[0]
	if !f.HasExit || f.ExitCode != 17 {
		t.Errorf("the failure is %+v, want exit code 17 out of the container", f)
	}
	if f.Band != agk.BandApplicationFailure {
		t.Errorf("the band is %s, want the application failure row", f.Band)
	}
	if f.Log == "" {
		t.Fatalf("the failure names no log, and the report prints the last lines of one")
	}
	doc2, err := os.ReadFile(f.Log)
	if err != nil {
		t.Fatalf("reading the log the failure names: %s", err)
	}
	if !strings.Contains(string(doc2), "the last thing the container said") {
		t.Errorf("the log is %q, and it has to carry what the container wrote", doc2)
	}
}
