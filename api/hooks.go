package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/trigger"
	"github.com/jackc/pgx/v5/pgconn"
)

// Webhooks worth trusting.
//
// A webhook is served at /hooks/<namespace><path>, on the method its default branch declares, and
// authenticates the caller rather than a user: "HMAC over the raw body with a per-trigger secret, a
// bearer token bound to a service account, or a client certificate", or nothing where the file says
// auth: none. A path no webhook of the namespace answers on that method is answered as an absent one
// is, before any signature is looked at, so that probing yields nothing; then the caller is proved,
// and a request that does not prove it is refused and counted against the webhook; then the body is
// read into the trigger context, map fills the workflow inputs from it, and the run is started by
// the one path every run takes, under the namespace's built-in identity.
//
// hmac follows Standard Webhooks (standardwebhooks.com), which Svix, Clerk, Resend and others send
// by, rather than a signature scheme of Agentiik's own: webhook-id names the delivery,
// webhook-timestamp the second it was signed at, and webhook-signature carries v1, and the base64 of
// the HMAC-SHA256 of id.timestamp.body under the secret. The body is the one received, byte for byte,
// "because two JSON encoders disagree on whitespace", and the timestamp is signed with it, so that a
// captured request cannot be sent again with a later one. The header may carry several signatures,
// space separated, one of which has to verify: a sender rotating its secret signs with both while
// the new one is written here, and no request is refused in between.

// webhooksWhy is what authorises a webhook instead of a principal.
const webhooksWhy = "a webhook authenticates the caller per trigger, not a user: by an HMAC over the raw body under the webhook's own secret, by the API token of a service account holding workflow:run on its workflow, or by the one client certificate it accepts, and by nothing where its file says auth: none. The run it starts is the namespace's built-in identity's, which holds what an owner granted it"

// HookSecrets seals and opens the secrets webhooks sign with. secret.Hooks fills it; package api
// holds the interface and none of what fills it, for the reason Values is: the command line links
// this package.
type HookSecrets interface {
	SealHook(namespace, method, path string, version int, secret []byte) (json.RawMessage, error)
	OpenHook(namespace, method, path string, version int, sealed json.RawMessage) ([]byte, error)
}

// hookWindow is how far from the API's clock a signed timestamp may be, either side: "a signed
// timestamp, a five-minute window". Either side, since a sender's clock ahead of this one is a clock,
// not an attack, and five minutes is what Standard Webhooks writes.
const hookWindow = 5 * time.Minute

// hookSyncWait is how long a sync webhook holds its request for the run to end: past it the
// request is answered as an async one is, 202 with the run, which goes on. A minute, because the
// proxies and load balancers a request crosses close an idle one at sixty seconds by default, nginx's
// proxy_read_timeout and an AWS load balancer's idle timeout among them, and a request they close is
// one whose caller learns nothing, not even the run it started.
const hookSyncWait = time.Minute

// hookMaxBytes bounds the body a webhook takes: one envelope's weight, as a run's inputs are
// bounded, since the body is frozen on the run as trigger.body beside them and read with them at
// every decision the controller takes.
const hookMaxBytes = trigger.InputsMaxBytes

// hookSecretBytes bound a webhook's secret once decoded: 24 to 64 bytes, as Standard Webhooks bounds
// one. Fewer is a key somebody could search; more adds nothing to HMAC-SHA256, which hashes a key
// longer than its block first.
const (
	hookSecretMin = 24
	hookSecretMax = 64
)

// hookPath and hookMethod are the grammar of a webhook's path and method, the schema's.
var (
	hookPath   = regexp.MustCompile(`^(?:/[A-Za-z0-9_~-][A-Za-z0-9._~-]*)+$`)
	hookMethod = regexp.MustCompile(`^[A-Z]+$`)
	// deliveryID is what a webhook-id may be: visible ASCII, which a sender's identifiers are, at
	// most as long as the delivery's row keeps.
	deliveryID = regexp.MustCompile(`^[\x21-\x7e]{1,255}$`)
)

