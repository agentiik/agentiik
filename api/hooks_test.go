package api_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// Webhooks worth trusting: "a webhook whose signature does not verify starts nothing".

// clearHooks keeps a webhook's secret in the clear beside what it is bound to, and opens it only
// where it was sealed: what a test of the routes needs. secret.Hooks' own tests hold its sealing.
type clearHooks struct{}

func (clearHooks) SealHook(namespace, method, path string, version int, secret []byte) (json.RawMessage, error) {
	return json.Marshal(map[string]any{"bound": fmt.Sprint(namespace, " ", method, " ", path, " ", version), "secret": secret})
}

func (clearHooks) OpenHook(namespace, method, path string, version int, sealed json.RawMessage) ([]byte, error) {
	var s struct {
		Bound  string `json:"bound"`
		Secret []byte `json:"secret"`
	}
	if err := json.Unmarshal(sealed, &s); err != nil {
		return nil, err
	}
	if s.Bound != fmt.Sprint(namespace, " ", method, " ", path, " ", version) {
		return nil, errors.New("sealed for another webhook")
	}
	return s.Secret, nil
}

// hooking is everyone, with two service accounts of finance: deployer, which may start the
// workflow's runs, and auditor, which may read it.
func hooking() api.Authorizer {
	finance := api.Target{Namespace: "finance"}
	g := everyone().(granted)
	g["finance/deployer"] = []grant{{api.WorkflowRun, finance}}
	g["finance/auditor"] = []grant{{api.WorkflowRead, finance}}
	return g
}

// servingHooks is a repository whose default branch's head declares on, pushed.
func servingHooks(t *testing.T, on string) *gitServer {
	t.Helper()
	g := servingGit(t, hooking(), func(o *api.ServerOptions) { o.Hooks = clearHooks{} })
	work := g.newClone("alice")
	work.write("agentiik.yaml", triggered(on))
	work.commit("hooked")
	work.must("push", "-q", "origin", "main")
	return g
}

// delivery is one request to a webhook, as its sender makes it.
type delivery struct {
	method, path, body string
	header             http.Header
}

// signed is body delivered to /hooks/finance/invoicing, signed with key as Standard Webhooks signs
// it, by id at at.
func signed(key []byte, id string, at time.Time, body string) delivery {
	stamp := strconv.FormatInt(at.Unix(), 10)
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("webhook-id", id)
	h.Set("webhook-timestamp", stamp)
	h.Set("webhook-signature", signature(key, id, stamp, body))
	return delivery{method: "POST", path: "/hooks/finance/invoicing", body: body, header: h}
}

func signature(key []byte, id, stamp, body string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + stamp + "." + body))
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func (g *gitServer) deliver(d delivery) *httptest.ResponseRecorder {
	g.t.Helper()
	r := httptest.NewRequestWithContext(g.t.Context(), d.method, d.path, strings.NewReader(d.body))
	for name, values := range d.header {
		r.Header[name] = values
	}
	w := httptest.NewRecorder()
	g.h.ServeHTTP(w, r)
	return w
}

// writeSecret writes key as the secret of the webhook at POST /invoicing.
func (g *gitServer) writeSecret(key []byte) {
	g.t.Helper()
	w, _ := call(g.t, g.h, "PUT", "/api/v1/finance/workflows/monthly-invoicing/webhooks/POST/invoicing", "alice",
		map[string]string{"secret": "whsec_" + base64.StdEncoding.EncodeToString(key)})
	if w.Code != http.StatusNoContent {
		g.t.Fatalf("writing the secret answered %d: %s", w.Code, w.Body)
	}
}

