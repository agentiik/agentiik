package e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// smokeWorkflow is the one-step workflow the smoke test pushes, naming the brick by the tag it was
// pushed under, which the push resolves to its digest as agk push does.
func smokeWorkflow(image string) string {
	return `apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: smoke, namespace: ` + Namespace + ` }
inputs:
  orders: { schema: { type: array }, required: true }
outputs:
  counted: { from: { step: count, port: ok } }
steps:
  count:
    image: ` + image + `
    runs_on: [` + Label + `]
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok]
`
}

// The smoke test, and the first half of the fact v0.2.0 holds: "a runner never speaks git, holds
// no lasting credential beyond its own runner identity, and obtains what its task names only by
// redeeming that task's grant".
//
// A one-step workflow is pushed and started with the operator token, through the API alone, and
// runs to succeeded on one of the two runners, which pulled the brick by digest from the registry,
// fetched its input and stored its output and its artifact through URLs the API signed. Then each
// runner is read from outside: what its two directories hold, its environment and its mounts.
// Each holds AGK_API, its runner.env and its key, and no database URL, no store credential or
// key, no bus credential at rest and no repository; and every request either of them made was a
// runner route, or an object through a presigned URL carrying no credential.
func TestAOneStepWorkflowRunsToSucceededAndEachRunnerHoldsItsOwnIdentityAlone(t *testing.T) {
	in := Stand(t)

	tag, pinned := in.Brick("count")
	manifest, err := os.ReadFile("testdata/bricks/count/brick.yaml")
	if err != nil {
		t.Fatal(err)
	}
	document := smokeWorkflow(tag)
	commit := randomHex(20)
	in.Push("smoke", commit, document, map[string][]byte{tag: manifest}, map[string]string{tag: pinned})
	run := in.Start("smoke", commit, map[string]any{
		"orders": []any{map[string]any{"ref": "a"}, map[string]any{"ref": "b"}, map[string]any{"ref": "c"}},
	})
	ended := in.Wait(run, 3*time.Minute)
	if ended.State != "succeeded" {
		t.Fatalf("run %s ended %s: %s", run, ended.State, ended.Answer)
	}

	// It ran on one of the runners, and on one alone.
	var ran []string
	for _, task := range ended.Tasks {
		ran = append(ran, task.Runner)
	}
	joined := []string{in.Runners[0].ID, in.Runners[1].ID}
	if len(ran) != 1 || !slices.Contains(joined, ran[0]) {
		t.Errorf("the run's tasks ran on %q, and it is one task on one of %q", ran, joined)
	}

	// What it produced is what the brick computed from the three orders it fetched.
	var counted struct {
		Items []struct {
			ID   string         `json:"id"`
			Data map[string]any `json:"data"`
			Fs   []struct {
				Name string `json:"name"`
			} `json:"files"`
		} `json:"items"`
	}
	in.Operator("GET", "/api/v1/runs/"+run+"/outputs/counted", nil, http.StatusOK, &counted)
	if len(counted.Items) != 1 || counted.Items[0].Data["orders"] != float64(3) || len(counted.Items[0].Fs) != 1 {
		t.Errorf("the output counted is %+v, and the brick publishes one item counting three orders, with its report", counted)
	}

	// Every request a runner made: runner routes, and objects through presigned URLs and the
	// signed upload policy, both ways.
	broken, objects := outsideTheOperator(in.Requests.All())
	for _, b := range broken {
		t.Error(b)
	}
	if objects.Reads == 0 || objects.Writes == 0 {
		t.Errorf("the runners read %d objects and wrote %d, and a task fetches its input through a presigned URL and stores its output through the signed upload policy", objects.Reads, objects.Writes)
	}

	// What each runner holds at rest, once the task's own directory is gone. A task's
	// directory holds the tree while its brick runs, and is removed when it has ended, which
	// may be a moment after the run is read as succeeded.
	forbidden := []string{in.path("objects"), in.path("bus"), in.path("postgres"), in.path("secrets")}
	held := append(in.held, heldValue{"the workflow's repository", document})
	for _, r := range in.Runners {
		var last []string
		eventually(in.ctx, t, 30*time.Second, "runner "+r.Name+" held its identity alone", func() error {
			h, err := r.Holdings(in.ctx)
			if err != nil {
				return err
			}
			if last = h.breaches(in.PublicURL, held, forbidden); len(last) > 0 {
				return fmt.Errorf("%s", strings.Join(last, "; "))
			}
			return nil
		})
	}
}