// notHooked is the answer a path no webhook answers gets, and so does one a namespace that does not
// exist would have: one sentence, the one every absent thing gets.
func notHooked(w http.ResponseWriter) {
	fail(w, http.StatusNotFound, "no such thing, or not yours")
}

// declaredHook is a webhook as its trigger row declares it, with the values in force written in.
type declaredHook struct {
	Auth     string `json:"auth"`
	Response string `json:"response"`
	Output   string `json:"output"`
}

// webhook answers a request to a webhook.
func (s *Server) webhook(w http.ResponseWriter, r *http.Request, _ Principal, over Target) {
	namespace, path, method := over.Namespace, "/"+r.PathValue("path"), r.Method
	if NamespaceName(namespace) != nil || len(path) > 255 || !hookPath.MatchString(path) || !hookMethod.MatchString(method) {
		notHooked(w)
		return
	}
	var hook db.Hook
	err := s.pool.In(r.Context(), namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		hook, err = ns.Hook(ctx, path, method)
		return err
	})
	if errors.Is(err, db.ErrNoHook) {
		notHooked(w)
		return
	}
	if err != nil {
		s.report(err)
		fail(w, http.StatusInternalServerError, "the webhook could not be read")
		return
	}
	var declared declaredHook
	if err := json.Unmarshal(hook.Declared, &declared); err != nil {
		s.report(fmt.Errorf("api: the webhook %s %s of %s is armed with %s: %w", method, path, namespace, hook.Declared, err))
		fail(w, http.StatusInternalServerError, "the webhook could not be read")
		return
	}

	raw, err := slurp(r, hookMaxBytes)
	if err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}

	now := s.now()
	proved, refusal, err := s.prove(r, namespace, hook, declared.Auth, raw, now)
	if err != nil {
		s.report(err)
		fail(w, http.StatusInternalServerError, "the caller could not be proved")
		return
	}
	if refusal != "" {
		// Counted in a transaction of its own, since the request starts nothing. A count that
		// could not be written is the installation's trouble and not the caller's answer.
		if err := s.pool.In(r.Context(), namespace, func(ctx context.Context, ns *db.NS) error {
			return ns.HookRefused(ctx, path, method, now)
		}); err != nil && !errors.Is(err, context.Canceled) {
			s.report(err)
		}
		fail(w, http.StatusUnauthorized, refusal)
		return
	}

	fired, err := requestContext(r, raw)
	if err != nil {
		var large *trigger.InputsTooLarge
		switch {
		case errors.As(err, &large):
			fail(w, http.StatusRequestEntityTooLarge, err.Error())
		case errors.Is(err, errNotText):
			fail(w, http.StatusUnsupportedMediaType, err.Error())
		default:
			fail(w, http.StatusBadRequest, err.Error())
		}
		return
	}

	target := Target{Namespace: namespace, Workflow: hook.Workflow}
	g, err := s.versions.Graph(r.Context(), namespace, hook.Workflow, hook.Commit)
	if s.refused(w, target, hook.Commit, err) {
		return
	}
	webhooks := g.Workflow().On.Webhook
	if hook.Position >= len(webhooks) {
		s.report(fmt.Errorf("api: the webhook %s %s of %s is armed at %d, and %s@%s declares %d", method, path, namespace, hook.Position, hook.Workflow, hook.Commit, len(webhooks)))
		fail(w, http.StatusInternalServerError, "the webhook could not be read")
		return
	}
	filled, err := g.Fill(webhooks[hook.Position].Map, graph.Fired{
		Commit: hook.Commit, Trigger: fired,
		TriggerKind: agk.TriggerWebhook.String(), TriggeredBy: namespace + "/" + db.BuiltIn,
	})
	if err == nil {
		filled, err = asSupplied(filled)
	}
	if err != nil {
		// "Validated against their schemas before the run exists": a map that cannot be evaluated
		// over this request is refused as an input is, and nothing is created.
		fail(w, http.StatusUnprocessableEntity, fmt.Sprintf("the webhook's map could not fill the inputs from this request: %v", err))
		return
	}
	delivery := proved.delivery
	detail := map[string]any{"path": path, "method": method}
	if delivery != "" {
		detail["delivery"] = delivery
	}
	if proved.caller != "" {
		detail["caller"] = proved.caller
	}
	prepared, err := s.starter.Prepare(r.Context(), trigger.Request{
		Namespace: namespace, Workflow: hook.Workflow, Kind: agk.TriggerWebhook, Commit: hook.Commit,
		Inputs: filled, Detail: detail, Context: db.TriggerContext{Trigger: fired},
	})
	if s.refused(w, target, hook.Commit, err) {
		return
	}

	var run agk.RunID
	state, again := agk.Queued, false
	err = s.pool.In(r.Context(), namespace, func(ctx context.Context, ns *db.NS) error {
		if delivery != "" {
			first, taken, err := ns.Delivery(ctx, path, method, delivery, now, 2*hookWindow)
			if err != nil {
				return err
			}
			if taken {
				run, again = first, true
				state, err = ns.RunState(ctx, first)
				return err
			}
		}
		var err error
		if run, err = prepared.Create(ctx, ns); err != nil {
			return err
		}
		if delivery != "" {
			return ns.DeliveryRun(ctx, path, method, delivery, run)
		}
		return nil
	})
	if s.refused(w, target, hook.Commit, err) {
		return
	}
	w.Header().Set("Location", fmt.Sprintf("/api/v1/%s/runs/%s", namespace, run))
	if declared.Response != "sync" || again {
		// A delivery taken already is answered with the run it started, as async answers, and not
		// waited on a second time.
		write(w, http.StatusAccepted, map[string]any{"run": string(run), "state": state.String(), "commit": hook.Commit})
		return
	}
	s.answerSync(w, r, namespace, run, hook.Commit, declared.Output)
}

