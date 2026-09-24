package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/internal/fixtures"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// readFixture decodes one document of the vendored corpus.
func readFixture(t *testing.T, file string, into any) {
	t.Helper()
	b, err := fs.ReadFile(fixtures.FS, file)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatalf("%s: %s", file, err)
	}
}

// redemptionFixture is the response half of one grant redemption of the corpus.
func redemptionFixture(t *testing.T, file string) Redemption {
	t.Helper()
	var pair struct {
		Response Redemption `json:"response"`
	}
	readFixture(t, file, &pair)
	return pair.Response
}

// answering narrows the corpus's redemption to what one message names, which is what the API
// answers that message with: the same task, its own input ports and its own secrets.
func answering(r Redemption, m bus.TaskMessage) Redemption {
	r.TaskID = m.TaskID
	ports := map[string]bool{}
	for _, in := range m.Inputs {
		ports[in.Port] = true
	}
	names := map[string]bool{}
	for _, s := range m.Secrets {
		names[s.Name] = true
	}
	inputs, secrets := []RedeemedInput{}, []RedeemedSecret{}
	for _, in := range r.Inputs {
		if ports[in.Port] {
			inputs = append(inputs, in)
		}
	}
	for _, s := range r.Secrets {
		if names[s.Name] {
			secrets = append(secrets, s)
		}
	}
	r.Inputs, r.Secrets = inputs, secrets
	return r
}

// redemptionRequests compiles the wire's redemption request, which is what a runner sends.
func redemptionRequests(t *testing.T) *jsonschema.Schema {
	t.Helper()
	doc, err := fixtures.Wire()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource("wire.schema.json", raw); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("wire.schema.json#/$defs/grantRedemption/properties/request")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// corpusExchange is the message and the redemption of the corpus that answers it.
func corpusExchange(t *testing.T) (bus.TaskMessage, []byte) {
	t.Helper()
	var m bus.TaskMessage
	readFixture(t, "fixtures/wire/valid/task-message.json", &m)
	r := answering(redemptionFixture(t, "fixtures/wire/valid/grant-redemption.json"), m)
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return m, b
}

// A redemption is posted where the page says, as the wire's request, and the corpus's answer is
// read back whole.
func TestARedemptionIsTheWiresRequestAndReadsTheWiresAnswer(t *testing.T) {
	m, answer := corpusExchange(t)
	schema := redemptionRequests(t)
	var sent []byte
	var path, auth string
	c := api(t, func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.Method+" "+r.URL.Path, r.Header.Get("Authorization")
		sent, _ = io.ReadAll(r.Body)
		w.Write(answer)
	})
	r, err := c.Redeem(t.Context(), m)
	if err != nil {
		t.Fatalf("redeeming: %s", err)
	}
	if NextAfter(err) != RedeemRun {
		t.Errorf("a redeemed task is %v", NextAfter(err))
	}
	if path != "POST /api/v1/tasks/redeem" || auth != "Bearer "+credential {
		t.Errorf("the redemption went as %s with %q", path, auth)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(sent))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(v); err != nil {
		t.Errorf("the request is not the wire's: %s\n%s", err, sent)
	}
	if !strings.Contains(string(sent), m.Grant) || !strings.Contains(string(sent), m.IdempotencyKey) {
		t.Errorf("the request names another task: %s", sent)
	}
	if r.TaskID != m.TaskID || len(r.Inputs) != 1 || len(r.Secrets) != 1 || len(r.Tree) != 1 || r.Uploads.KeyPrefix != "finance/sha256/" {
		t.Errorf("the answer reads as %+v", r)
	}
}