// A first run as Get started makes one: a runner joins the pool default claiming no label, and a
// workflow whose step names no runs_on runs on it to succeeded, and on no runner of the labelled
// pool. Before, agk-runner join refused a host claiming no label, so no runner could join the pool
// default and such a step ran nowhere.
func TestAStepNamingNoRunsOnRunsOnARunnerOfThePoolDefaultClaimingNoLabel(t *testing.T) {
	in := Stand(t)
	c := in.JoinDefault("c")

	tag, pinned := in.Brick("count")
	manifest, err := os.ReadFile("testdata/bricks/count/brick.yaml")
	if err != nil {
		t.Fatal(err)
	}
	document := strings.Replace(smokeWorkflow(tag), "    runs_on: ["+Label+"]\n", "", 1)
	if strings.Contains(document, "runs_on") {
		t.Fatalf("the workflow still names runs_on:\n%s", document)
	}
	commit := randomHex(20)
	in.Push("smoke", commit, document, map[string][]byte{tag: manifest}, map[string]string{tag: pinned})
	run := in.Start("smoke", commit, map[string]any{"orders": []any{map[string]any{"ref": "a"}}})
	ended := in.Wait(run, 3*time.Minute)
	if ended.State != "succeeded" {
		t.Fatalf("run %s ended %s: %s", run, ended.State, ended.Answer)
	}
	for _, task := range ended.Tasks {
		if task.Runner != c.ID {
			t.Errorf("a task ran on %s, and a step naming no runs_on goes to the pool default, whose one runner is %s", task.Runner, c.ID)
		}
	}
}

func TestEveryRequestOutsideTheOperatorIsARunnerRouteOrAPresignedObject(t *testing.T) {
	signed := url.Values{"run": {"01JM"}, "expires": {"1790000000"}, "signature": {"ab12"}}
	requests := []Request{
		{Method: "PUT", Path: "/api/v1/e2e/workflows/smoke/versions/abc", Caller: CallerOperator, Status: 200},
		{Method: "POST", Path: "/api/v1/runners", Caller: CallerNobody, Status: 201},
		{Method: "POST", Path: "/api/v1/runners/heartbeat", Caller: CallerBearer, Status: 200},
		{Method: "POST", Path: "/api/v1/tasks/redeem", Caller: CallerBearer, Status: 200},
		{Method: "GET", Path: "/objects/e2e/sha256/aa", Query: signed, Caller: CallerNobody, Status: 200},
		{Method: "POST", Path: "/objects/e2e", Caller: CallerNobody, Status: 204},
	}
	broken, objects := outsideTheOperator(requests)
	if len(broken) != 0 || objects != (Objects{Reads: 1, Writes: 1}) {
		t.Fatalf("a runner that kept to its routes broke %q and did %+v", broken, objects)
	}

	for _, c := range []struct {
		name string
		r    Request
		says string
	}{
		{"an operator route asked as a runner", Request{Method: "GET", Path: "/api/v1/e2e/runs", Caller: CallerBearer, Status: 200}, "neither a runner route nor an object"},
		{"a heartbeat asked as nobody", Request{Method: "POST", Path: "/api/v1/runners/heartbeat", Caller: CallerNobody, Status: 401}, "a runner asks it as bearer"},
		{"an object read carrying a credential", Request{Method: "GET", Path: "/objects/e2e/sha256/aa", Query: signed, Caller: CallerBearer, Status: 200}, "carried a credential"},
		{"an object read with no signature", Request{Method: "GET", Path: "/objects/e2e/sha256/aa", Caller: CallerNobody, Status: 200}, "no run, expires and signature"},
		{"an object read the API refused", Request{Method: "GET", Path: "/objects/e2e/sha256/aa", Query: signed, Caller: CallerNobody, Status: 403}, "was answered 403"},
	} {
		broken, _ := outsideTheOperator(append(requests, c.r))
		if len(broken) != 1 || !strings.Contains(broken[0], c.says) {
			t.Errorf("%s: broke %q, want one saying %q", c.name, broken, c.says)
		}
	}
}

func TestTheCallerIsToldFromTheAuthorizationHeader(t *testing.T) {
	for _, c := range []struct {
		header string
		want   Caller
	}{
		{"", CallerNobody},
		{"Bearer agk_op_the-token", CallerOperator},
		{"Bearer agkrunner_abc", CallerBearer},
		{"Basic dXNlcjpwYXNz", CallerOther},
	} {
		if got := callerOf(c.header, "agk_op_the-token"); got != c.want {
			t.Errorf("%q is %s, want %s", c.header, got, c.want)
		}
	}
}