// proof is what proving a webhook's caller established: the delivery a signed request names, and
// the service account whose token a bearer request carries.
type proof struct {
	delivery, caller string
}

// prove proves the caller of a webhook by the webhook's auth, and answers what it established, or
// why the caller is not proved, one sentence whatever the reason: a guesser learns nothing from it
// but that the answer was no. err is the installation's trouble.
func (s *Server) prove(r *http.Request, namespace string, hook db.Hook, auth string, raw []byte, now time.Time) (proved proof, refusal string, err error) {
	path, method := r.PathValue("path"), r.Method
	path = "/" + path
	credential := func() (db.HookCredential, error) {
		var c db.HookCredential
		err := s.pool.In(r.Context(), namespace, func(ctx context.Context, ns *db.NS) error {
			var err error
			c, err = ns.HookCredential(ctx, hook.Workflow, path, method)
			return err
		})
		if errors.Is(err, db.ErrNoHookCredential) {
			return db.HookCredential{}, nil
		}
		return c, err
	}
	switch auth {
	case "none":
		// "An endpoint is open only where its file says auth: none."
		return proof{}, "", nil

	case "hmac":
		const refused = "the request is not signed with the webhook's secret within five minutes of now: webhook-id, webhook-timestamp and webhook-signature, as Standard Webhooks writes them"
		id, stamp, signatures := r.Header.Get("webhook-id"), r.Header.Get("webhook-timestamp"), r.Header.Get("webhook-signature")
		seconds, err := strconv.ParseInt(stamp, 10, 64)
		if !deliveryID.MatchString(id) || err != nil || signatures == "" {
			return proof{}, refused, nil
		}
		if at := time.Unix(seconds, 0); at.Before(now.Add(-hookWindow)) || at.After(now.Add(hookWindow)) {
			return proof{}, refused, nil
		}
		c, err := credential()
		if err != nil {
			return proof{}, "", err
		}
		if c.Secret == nil {
			return proof{}, refused, nil
		}
		if s.hooks == nil {
			return proof{}, "", fmt.Errorf("api: the webhook %s %s of %s is signed with a secret, and this API holds no keyring to open it with", method, path, namespace)
		}
		// Opened under the namespace's storage name, which it was sealed under and a rename leaves
		// as it was.
		sealedUnder := c.Storage
		if sealedUnder == "" {
			sealedUnder = namespace
		}
		secret, err := s.hooks.OpenHook(sealedUnder, method, path, c.Version, c.Secret)
		if err != nil {
			return proof{}, "", fmt.Errorf("api: the secret of the webhook %s %s of %s could not be opened: %w", method, path, namespace, err)
		}
		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte(id + "." + stamp + "."))
		mac.Write(raw)
		want := mac.Sum(nil)
		for _, signature := range strings.Fields(signatures) {
			version, sum, ok := strings.Cut(signature, ",")
			if !ok || version != "v1" {
				continue
			}
			got, err := base64.StdEncoding.DecodeString(sum)
			if err == nil && hmac.Equal(got, want) {
				return proof{delivery: id}, "", nil
			}
		}
		return proof{}, refused, nil

	case "bearer":
		const refused = "the request carries no API token of a service account holding workflow:run on the workflow"
		served := r.Clone(r.Context())
		// The token and nothing else: a console session is a person's, and a webhook
		// "authenticates the caller, not a user".
		served.Header.Del("Cookie")
		if !strings.HasPrefix(served.Header.Get("Authorization"), "Bearer ") {
			return proof{}, refused, nil
		}
		as, err := s.router.identify(served)
		if err != nil {
			return proof{}, "", err
		}
		principal := string(as.Principal)
		if as.Refused != "" || principal == "" || Principal(principal) == BootstrapOperator || !strings.Contains(principal, "/") {
			return proof{}, refused, nil
		}
		held, err := s.router.allow(r.Context(), as, WorkflowRun, Target{Namespace: namespace, Workflow: hook.Workflow})
		if err != nil {
			return proof{}, "", err
		}
		if !held {
			return proof{}, refused, nil
		}
		return proof{caller: principal}, "", nil

	case "mtls":
		const refused = "the request presents no client certificate the webhook accepts, over the TLS this API serves itself"
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			return proof{}, refused, nil
		}
		leaf := r.TLS.PeerCertificates[0]
		if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
			return proof{}, refused, nil
		}
		c, err := credential()
		if err != nil {
			return proof{}, "", err
		}
		sum := sha256.Sum256(leaf.Raw)
		if len(c.Certificate) != len(sum) || subtle.ConstantTimeCompare(c.Certificate, sum[:]) != 1 {
			return proof{}, refused, nil
		}
		return proof{}, "", nil
	}
	return proof{}, "", fmt.Errorf("api: the webhook %s %s of %s authenticates by %q, which is no auth this API knows", method, path, namespace, auth)
}