// Each answer leads where the page's table says, and an answer that says nothing of whose the task
// is never lets go of it.
func TestEachAnswerToARedemptionLeadsWhereThePageSays(t *testing.T) {
	m, answer := corpusExchange(t)
	unknown := bytes.Replace(answer, []byte(`"task_id"`), []byte(`"grant_ttl":900,"task_id"`), 1)
	// An expiry written as a number: JSON, and not this runner's answer.
	var doc map[string]any
	if err := json.Unmarshal(answer, &doc); err != nil {
		t.Fatal(err)
	}
	doc["expires_at"] = 1789023069
	reshaped, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		status int
		body   []byte
		want   Next
	}{
		"200":                        {http.StatusOK, answer, RedeemRun},
		"401, a grant that expired":  {http.StatusUnauthorized, []byte(`{"error":"that grant cannot be redeemed"}`), RedeemAgain},
		"403, draining":              {http.StatusForbidden, []byte(`{"error":"this runner is draining"}`), RedeemPutBack},
		"409, another runner's":      {http.StatusConflict, []byte(`{"error":"that task is not this runner's to work on"}`), RedeemLetGo},
		"422, never answerable":      {http.StatusUnprocessableEntity, []byte(`{"error":"the secret billing is not held for this namespace"}`), RedeemReport},
		"500":                        {http.StatusInternalServerError, []byte(`{"error":"the grant could not be redeemed"}`), RedeemAgain},
		"503":                        {http.StatusServiceUnavailable, nil, RedeemAgain},
		"400, a status nobody named": {http.StatusBadRequest, []byte(`{"error":"a redemption names the task it is for"}`), RedeemAgain},
		"200 with a field unknown":   {http.StatusOK, unknown, RedeemReport},
		"200 cut short":              {http.StatusOK, answer[:len(answer)/2], RedeemAgain},
		"200 from a proxy":           {http.StatusOK, []byte("<html>maintenance</html>"), RedeemAgain},
		"200 with nothing":           {http.StatusOK, nil, RedeemAgain},
		"200 of another shape":       {http.StatusOK, reshaped, RedeemReport},
		"200 for another task":       {http.StatusOK, bytes.Replace(answer, []byte(m.TaskID), []byte("01JMZ8V1PC7K3M0QY4B8ZR6TDM"), 1), RedeemAgain},
		"200 of a proxy's JSON":      {http.StatusOK, []byte(`{"message":"Service Unavailable"}`), RedeemAgain},
		"200 of a list":              {http.StatusOK, []byte(`[]`), RedeemAgain},
		"200 of the task, unusable":  {http.StatusOK, bytes.Replace(answer, []byte(`"finance/sha256/"`), []byte(`"payroll/sha256/"`), 1), RedeemReport},
	} {
		t.Run(name, func(t *testing.T) {
			client := api(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				w.Write(c.body)
			})
			_, err := client.Redeem(t.Context(), m)
			if got := NextAfter(err); got != c.want {
				t.Errorf("the answer leads to %v, want %v: %v", got, c.want, err)
			}
			if c.want == RedeemReport && c.status == http.StatusOK && !errors.Is(err, ErrAnswerUnusable) {
				t.Errorf("a 200 that cannot be used answered %v", err)
			}
		})
	}

	t.Run("no answer at all", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		srv.Close()
		client, err := NewClient(srv.URL, credential, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Redeem(t.Context(), m)
		if got := NextAfter(err); got != RedeemAgain {
			t.Errorf("no answer leads to %v: %v", got, err)
		}
	})
}

// A field the wire does not describe is refused rather than dropped, naming it, and the refusal
// repeats no URL and no value of the answer.
func TestAnUnknownFieldInARedemptionAnswerIsRefused(t *testing.T) {
	m, answer := corpusExchange(t)
	for _, extra := range []struct{ where, field string }{
		{`"task_id"`, `"grant_ttl":900,`},
		{`"path"`, `"owner":"root",`},
		{`"name"`, `"provider":"builtin",`},
		{`"key_prefix"`, `"method":"PUT",`},
	} {
		body := bytes.Replace(answer, []byte(extra.where), []byte(extra.field+extra.where), 1)
		client := api(t, func(w http.ResponseWriter, r *http.Request) { w.Write(body) })
		_, err := client.Redeem(t.Context(), m)
		if !errors.Is(err, ErrAnswerUnusable) {
			t.Errorf("an answer carrying %s was taken: %v", extra.field, err)
			continue
		}
		name := strings.Split(extra.field, `"`)[1]
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal does not name %s: %s", name, err)
		}
		for _, leaked := range []string{"X-Amz-Signature", "bk_live_7Qm2rXt9vZa4", "9d4b71e0c6a52f38"} {
			if strings.Contains(err.Error(), leaked) {
				t.Errorf("the refusal repeats %s: %s", leaked, err)
			}
		}
	}
}