func TestTheRunnerIsReadOutOfWhatJoinSaid(t *testing.T) {
	said := "This host joined pool e2e as runner rn-01jm8v1p9c.\nIts key is in /var/lib/agentiik/runner.key\n"
	if got := joinedAs(said); got != "rn-01jm8v1p9c" {
		t.Errorf("join said %q, and it was read as runner %q", said, got)
	}
	if got := joinedAs("agk-runner join: refused"); got != "" {
		t.Errorf("a refusal was read as runner %q", got)
	}
}

// aRunnerAtRest is what a runner that keeps to its identity holds.
func aRunnerAtRest() Holdings {
	return Holdings{
		Files: map[string][]byte{
			"/etc/agentiik/runner.env":             []byte("AGK_API=https://127.0.0.1:8443\nAGK_RUNNER_ID=rn-1\nAGK_RUNNER_POOL=e2e\nAGK_RUNNER_LABELS=zone=e2e\nAGK_RUNNER_CREDENTIAL=agkrunner_abc\n"),
			"/etc/agentiik/runner.toml":            []byte(runnerPolicy),
			"/var/lib/agentiik/runner.key":         []byte("-----BEGIN PRIVATE KEY-----\nMC4CAQAwBQYDK2VwBCIEIA==\n-----END PRIVATE KEY-----\n"),
			"/var/lib/agentiik/work/.bin/agk":      []byte("\x7fELF"),
			"/var/lib/agentiik/work/.keys/01JM.ok": []byte(`{"state":"succeeded"}`),
		},
		Directories: []string{"/etc/agentiik", "/var/lib/agentiik", "/var/lib/agentiik/work"},
		// A host setting in the environment, as the page's Compose sample sets it.
		Env:   []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "AGK_RUNNER_CONCURRENCY=4"},
		Added: []string{"/run", "/run/agentiik", "/var/run"},
		Mounts: []Mount{
			{Type: "bind", Source: "/tmp/agk-e2e-1/runner-a/etc", Destination: "/etc/agentiik"},
			{Type: "volume", Source: "/var/lib/docker/volumes/agk-e2e-1-lib-a/_data", Destination: "/var/lib/agentiik"},
			{Type: "volume", Source: "/var/lib/docker/volumes/agk-e2e-1-secrets-a/_data", Destination: "/run/agentiik/secrets"},
			{Type: "volume", Source: "/var/lib/docker/volumes/agk-e2e-1-socket-a/_data", Destination: "/var/run"},
			{Type: "bind", Source: "/tmp/agk-e2e-1/tls/ca.pem", Destination: "/etc/ssl/certs/ca-certificates.crt"},
		},
	}
}