// started are the runs of finance, oldest first, each as id, trigger kind, principal, commit and
// inputs.
func (g *gitServer) started() []string {
	g.t.Helper()
	rows, err := dbtest.Superuser(g.t, g.super).Query(g.t.Context(),
		`select id || ' ' || trigger || ' ' || coalesce(triggered_by, '-') || ' ' || commit || ' ' || inputs::text from runs order by created_at, id`)
	if err != nil {
		g.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			g.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// failures is what the webhook counts against itself.
func (g *gitServer) failures() int64 {
	g.t.Helper()
	armed := g.armed("bob")
	if len(armed.Triggers) != 1 || armed.Triggers[0].Failures == nil {
		g.t.Fatalf("the workflow has armed %+v", armed)
	}
	return *armed.Triggers[0].Failures
}

// Tested on captured requests: a good signature, a tampered body, a stale timestamp, a replayed
// delivery, a rotated secret, and a body re-encoded with other whitespace.
func TestAWebhookStartsARunOnlyForWhatItsSecretSigned(t *testing.T) {
	g := servingHooks(t, `  webhook:
    - path: /invoicing
      map:
        orders: ${{ trigger.body.orders }}
`)
	key := bytes.Repeat([]byte{7}, 32)
	now := time.Now()
	body := `{"orders":[{"customer_id":"C-1042"}]}`

	// Signed before any secret is written: nothing to verify it against.
	if w := g.deliver(signed(key, "msg_0", now, body)); w.Code != http.StatusUnauthorized {
		t.Fatalf("a request to a webhook with no secret written answered %d: %s", w.Code, w.Body)
	}
	g.writeSecret(key)

	good := signed(key, "msg_1", now, body)
	w := g.deliver(good)
	if w.Code != http.StatusAccepted {
		t.Fatalf("a request signed with the secret answered %d: %s", w.Code, w.Body)
	}
	var answer struct{ Run, State, Commit string }
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	runs := g.started()
	if len(runs) != 1 || runs[0] != answer.Run+" webhook finance/agentiik "+answer.Commit+` {"orders": [{"customer_id": "C-1042"}]}` {
		t.Fatalf("the request started %q, answering %+v", runs, answer)
	}
	if w.Header().Get("Location") != "/api/v1/finance/runs/"+answer.Run || answer.State != "queued" {
		t.Errorf("the answer is %+v, at %q", answer, w.Header().Get("Location"))
	}
	var frozen string
	if err := dbtest.Superuser(t, g.super).QueryRow(t.Context(), `select trigger_context -> 'trigger' ->> 'body' from runs`).Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	if frozen != `{"orders": [{"customer_id": "C-1042"}]}` {
		t.Errorf("the run froze the body %s", frozen)
	}

	// The same delivery again, as a sender retrying sends it: the run it started, and no second.
	again := g.deliver(good)
	if again.Code != http.StatusAccepted || !strings.Contains(again.Body.String(), answer.Run) || len(g.started()) != 1 {
		t.Errorf("the delivery sent again answered %d %s, and %d runs exist", again.Code, again.Body, len(g.started()))
	}

	refused := []struct {
		why string
		d   delivery
	}{
		{"the body tampered with", func() delivery {
			d := signed(key, "msg_2", now, body)
			d.body = `{"orders":[{"customer_id":"C-9999"}]}`
			return d
		}()},
		{"the body re-encoded with other whitespace", func() delivery {
			d := signed(key, "msg_3", now, body)
			d.body = `{"orders": [{"customer_id": "C-1042"}]}`
			return d
		}()},
		{"a timestamp six minutes old", signed(key, "msg_4", now.Add(-6*time.Minute), body)},
		{"a timestamp six minutes ahead", signed(key, "msg_5", now.Add(6*time.Minute), body)},
		{"another delivery's signature", func() delivery {
			d := signed(key, "msg_6", now, body)
			d.header.Set("webhook-signature", good.header.Get("webhook-signature"))
			return d
		}()},
		{"a signature of another scheme", func() delivery {
			d := signed(key, "msg_7", now, body)
			d.header.Set("webhook-signature", strings.Replace(d.header.Get("webhook-signature"), "v1,", "v1a,", 1))
			return d
		}()},
		{"no signature", func() delivery {
			d := signed(key, "msg_8", now, body)
			d.header.Del("webhook-signature")
			return d
		}()},
	}
	for i, c := range refused {
		w := g.deliver(c.d)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("a request with %s answered %d: %s", c.why, w.Code, w.Body)
		}
		if got := g.failures(); got != int64(i+2) {
			t.Errorf("after a request with %s the webhook counts %d failures, want %d", c.why, got, i+2)
		}
	}
	if n := len(g.started()); n != 1 {
		t.Errorf("the refused requests started %d runs", n-1)
	}

	// The secret rotated by writing another: a request signed with the old one is refused, and one
	// signed with both, as a sender rotating signs, is taken.
	rotated := bytes.Repeat([]byte{9}, 32)
	g.writeSecret(rotated)
	if w := g.deliver(signed(key, "msg_9", now, body)); w.Code != http.StatusUnauthorized {
		t.Errorf("a request signed with the secret rotated away answered %d: %s", w.Code, w.Body)
	}
	both := signed(key, "msg_10", now, body)
	both.header.Set("webhook-signature", both.header.Get("webhook-signature")+" "+signature(rotated, "msg_10", both.header.Get("webhook-timestamp"), body))
	if w := g.deliver(both); w.Code != http.StatusAccepted {
		t.Errorf("a request signed with the old and the new secret answered %d: %s", w.Code, w.Body)
	}
	if n := len(g.started()); n != 2 {
		t.Errorf("%d runs exist, want 2", n)
	}

	// Inputs map fills are held to their schemas before a run exists, and a delivery refused so
	// is not taken.
	bad := signed(rotated, "msg_11", now, `{"orders":"none"}`)
	if w := g.deliver(bad); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `"input":"orders"`) {
		t.Errorf("a body map fills an input the schema refuses from answered %d: %s", w.Code, w.Body)
	}
	if n := len(g.started()); n != 2 {
		t.Errorf("a refused input started a run: %d exist", n)
	}

	// The listing says a secret is written, and never what.
	listed, _ := call(t, g.h, "GET", "/api/v1/finance/workflows/monthly-invoicing/triggers", "bob", nil)
	if !strings.Contains(listed.Body.String(), `"credential":{"secret":true,"written_by":"alice"`) || strings.Contains(listed.Body.String(), base64.StdEncoding.EncodeToString(rotated)) {
		t.Errorf("the listing reads %s", listed.Body)
	}
}

