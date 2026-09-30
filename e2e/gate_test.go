package e2e

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The gate of v0.5.0: "A scheduled workflow runs with nobody present, a webhook whose signature does
// not verify starts nothing, and every triggered run names its trigger and its commit."
//
// The namespace's built-in identity is given operator, as an owner decides what unattended runs may
// do; a workflow declaring a schedule every minute and a webhook authenticating by hmac is pushed
// with the operator token, and the webhook's secret written; and then nobody asks for a run. Within two minutes the
// leading controller has started one on the schedule, which runs to succeeded on a runner. A request
// to the webhook signed with another secret is refused and starts nothing; one signed with the
// secret written starts a run, whose map fills its orders from the body. Every run of the workflow
// names its trigger kind, the built-in identity it is attributed to, and the commit pushed.
func TestAScheduleRunsWithNobodyPresentAndABadSignatureStartsNothing(t *testing.T) {
	in := Stand(t)
	tag, pinned := in.Brick("count")
	manifest, err := os.ReadFile("testdata/bricks/count/brick.yaml")
	if err != nil {
		t.Fatal(err)
	}
	document := `apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: gate, namespace: ` + Namespace + ` }
inputs:
  orders: { schema: { type: array }, default: [{ ref: scheduled }] }
on:
  schedule:
    - cron: "* * * * *"
  webhook:
    - path: /gate
      map:
        orders: ${{ trigger.body.orders }}
outputs:
  counted: { from: { step: count, port: ok } }
steps:
  count:
    image: ` + tag + `
    runs_on: [` + Label + `]
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok]
`
	commit := randomHex(20)
	in.Operator("POST", "/api/v1/"+Namespace+"/grants", map[string]string{"principal": Namespace + "/agentiik", "role": "operator"}, http.StatusCreated, nil)
	in.Push("gate", commit, document, map[string][]byte{tag: manifest}, map[string]string{tag: pinned})
	key := []byte(randomHex(16))
	in.Operator("PUT", "/api/v1/"+Namespace+"/workflows/gate/webhooks/POST/gate", map[string]string{"secret": "whsec_" + base64.StdEncoding.EncodeToString(key)}, http.StatusNoContent, nil)

	// Nobody asks: the schedule does.
	var scheduled string
	eventually(in.ctx, t, 2*time.Minute+30*time.Second, "the schedule started a run", func() error {
		runs := in.runsOf("gate")
		for _, r := range runs {
			if r.TriggerKind == "schedule" {
				scheduled = r.Run
				return nil
			}
		}
		return fmt.Errorf("the workflow has the runs %+v", runs)
	})
	if ended := in.Wait(scheduled, 3*time.Minute); ended.State != "succeeded" {
		t.Fatalf("the scheduled run %s ended %s: %s %v", scheduled, ended.State, ended.Answer, ended.Reasons)
	}

	// A request signed with another secret starts nothing; one signed with the secret written does.
	body := `{"orders":[{"ref":"a"},{"ref":"b"}]}`
	if code, answer := in.hook("/gate", body, []byte(randomHex(16))); code != http.StatusUnauthorized {
		t.Fatalf("a request signed with another secret answered %d: %s", code, answer)
	}
	for _, r := range in.runsOf("gate") {
		if r.TriggerKind == "webhook" {
			t.Fatalf("a request whose signature does not verify started %+v", r)
		}
	}
	code, answer := in.hook("/gate", body, key)
	if code != http.StatusAccepted {
		t.Fatalf("a request signed with the secret written answered %d: %s", code, answer)
	}
	var started struct {
		Run string `json:"run"`
	}
	if err := json.Unmarshal(answer, &started); err != nil {
		t.Fatal(err)
	}
	if ended := in.Wait(started.Run, 3*time.Minute); ended.State != "succeeded" {
		t.Fatalf("the webhook's run %s ended %s: %s %v", started.Run, ended.State, ended.Answer, ended.Reasons)
	}

	for _, r := range in.runsOf("gate") {
		if (r.TriggerKind != "schedule" && r.TriggerKind != "webhook") || r.TriggeredBy != Namespace+"/agentiik" || r.Commit != commit {
			t.Errorf("a run of the workflow names %+v, and every run a trigger started names its kind, %s/agentiik and %s", r, Namespace, commit)
		}
	}
}

// listedRun is a run as GET /api/v1/{ns}/runs lists it, as far as the gate reads it.
type listedRun struct {
	Run         string `json:"run"`
	Workflow    string `json:"workflow"`
	Commit      string `json:"commit"`
	TriggerKind string `json:"trigger_kind"`
	TriggeredBy string `json:"triggered_by"`
}

// runsOf lists the runs of workflow in the namespace, as the operator reads them.
func (in *Installation) runsOf(workflow string) []listedRun {
	in.t.Helper()
	var listed struct {
		Runs []listedRun `json:"runs"`
	}
	in.Operator("GET", "/api/v1/"+Namespace+"/runs", nil, http.StatusOK, &listed)
	var out []listedRun
	for _, r := range listed.Runs {
		if r.Workflow == workflow {
			out = append(out, r)
		}
	}
	return out
}

// hook sends body to the webhook at path, signed with key as Standard Webhooks signs a delivery,
// through the terminator and with no credential of the operator's, and answers what it answered.
func (in *Installation) hook(path, body string, key []byte) (int, []byte) {
	in.t.Helper()
	id, stamp := "msg_"+randomHex(8), strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + stamp + "." + body))
	r, err := http.NewRequestWithContext(in.ctx, "POST", in.PublicURL+"/hooks/"+Namespace+path, strings.NewReader(body))
	if err != nil {
		in.t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("webhook-id", id)
	r.Header.Set("webhook-timestamp", stamp)
	r.Header.Set("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	answer, err := in.client.Do(r)
	if err != nil {
		in.t.Fatal(err)
	}
	defer answer.Body.Close()
	read, err := io.ReadAll(io.LimitReader(answer.Body, 1<<20))
	if err != nil {
		in.t.Fatal(err)
	}
	return answer.StatusCode, read
}