func TestARunnerHoldingItsIdentityAloneBreaksNothingAndEveryOtherHoldingIsNamed(t *testing.T) {
	const public = "https://127.0.0.1:8443"
	held := []heldValue{
		{"the presign key", "cHJlc2lnbi1rZXk="},
		{"the database URL", "postgres://agentiik@/agentiik?host=/tmp/agk-e2e-1/postgres"},
		{"the workflow's repository", "apiVersion: agentiik.dev/v1\nkind: Workflow\n"},
	}
	forbidden := []string{"/tmp/agk-e2e-1/objects", "/tmp/agk-e2e-1/bus"}
	if broken := aRunnerAtRest().breaches(public, held, forbidden); len(broken) != 0 {
		t.Fatalf("a runner holding its identity alone broke %q", broken)
	}

	for _, c := range []struct {
		name   string
		change func(*Holdings)
		says   string
	}{
		{"a file beside runner.env", func(h *Holdings) { h.Files["/etc/agentiik/database.env"] = nil }, "holds runner.env and runner.toml alone"},
		{"no runner.env", func(h *Holdings) { delete(h.Files, "/etc/agentiik/runner.env") }, "there is no runner.env"},
		{"a database URL in runner.env", func(h *Holdings) {
			h.Files["/etc/agentiik/runner.env"] = append(h.Files["/etc/agentiik/runner.env"], []byte("AGK_DATABASE_URL=x\n")...)
		}, "sets AGK_DATABASE_URL"},
		{"another address of the API", func(h *Holdings) {
			h.Files["/etc/agentiik/runner.env"] = []byte("AGK_API=http://127.0.0.1:8080\nAGK_RUNNER_CREDENTIAL=agkrunner_abc\n")
		}, "the one address a runner is given"},
		{"no credential", func(h *Holdings) {
			h.Files["/etc/agentiik/runner.env"] = []byte("AGK_API=" + public + "\n")
		}, "no runner credential"},
		{"no key", func(h *Holdings) { delete(h.Files, "/var/lib/agentiik/runner.key") }, "the host's key"},
		{"a repository", func(h *Holdings) { h.Directories = append(h.Directories, "/var/lib/agentiik/work/t1/repo/.git") }, "never holds a repository"},
		{"the tree left behind", func(h *Holdings) { h.Files["/var/lib/agentiik/work/t1/repo/agentiik.yaml"] = nil }, "never holds a repository"},
		{"the workflow's text", func(h *Holdings) {
			h.Files["/var/lib/agentiik/work/t1/copy"] = []byte("apiVersion: agentiik.dev/v1\nkind: Workflow\nsteps: {}\n")
		}, "holds the workflow's repository"},
		{"the presign key", func(h *Holdings) { h.Files["/var/lib/agentiik/work/leak"] = []byte("key=cHJlc2lnbi1rZXk=") }, "holds the presign key"},
		{"a bus credential file", func(h *Holdings) {
			h.Files["/var/lib/agentiik/bus.creds"] = []byte("-----BEGIN NATS USER JWT-----\n")
		}, "a NATS credential file"},
		{"a bare JWT", func(h *Holdings) {
			h.Files["/var/lib/agentiik/bus.jwt"] = []byte("eyJ0eXAiOiJKV1QiLCJhbGciOiJlZDI1NTE5LW5rZXkifQ.e30.sig")
		}, "a JWT"},
		{"a user seed", func(h *Holdings) {
			h.Files["/var/lib/agentiik/bus.seed"] = []byte("SUAMLK2ZNL35WSMW37E7UD4VZ7ELPKW7DHC3BWBSD2GCZ7IUQQXZIORRBU")
		}, "an NKey seed"},
		{"the credential in the environment", func(h *Holdings) { h.Env = append(h.Env, "AGK_RUNNER_CREDENTIAL=agkrunner_abc") }, "the agent's environment sets AGK_RUNNER_CREDENTIAL"},
		{"an installation setting in the environment", func(h *Holdings) { h.Env = append(h.Env, "AGK_BUS_URL=tls://127.0.0.1:4222") }, "sets AGK_BUS_URL, which is not a runner's setting"},
		{"the database URL in the environment", func(h *Holdings) {
			h.Env = append(h.Env, "DB=postgres://agentiik@/agentiik?host=/tmp/agk-e2e-1/postgres")
		}, "the agent's environment holds the database URL"},
		{"the object store mounted", func(h *Holdings) {
			h.Mounts = append(h.Mounts, Mount{Type: "bind", Source: "/tmp/agk-e2e-1/objects", Destination: "/var/lib/agentiik/objects"})
		}, "no runner sees"},
		{"a file written outside the two directories", func(h *Holdings) { h.Added = append(h.Added, "/tmp/bus.creds") }, "written in the agent's own filesystem"},
		{"a value left on the tmpfs", func(h *Holdings) { h.Secrets = append(h.Secrets, "/run/agentiik/secrets/t1/billing") }, "still on the secrets tmpfs"},
		{"something else mounted", func(h *Holdings) {
			h.Mounts = append(h.Mounts, Mount{Type: "bind", Source: "/home", Destination: "/home"})
		}, "which the agent is not given"},
	} {
		h := aRunnerAtRest()
		c.change(&h)
		broken := h.breaches(public, held, forbidden)
		if !slices.ContainsFunc(broken, func(b string) bool { return strings.Contains(b, c.says) }) {
			t.Errorf("%s: broke %q, want one saying %q", c.name, broken, c.says)
		}
	}
}

func TestTheRecorderKeepsARequestWhoseAnswerWasCutAndTheStatusAfterAContinue(t *testing.T) {
	r := &Requests{operator: "agk_op_the-token"}
	handler := r.recording(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/objects/e2e/sha256/cut":
			panic(http.ErrAbortHandler)
		case "/objects/e2e":
			w.WriteHeader(http.StatusContinue)
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	for _, path := range []string{"/objects/e2e/sha256/cut", "/objects/e2e", "/api/v1/runners/heartbeat"} {
		func() {
			defer func() { recover() }()
			method := "GET"
			if path != "/objects/e2e/sha256/cut" {
				method = "POST"
			}
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, nil))
		}()
	}
	got := r.All()
	if len(got) != 3 || got[0].Status != 0 || got[1].Status != http.StatusForbidden || got[2].Status != http.StatusOK {
		t.Fatalf("the recorder kept %+v, want the cut request with no status, the upload as 403 and the empty answer as 200", got)
	}
	broken, _ := outsideTheOperator(got)
	if !slices.ContainsFunc(broken, func(b string) bool { return strings.Contains(b, "never answered") }) ||
		!slices.ContainsFunc(broken, func(b string) bool { return strings.Contains(b, "answered 403") }) {
		t.Errorf("the cut read and the refused upload broke %q", broken)
	}
}