// "Answer an unknown or invisible hook path with the 404 an absent one gets, before any signature
// work, so probing yields nothing": another path, another method, another namespace, a namespace
// that cannot exist, a path that is not one; and none of them is counted against the webhook.
func TestAPathNoWebhookAnswersIsAnsweredAsAbsent(t *testing.T) {
	g := servingHooks(t, `  webhook:
    - path: /invoicing
`)
	g.writeSecret(bytes.Repeat([]byte{7}, 32))
	var bodies []string
	for _, d := range []delivery{
		{method: "POST", path: "/hooks/finance/receipts"},
		{method: "PUT", path: "/hooks/finance/invoicing"},
		{method: "GET", path: "/hooks/finance/invoicing"},
		{method: "POST", path: "/hooks/team-ops/invoicing"},
		{method: "POST", path: "/hooks/nowhere/invoicing"},
		{method: "POST", path: "/hooks/Finance/invoicing"},
		{method: "POST", path: "/hooks/finance/invoicing/"},
		{method: "POST", path: "/hooks/finance/.hidden"},
		{method: "post", path: "/hooks/finance/invoicing"},
	} {
		w := g.deliver(d)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s answered %d: %s", d.method, d.path, w.Code, w.Body)
		}
		bodies = append(bodies, w.Body.String())
	}
	for _, b := range bodies {
		if b != bodies[0] {
			t.Errorf("an absent webhook is answered %q and another %q", bodies[0], b)
		}
	}
	if got := g.failures(); got != 0 {
		t.Errorf("the requests to no webhook counted %d failures against one", got)
	}
}

// bearer: "a token bound to a service account", which "authenticates the caller, not a user", and
// holds workflow:run on the workflow. The run is still the namespace's built-in identity's.
func TestAWebhookByBearerTakesTheTokenOfAServiceAccountThatMayRunIt(t *testing.T) {
	g := servingHooks(t, `  webhook:
    - path: /invoicing
      auth: bearer
      map:
        cycle: ${{ trigger.query.cycle + "-" + trigger.headers["x-sender"] }}
`)
	ask := func(token string) *httptest.ResponseRecorder {
		h := http.Header{}
		h.Set("X-Sender", "ci")
		if token != "" {
			h.Set("Authorization", "Bearer "+token)
		}
		return g.deliver(delivery{method: "POST", path: "/hooks/finance/invoicing?cycle=2026-09", header: h})
	}
	for i, token := range []string{"", "alice", "owner", "finance/auditor"} {
		if w := ask(token); w.Code != http.StatusUnauthorized {
			t.Errorf("a request bearing %q answered %d: %s", token, w.Code, w.Body)
		}
		if got := g.failures(); got != int64(i+1) {
			t.Errorf("after a request bearing %q the webhook counts %d failures", token, got)
		}
	}
	w := ask("finance/deployer")
	if w.Code != http.StatusAccepted {
		t.Fatalf("a request bearing deployer's token answered %d: %s", w.Code, w.Body)
	}
	runs := g.started()
	if len(runs) != 1 || !strings.Contains(runs[0], " webhook finance/agentiik ") || !strings.HasSuffix(runs[0], `{"cycle": "2026-09-ci"}`) {
		t.Errorf("the request started %q", runs)
	}
	var headers, caller string
	if err := dbtest.Superuser(t, g.super).QueryRow(t.Context(), `select (trigger_context -> 'trigger' -> 'headers')::text from runs`).Scan(&headers); err != nil {
		t.Fatal(err)
	}
	// The run is the built-in identity's, and the entry says which service account called.
	if err := dbtest.Superuser(t, g.super).QueryRow(t.Context(),
		`select actor || ' ' || (detail::jsonb ->> 'caller') from audit_log where action = 'run.trigger'`).Scan(&caller); err != nil {
		t.Fatal(err)
	}
	if caller != "finance/agentiik finance/deployer" {
		t.Errorf("the run.trigger entry reads %q", caller)
	}
	if strings.Contains(headers, "authorization") || !strings.Contains(headers, `"x-sender": "ci"`) {
		t.Errorf("the run froze the headers %s", headers)
	}
}