// errNotText is a body that is neither JSON nor text.
var errNotText = errors.New("the request body is neither JSON, by its Content-Type, nor UTF-8 text, and trigger.body holds one or the other")

// hiddenHeaders are the request headers trigger.headers leaves out: the credentials a request
// carries, which a run is not to hold for whoever reads its context.
var hiddenHeaders = map[string]bool{"authorization": true, "proxy-authorization": true, "cookie": true}

// requestContext is the trigger root a request fires a run with: its body, parsed where its
// Content-Type says JSON and text otherwise, null where it is empty; its headers, their names in
// lower case and their values joined by commas as HTTP joins them, credentials left out; and its
// query, each name's first value.
func requestContext(r *http.Request, raw []byte) (map[string]any, error) {
	var body any
	switch kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); {
	case len(bytes.TrimSpace(raw)) == 0:
		body = nil
	case kind == "application/json" || strings.HasSuffix(kind, "+json"):
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		if err := d.Decode(&body); err != nil {
			return nil, fmt.Errorf("the request body says it is JSON and is not: %v", err)
		}
		if d.More() {
			return nil, errors.New("the request body says it is JSON and holds more than one document")
		}
		if n := trigger.Values(body); n > trigger.InputsMaxValues {
			return nil, &trigger.InputsTooLarge{Why: fmt.Sprintf("the request body holds %d values, and trigger.body holds at most %d, as many as a run's inputs may, since it is frozen on the run beside them", n, trigger.InputsMaxValues)}
		}
	case utf8.Valid(raw):
		body = string(raw)
	default:
		return nil, errNotText
	}
	headers := map[string]any{}
	for name, values := range r.Header {
		name = strings.ToLower(name)
		if !hiddenHeaders[name] {
			headers[name] = strings.Join(values, ", ")
		}
	}
	query := map[string]any{}
	for name, values := range r.URL.Query() {
		query[name] = values[0]
	}
	return map[string]any{"body": body, "headers": headers, "query": query}, nil
}