// A redemption that answers another task than the message's is refused, each way a pair of
// documents could describe two tasks, and so are the invalid redemptions of the corpus.
func TestARedemptionThatDoesNotAnswerTheMessageIsRefused(t *testing.T) {
	var m bus.TaskMessage
	readFixture(t, "fixtures/wire/valid/task-message.json", &m)
	fixture := answering(redemptionFixture(t, "fixtures/wire/valid/grant-redemption.json"), m)
	if err := fixture.answers(m); err != nil {
		t.Fatalf("the corpus's own redemption is refused: %s", err)
	}
	clone := func() Redemption {
		var r Redemption
		b, _ := json.Marshal(fixture)
		json.Unmarshal(b, &r)
		return r
	}
	for name, change := range map[string]func(*Redemption){
		"another task_id":         func(r *Redemption) { r.TaskID = "01JMZ8V1PC7K3M0QY4B8ZR6TDM" },
		"no expiry":               func(r *Redemption) { r.ExpiresAt = "soon" },
		"a port not named":        func(r *Redemption) { r.Inputs[0].Port = "orders" },
		"a port twice":            func(r *Redemption) { r.Inputs = append(r.Inputs, r.Inputs[0]) },
		"a port missing":          func(r *Redemption) { r.Inputs = nil },
		"another envelope":        func(r *Redemption) { r.Inputs[0].Envelope.Digest = "sha256:" + strings.Repeat("0", 64) },
		"an envelope with no URL": func(r *Redemption) { r.Inputs[0].Envelope.URL = "" },
		"an artifact by name": func(r *Redemption) {
			r.Inputs[0].Artifacts = []RedeemedArtifact{{URI: "invoice.pdf", SHA256: strings.Repeat("a", 64), URL: "https://x"}}
		},
		"a secret not named":   func(r *Redemption) { r.Secrets[0].Name = "payroll" },
		"a secret twice":       func(r *Redemption) { r.Secrets = append(r.Secrets, r.Secrets[0]) },
		"a secret missing":     func(r *Redemption) { r.Secrets = nil },
		"a secret elsewhere":   func(r *Redemption) { r.Secrets[0].Mount = "/agk/secrets/other" },
		"a secret in hex":      func(r *Redemption) { r.Secrets[0].Encoding = "hex" },
		"a tree path climbing": func(r *Redemption) { r.Tree[0].Path = "../agentiik.yaml" },
		"a tree path absolute": func(r *Redemption) { r.Tree[0].Path = "/agentiik.yaml" },
		"a tree path twice":    func(r *Redemption) { r.Tree = append(r.Tree, r.Tree[0]) },
		"a tree file under a file": func(r *Redemption) {
			below := r.Tree[0]
			below.Path += "/below"
			r.Tree = append(r.Tree, below)
		},
		"a tree file relocated": func(r *Redemption) { r.Tree[0].To = "/etc/ssl/certs/internal-ca.pem" },
		"a tree file by name":   func(r *Redemption) { r.Tree[0].SHA256 = "agentiik.yaml" },
		"no upload policy":      func(r *Redemption) { r.Uploads.URL = "" },
		"another namespace":     func(r *Redemption) { r.Uploads.KeyPrefix = "payroll/sha256/" },
	} {
		t.Run(name, func(t *testing.T) {
			r := clone()
			change(&r)
			if err := r.answers(m); err == nil {
				t.Error("the redemption was taken as the message's")
			}
		})
	}

	cases, err := fixtures.GrantRedemptions()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if c.Valid {
			continue
		}
		b, err := fs.ReadFile(fixtures.FS, c.File)
		if err != nil {
			t.Fatal(err)
		}
		var pair struct {
			Response Redemption `json:"response"`
		}
		if err := json.Unmarshal(b, &pair); err != nil {
			t.Fatal(err)
		}
		if err := answering(pair.Response, m).answers(m); err == nil {
			t.Errorf("%s is taken, and the corpus refuses it by %s", c.File, c.Rule)
		}
	}
}