// mtls: the one certificate written for the webhook, presented in the handshake of the TLS the API
// serves itself.
func TestAWebhookByMTLSTakesTheCertificateWrittenForIt(t *testing.T) {
	g := servingHooks(t, `  webhook:
    - path: /invoicing
      auth: mtls
`)
	server := httptest.NewUnstartedServer(g.h)
	server.TLS = &tls.Config{ClientAuth: tls.RequestClientCert}
	server.StartTLS()
	t.Cleanup(server.Close)
	accepted, acceptedPEM := aClientCertificate(t)
	other, _ := aClientCertificate(t)

	w, _ := call(t, g.h, "PUT", "/api/v1/finance/workflows/monthly-invoicing/webhooks/POST/invoicing", "alice", map[string]string{"certificate": acceptedPEM})
	if w.Code != http.StatusNoContent {
		t.Fatalf("writing the certificate answered %d: %s", w.Code, w.Body)
	}
	ask := func(presented *tls.Certificate) int {
		client := server.Client()
		transport := client.Transport.(*http.Transport).Clone()
		if presented != nil {
			transport.TLSClientConfig.Certificates = []tls.Certificate{*presented}
		}
		client.Transport = transport
		resp, err := client.Post(server.URL+"/hooks/finance/invoicing", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := ask(nil); code != http.StatusUnauthorized {
		t.Errorf("a request presenting no certificate answered %d", code)
	}
	if code := ask(&other); code != http.StatusUnauthorized {
		t.Errorf("a request presenting another certificate answered %d", code)
	}
	if code := ask(&accepted); code != http.StatusAccepted {
		t.Errorf("a request presenting the certificate written answered %d", code)
	}
	if got := g.failures(); got != 2 {
		t.Errorf("the webhook counts %d failures, want 2", got)
	}
	// Over plain HTTP, as behind a terminator, no certificate reaches the API, whatever a header says.
	h := http.Header{}
	h.Set("Client-Cert", ":"+base64.StdEncoding.EncodeToString(accepted.Certificate[0])+":")
	if w := g.deliver(delivery{method: "POST", path: "/hooks/finance/invoicing", header: h}); w.Code != http.StatusUnauthorized {
		t.Errorf("a certificate in a header answered %d: %s", w.Code, w.Body)
	}
}

// aClientCertificate is a self-signed client certificate, and it in PEM.
func aClientCertificate(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "siem"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// response: sync holds the request until the run ends and answers the output it names; a run ending
// otherwise is a 502 naming it.
func TestASyncWebhookAnswersWithTheOutputItNames(t *testing.T) {
	g := servingHooks(t, `  webhook:
    - path: /invoicing
      auth: none
      response: sync
      output: invoices
`)
	super := dbtest.Superuser(t, g.super)
	// ends ends the next run the webhook starts, as the controller would, succeeded with its output
	// or in state.
	ends := func(state string) <-chan struct{} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			for range 200 {
				var run string
				if err := super.QueryRow(t.Context(), `select id from runs where state = 'queued' limit 1`).Scan(&run); err != nil {
					time.Sleep(20 * time.Millisecond)
					continue
				}
				if state == "succeeded" {
					envelope := anEnvelope(run)
					digest, size, err := artifact.PutEnvelope(t.Context(), artifact.Dir(g.objects), "finance", envelope)
					if err != nil {
						t.Error(err)
						return
					}
					ports, _ := json.Marshal(map[string]any{"ok": map[string]any{"digest": "sha256:" + digest, "size": size, "items": 1}})
					outputs, _ := json.Marshal(map[string]any{"invoices": map[string]any{"step": "archive", "port": "ok", "count": 1}})
					super.Exec(t.Context(), `update steps set ports = $2, state = 'succeeded' where run_id = $1 and step = 'archive'`, run, ports)
					super.Exec(t.Context(), `update runs set state = 'succeeded', started_at = now(), finished_at = now(), outputs = $2 where id = $1`, run, outputs)
				} else {
					super.Exec(t.Context(), `update runs set state = $2, started_at = now(), finished_at = now() where id = $1`, run, state)
				}
				return
			}
			t.Error("no run was started")
		}()
		return done
	}

	done := ends("succeeded")
	w := g.deliver(delivery{method: "POST", path: "/hooks/finance/invoicing"})
	<-done
	var envelope agk.Envelope
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &envelope) != nil || envelope.Meta.Port != "ok" || len(envelope.Items) != 1 {
		t.Errorf("a sync webhook whose run succeeded answered %d: %s", w.Code, w.Body)
	}

	done = ends("failed")
	w = g.deliver(delivery{method: "POST", path: "/hooks/finance/invoicing"})
	<-done
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), `"state":"failed"`) {
		t.Errorf("a sync webhook whose run failed answered %d: %s", w.Code, w.Body)
	}
}