// asSupplied writes what map filled as a run's inputs are supplied: each number a json.Number, as
// written, and every value one JSON holds, which a timestamp or a duration an expression made is
// turned into as JSON writes it.
func asSupplied(filled map[string]any) (map[string]any, error) {
	b, err := json.Marshal(filled)
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var out map[string]any
	return out, d.Decode(&out)
}

// answerSync holds a sync webhook's request until its run ends, and answers the output it names: 200
// with the output's envelope where the run succeeded, 502 with the run and its state where it ended
// otherwise, and 202 with the run where it has not ended within hookSyncWait or the caller went.
func (s *Server) answerSync(w http.ResponseWriter, r *http.Request, namespace string, run agk.RunID, commit, output string) {
	ctx, cancel := context.WithTimeout(r.Context(), hookSyncWait)
	defer cancel()
	state := agk.Queued
	for wait := 100 * time.Millisecond; ; wait = min(2*wait, time.Second) {
		err := s.pool.In(ctx, namespace, func(ctx context.Context, ns *db.NS) error {
			var err error
			state, err = ns.RunState(ctx, run)
			return err
		})
		if err != nil && ctx.Err() == nil {
			s.report(err)
			fail(w, http.StatusInternalServerError, "the run could not be read")
			return
		}
		if state.Terminal() || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
	switch {
	case !state.Terminal():
		write(w, http.StatusAccepted, map[string]any{"run": string(run), "state": state.String(), "commit": commit})
		return
	case state != agk.Succeeded:
		// The workflow the webhook stands in front of did not answer: 502, as a gateway answers
		// for a server behind it, with the run to read why.
		write(w, http.StatusBadGateway, map[string]any{"run": string(run), "state": state.String(), "commit": commit})
		return
	}
	var out db.Output
	err := s.pool.In(r.Context(), namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		out, err = ns.Output(ctx, run, output)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoOutput):
		write(w, http.StatusBadGateway, map[string]any{"run": string(run), "state": state.String(), "commit": commit,
			"error": fmt.Sprintf("the run succeeded and records no output %s", output)})
		return
	case err != nil:
		s.report(err)
		fail(w, http.StatusInternalServerError, "the output could not be read")
		return
	}
	s.envelope(w, r, namespace, "the output "+output, out.Envelope)
}

// hookCredential is what PUT /api/v1/{ns}/workflows/{name}/webhooks/{method}/{path} writes: the
// secret an hmac webhook is signed with, or the certificate an mtls one accepts, one of them.
type hookCredential struct {
	Secret      *string
	Certificate *string
}

func (h *hookCredential) field(b *body, name string) error {
	switch name {
	case "secret":
		h.Secret = new(string)
		return text(b, h.Secret)
	case "certificate":
		h.Certificate = new(string)
		return text(b, h.Certificate)
	}
	return unknown(name)
}

// hookCertificateMaxBytes bounds the body of a credential written: a certificate in PEM, which is
// a few kilobytes, and a secret, which is at most a hundred bytes.
const hookCertificateMaxBytes = smallMaxBytes

// writeHookCredential writes what a webhook checks its caller against, the secret an hmac
// signature is made with or the certificate an mtls caller presents, and answers 204. "No role reads
// them back through the API, and a rotation is a write": nothing answers a secret, and writing one
// replaces the last. It is written for the workflow's webhook at the method and the path, whether or
// not the default branch arms one there yet, so that the secret is in place before the push that
// arms it and the webhook refuses nothing its sender signs.
func (s *Server) writeHookCredential(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	var in hookCredential
	if err := readAtMost(r, &in, hookCertificateMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	method, path := r.PathValue("method"), "/"+r.PathValue("path")
	switch {
	case !hookMethod.MatchString(method):
		fail(w, http.StatusBadRequest, fmt.Sprintf("%.64q is not a method a webhook answers, which is written in capitals, POST or PUT", method))
		return
	case len(path) > 255 || !hookPath.MatchString(path):
		fail(w, http.StatusBadRequest, fmt.Sprintf("%.300q is not a webhook's path: segments each beginning with a slash, then a letter, a digit, _, ~ or -, at most 255 characters", path))
		return
	case (in.Secret == nil) == (in.Certificate == nil):
		fail(w, http.StatusBadRequest, "the request body carries a secret, for a webhook authenticating by hmac, or a certificate, for one authenticating by mtls: one of the two")
		return
	}

	var write func(ctx context.Context, ns *db.NS) error
	written := "secret"
	if in.Secret != nil {
		key, err := hookSecret(*in.Secret)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if s.hooks == nil {
			fail(w, http.StatusServiceUnavailable, "this installation has no master key attached, and a webhook's secret is sealed under it: nothing is written")
			return
		}
		write = func(ctx context.Context, ns *db.NS) error {
			// Sealed under the namespace's storage name, the name it was created with, which a
			// rename leaves as it was, so that the secret opens for the namespace's whole life.
			storage, err := ns.Storage(ctx)
			if err != nil {
				return err
			}
			return ns.WriteHookSecret(ctx, over.Workflow, path, method, string(who), func(version int) (json.RawMessage, error) {
				return s.hooks.SealHook(storage, method, path, version, key)
			})
		}
	} else {
		written = "certificate"
		sum, err := hookCertificate(*in.Certificate)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		write = func(ctx context.Context, ns *db.NS) error {
			return ns.WriteHookCertificate(ctx, over.Workflow, path, method, string(who), sum)
		}
	}
	err := s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		if err := write(ctx, ns); err != nil {
			return err
		}
		return ns.Audit(ctx, audit.Record{
			Actor: string(who), Action: audit.WebhookCredentialWrite, Target: over.Workflow, Result: audit.Done,
			Detail: map[string]any{"path": path, "method": method, "written": written},
		})
	})
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "23503":
		// No workflow of that name, which the router's question did not settle.
		fail(w, http.StatusNotFound, "no such thing, or not yours")
		return
	case err != nil:
		s.report(err)
		fail(w, http.StatusInternalServerError, "the credential could not be written")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// hookSecret reads a webhook's secret as Standard Webhooks writes one: whsec_ and the base64 of its
// bytes, 24 to 64 of them.
func hookSecret(written string) ([]byte, error) {
	encoded, ok := strings.CutPrefix(written, "whsec_")
	if !ok {
		return nil, errors.New("a webhook's secret is written whsec_ and the base64 of its bytes, as Standard Webhooks writes one and its senders hand it over")
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("a webhook's secret is written whsec_ and the base64 of its bytes, and what follows whsec_ is not base64: %v", err)
	}
	if len(key) < hookSecretMin || len(key) > hookSecretMax {
		return nil, fmt.Errorf("a webhook's secret is %d to %d bytes, and this one is %d: 32 random bytes, openssl rand -base64 32, are one", hookSecretMin, hookSecretMax, len(key))
	}
	return key, nil
}

// hookCertificate reads the certificate an mtls webhook accepts, one in PEM, and answers the SHA-256
// of its DER, which is what a caller's is held to.
func hookCertificate(written string) ([]byte, error) {
	block, rest := pem.Decode([]byte(written))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("the certificate is written in PEM, one block of type CERTIFICATE: the client's own, which it presents")
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return nil, errors.New("the certificate is one PEM block, the client's own, and the webhook accepts that one certificate and no chain")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return nil, fmt.Errorf("the certificate could not be read: %v", err)
	}
	sum := sha256.Sum256(block.Bytes)
	return sum[:], nil
}