// What a webhook checks its caller against is written as Standard Webhooks writes a secret, or as a
// certificate in PEM, one of them, for a path and a method a webhook could answer.
func TestAWebhookCredentialIsWrittenOneOfTwoWays(t *testing.T) {
	g := servingHooks(t, `  webhook:
    - path: /invoicing
`)
	secret := "whsec_" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	_, certificate := aClientCertificate(t)
	for _, c := range []struct {
		path string
		body any
		want int
	}{
		{"POST/invoicing", map[string]string{"secret": secret}, http.StatusNoContent},
		{"POST/not-yet/armed", map[string]string{"secret": secret}, http.StatusNoContent},
		{"PUT/invoicing", map[string]string{"certificate": certificate}, http.StatusNoContent},
		{"POST/invoicing", map[string]string{"secret": strings.TrimPrefix(secret, "whsec_")}, http.StatusBadRequest},
		{"POST/invoicing", map[string]string{"secret": "whsec_" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 16))}, http.StatusBadRequest},
		{"POST/invoicing", map[string]string{"secret": "whsec_not base64"}, http.StatusBadRequest},
		{"POST/invoicing", map[string]string{"secret": secret, "certificate": certificate}, http.StatusBadRequest},
		{"POST/invoicing", map[string]string{}, http.StatusBadRequest},
		{"POST/invoicing", map[string]string{"certificate": "not a certificate"}, http.StatusBadRequest},
		{"POST/invoicing", map[string]string{"value": secret}, http.StatusBadRequest},
		{"post/invoicing", map[string]string{"secret": secret}, http.StatusBadRequest},
		{"POST/.hidden", map[string]string{"secret": secret}, http.StatusBadRequest},
	} {
		if w, _ := call(t, g.h, "PUT", "/api/v1/finance/workflows/monthly-invoicing/webhooks/"+c.path, "alice", c.body); w.Code != c.want {
			t.Errorf("writing %v at %s answered %d, want %d: %s", c.body, c.path, w.Code, c.want, w.Body)
		}
	}
	if w, _ := call(t, g.h, "PUT", "/api/v1/finance/workflows/payroll/webhooks/POST/invoicing", "alice", map[string]string{"secret": secret}); w.Code != http.StatusNotFound {
		t.Errorf("writing a secret for a workflow that does not exist answered %d: %s", w.Code, w.Body)
	}
	if w, _ := call(t, g.h, "PUT", "/api/v1/finance/workflows/monthly-invoicing/webhooks/POST/invoicing", "bob", map[string]string{"secret": secret}); w.Code != http.StatusNotFound {
		t.Errorf("bob, who reads the workflow and does not change it, wrote a secret: %d %s", w.Code, w.Body)
	}
	var written []string
	rows, err := dbtest.Superuser(t, g.super).Query(t.Context(), `select detail::text from audit_log where action = 'webhook_credential.write' order by seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		rows.Scan(&d)
		written = append(written, d)
	}
	if strings.Join(written, "\n") != `{"method":"POST","path":"/invoicing","written":"secret"}
{"method":"POST","path":"/not-yet/armed","written":"secret"}
{"method":"PUT","path":"/invoicing","written":"certificate"}` {
		t.Errorf("the log records %q", written)
	}
}
