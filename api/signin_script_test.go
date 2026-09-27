package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/password"
	"github.com/agentiik/agentiik/internal/webauthn"
	"github.com/agentiik/agentiik/internal/webauthn/webauthntest"
)

// The page's scripts, run on a JavaScript engine: codec.js against what the API writes and reads,
// and page.js on a stand-in for the browser it runs in.
//
// A browser is not needed for either, and no JavaScript engine is a dependency of the module: the
// tests run where node is, as it is on the hosted runners CI uses, or macOS's own jsc, and skip
// elsewhere, saying so.

// jsc is where macOS keeps JavaScriptCore's shell.
const jsc = "/System/Library/Frameworks/JavaScriptCore.framework/Versions/Current/Helpers/jsc"

// javaScript is an engine to run a script file with, or the test skipped.
func javaScript(t *testing.T) string {
	t.Helper()
	if node, err := exec.LookPath("node"); err == nil {
		return node
	}
	if _, err := os.Stat(jsc); err == nil {
		return jsc
	}
	t.Skip("no JavaScript engine on this machine, node or macOS's jsc, to run the page's scripts on")
	return ""
}

// ran is what the harness printed.
type ran struct {
	Encoded      []string            `json:"encoded"`
	View         string              `json:"view"`
	Decoded      []map[string]string `json:"decoded"`
	Creation     map[string]any      `json:"creation"`
	Request      map[string]any      `json:"request"`
	Registration json.RawMessage     `json:"registration"`
	Assertion    json.RawMessage     `json:"assertion"`
	Parsed       bool                `json:"parsed"`
}

func random(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// decodedAs is options as the API wrote them, with the members named decoded to {"hex": ...}, as
// the harness describes what the page hands navigator.credentials. Each name is a path of members,
// [] standing for every element of an array.
func decodedAs(t *testing.T, written []byte, names ...string) map[string]any {
	t.Helper()
	var o map[string]any
	if err := json.Unmarshal(written, &o); err != nil {
		t.Fatal(err)
	}
	var decode func(v any, path []string) any
	decode = func(v any, path []string) any {
		if len(path) == 0 {
			b, err := base64.RawURLEncoding.Strict().DecodeString(v.(string))
			if err != nil {
				t.Fatal(err)
			}
			return map[string]any{"hex": hex.EncodeToString(b)}
		}
		if path[0] == "[]" {
			for i, e := range v.([]any) {
				v.([]any)[i] = decode(e, path[1:])
			}
			return v
		}
		m := v.(map[string]any)
		m[path[0]] = decode(m[path[0]], path[1:])
		return m
	}
	for _, name := range names {
		decode(o, strings.Split(name, "."))
	}
	return o
}

// readBack reads a credential the page wrote as the verification route reads it, and fails the test
// where the route would refuse it.
func readBack(t *testing.T, ceremony string, credential []byte) *publicKeyCredential {
	t.Helper()
	body := `{"ceremony":"` + ceremony + `","credential":` + string(credential) + `}`
	var ask ceremonyAnswered
	if err := readAtMost(httptest.NewRequest("POST", "/api/v1/auth/passkey/verify", strings.NewReader(body)), &ask, smallMaxBytes); err != nil {
		t.Fatalf("the %s the page wrote does not read: %s\n%s", ceremony, err, credential)
	}
	if err := ask.check(); err != nil {
		t.Fatalf("the %s the page wrote is refused: %s\n%s", ceremony, err, credential)
	}
	return ask.Credential
}

// The page's conversions, codec.js, between the JSON of a ceremony and what navigator.credentials
// takes and answers: base64url as Go's strict decoder reads it, byte for byte and refusal for
// refusal; the options the API issues, their bytes decoded and every other member passed on as
// written; and a browser's credentials, written as toJSON() writes them and read back by the API's
// own reader into the bytes they started as. page.js is parsed on the same engine, which is what a
// syntax error would stop.
func TestThePagesConversionsAreWhatTheAPIWritesAndReads(t *testing.T) {
	engine := javaScript(t)
	b64 := base64.RawURLEncoding

	// Every length up to 66 bytes, each remainder of three many times over, and every byte value.
	var inputs [][]byte
	for n := 0; n <= 66; n++ {
		inputs = append(inputs, random(t, n))
	}
	every := make([]byte, 256)
	for i := range every {
		every[i] = byte(i)
	}
	inputs = append(inputs, every)
	view := random(t, 20)

	// The texts each input encodes to, which read back, and those Go's strict decoder refuses: padding,
	// another alphabet, a length no bytes encode to, a bit past the last byte, and what is not
	// ASCII.
	var texts []string
	for _, in := range inputs {
		texts = append(texts, b64.EncodeToString(in))
	}
	texts = append(texts, "A", "AB=", "AB==", "AQ==", "A+B/", "A/Bq", "A B", "ab", "AR", "AAB", "AQ", "AAE", "é", "AAA\u0000", "AAAAA", "=")

	// Options as the API writes them.
	var creation creationOptions
	creation.RP.ID, creation.RP.Name = "agentiik.example.com", rpName
	creation.User.ID, creation.User.Name, creation.User.DisplayName = b64.EncodeToString(random(t, handleBytes)), "alice", "Alice Martin"
	creation.Challenge, creation.Timeout, creation.Attestation = b64.EncodeToString(random(t, challengeBytes)), 300000, "none"
	for _, alg := range webauthn.Algorithms() {
		creation.PubKeyCredParams = append(creation.PubKeyCredParams, credentialParameter{Type: "public-key", Alg: int(alg)})
	}
	creation.ExcludeCredentials = []credentialDescriptor{
		{Type: "public-key", ID: b64.EncodeToString(random(t, 32))},
		{Type: "public-key", ID: b64.EncodeToString(random(t, 17))},
	}
	creation.AuthenticatorSelection.ResidentKey, creation.AuthenticatorSelection.RequireResidentKey = "required", true
	creation.AuthenticatorSelection.UserVerification = "required"
	creationJSON, err := json.Marshal(creation)
	if err != nil {
		t.Fatal(err)
	}
	request := requestOptions{
		Challenge: b64.EncodeToString(random(t, challengeBytes)), Timeout: 300000, RPID: "agentiik.example.com",
		AllowCredentials: []credentialDescriptor{{Type: "public-key", ID: b64.EncodeToString(random(t, 16))}},
		UserVerification: "preferred",
	}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}

	// A browser's credentials, answering those options, as the API's own tests make them.
	browser := webauthntest.New("https://agentiik.example.com")
	made, passkey, err := browser.Create(creationJSON)
	if err != nil {
		t.Fatal(err)
	}
	made.Response.PublicKey = b64.EncodeToString(random(t, 91))
	got, err := browser.GetWith(requestJSON, passkey)
	if err != nil {
		t.Fatal(err)
	}
	raw := func(s string) string {
		b, err := b64.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return hex.EncodeToString(b)
	}

	vectors := map[string]any{
		"bytes": func() []string {
			var out []string
			for _, in := range inputs {
				out = append(out, hex.EncodeToString(in))
			}
			return out
		}(),
		"view":     hex.EncodeToString(view),
		"texts":    texts,
		"creation": json.RawMessage(creationJSON),
		"request":  json.RawMessage(requestJSON),
		"registration": map[string]any{
			"id": made.ID, "rawId": raw(made.RawID), "authenticatorAttachment": made.AuthenticatorAttachment,
			"clientDataJSON": raw(made.Response.ClientDataJSON), "attestationObject": raw(made.Response.AttestationObject),
			"authenticatorData": raw(made.Response.AuthenticatorData), "transports": made.Response.Transports,
			"publicKey": raw(made.Response.PublicKey), "publicKeyAlgorithm": made.Response.PublicKeyAlgorithm,
		},
		"assertion": map[string]any{
			"id": got.ID, "rawId": raw(got.RawID), "authenticatorAttachment": got.AuthenticatorAttachment,
			"clientDataJSON": raw(got.Response.ClientDataJSON), "authenticatorData": raw(got.Response.AuthenticatorData),
			"signature": raw(got.Response.Signature), "userHandle": raw(got.Response.UserHandle),
		},
	}
	written, err := json.Marshal(vectors)
	if err != nil {
		t.Fatal(err)
	}
	var script bytes.Buffer
	for _, part := range []string{"signin/assets/codec.js", "function pageScript() {", "signin/assets/page.js", "}", "vectors", "testdata/codec_harness.js"} {
		switch {
		case part == "vectors":
			script.WriteString("const vectors = " + string(written) + ";\n")
		case strings.HasSuffix(part, ".js") && strings.HasPrefix(part, "signin/"):
			b, err := signinFiles.ReadFile(part)
			if err != nil {
				t.Fatal(err)
			}
			script.Write(b)
		case strings.HasSuffix(part, ".js"):
			b, err := os.ReadFile(part)
			if err != nil {
				t.Fatal(err)
			}
			script.Write(b)
		default:
			script.WriteString(part)
		}
		script.WriteString("\n")
	}
	file := filepath.Join(t.TempDir(), "codec.js")
	if err := os.WriteFile(file, script.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(t.Context(), engine, file)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s ran the page's scripts and failed: %s\n%s%s", filepath.Base(engine), err, stderr.String(), stdout.String())
	}
	var out ran
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		t.Fatalf("the harness printed what does not read: %s\n%s", err, stdout.String())
	}

	if !out.Parsed {
		t.Error("page.js did not parse")
	}
	if len(out.Encoded) != len(inputs) {
		t.Fatalf("the page encoded %d of %d inputs", len(out.Encoded), len(inputs))
	}
	for i, in := range inputs {
		if want := b64.EncodeToString(in); out.Encoded[i] != want {
			t.Errorf("the page encodes %x as %q, and Go as %q", in, out.Encoded[i], want)
		}
	}
	if want := b64.EncodeToString(view); out.View != want {
		t.Errorf("the page encodes a view of %x as %q, and Go as %q", view, out.View, want)
	}
	if len(out.Decoded) != len(texts) {
		t.Fatalf("the page decoded %d of %d texts", len(out.Decoded), len(texts))
	}
	for i, text := range texts {
		want, err := b64.Strict().DecodeString(text)
		switch got := out.Decoded[i]; {
		case err != nil && got["refused"] == "":
			t.Errorf("the page reads %q, which Go refuses (%s), as %s", text, err, got["hex"])
		case err == nil && got["hex"] != hex.EncodeToString(want):
			t.Errorf("the page reads %q as %v, and Go as %x", text, got, want)
		}
	}

	if want := decodedAs(t, creationJSON, "challenge", "user.id", "excludeCredentials.[].id"); !reflect.DeepEqual(out.Creation, want) {
		t.Errorf("the page makes of the registration's options\n%v\nwhere\n%v\nwas expected", out.Creation, want)
	}
	if want := decodedAs(t, requestJSON, "challenge", "allowCredentials.[].id"); !reflect.DeepEqual(out.Request, want) {
		t.Errorf("the page makes of the sign-in's options\n%v\nwhere\n%v\nwas expected", out.Request, want)
	}

	// What toJSON() writes is what the API's tests send as a browser's credentials, and what the
	// API reads of it is the bytes the authenticator answered.
	for _, c := range []struct {
		ceremony string
		wrote    json.RawMessage
		want     webauthntest.Credential
	}{
		{"registration", out.Registration, made},
		{"assertion", out.Assertion, got},
	} {
		var wrote, want map[string]any
		expected, err := json.Marshal(c.want)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(c.wrote, &wrote); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(expected, &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(wrote, want) {
			t.Errorf("the page writes the %s\n%s\nwhere\n%s\nwas expected", c.ceremony, c.wrote, expected)
		}
		read := readBack(t, c.ceremony, c.wrote)
		for name, pair := range map[string][2]string{
			"rawId":             {hex.EncodeToString(read.RawID), raw(c.want.RawID)},
			"clientDataJSON":    {hex.EncodeToString(read.ClientDataJSON), raw(c.want.Response.ClientDataJSON)},
			"attestationObject": {hex.EncodeToString(read.AttestationObject), raw(c.want.Response.AttestationObject)},
			"authenticatorData": {hex.EncodeToString(read.AuthenticatorData), raw(c.want.Response.AuthenticatorData)},
			"signature":         {hex.EncodeToString(read.Signature), raw(c.want.Response.Signature)},
			"userHandle":        {hex.EncodeToString(read.UserHandle), raw(c.want.Response.UserHandle)},
		} {
			if pair[0] != pair[1] {
				t.Errorf("the API reads the %s's %s the page wrote as %s, and the authenticator answered %s", c.ceremony, name, pair[0], pair[1])
			}
		}
	}
}

// The page's script on a stand-in browser, testdata/page_harness.js: the DOM of each page as it is
// served, fetch answered as the API answers, navigator.credentials, location and history, driven
// through what a person does. Signed out, the page offers the passkey; a session that may only
// enrol is told what it needs and offered its sign-out, and no sign-in, agk login's included; a password refused by the policy is not
// offered again, whatever answers after; a sign-in agk login opened, by passkey or by password,
// hands on its loopback address and follows the API back to it and nowhere else; an enrolment
// link's code travels in the options' body alone and leaves the address once spent; where no
// passkey can run, an installation addressed by an IP address or a page that is not a secure
// context, the page says why and offers none; a credential with no toJSON() is written by codec;
// and every request is the page's own fetch, under the public URL's path, with credentials
// same-origin and no mode, which the API's Origin check needs.
func TestThePageScriptSignsInEnrolsAndSignsOutOnAStandInBrowser(t *testing.T) {
	onAStandInBrowser(t, javaScript(t), "null")
}

// The password form on the stand-in browser, against what POST /api/v1/auth/login answers over a real
// PostgreSQL, each answer as the route wrote it: a full session says who signed in and offers no
// other sign-in; one that may only enrol offers the way to enrol a passkey and the sign-out, and no
// sign-in; a wrong password keeps both forms and says so; passwords forbidden take the form away
// and keep the passkey; and too many attempts say, in minutes, how long Retry-After asks to wait.
func TestThePageScriptSignsInWithAPasswordAsTheRouteAnswers(t *testing.T) {
	engine := javaScript(t)
	answers, err := json.Marshal(passwordAnswers(t))
	if err != nil {
		t.Fatal(err)
	}
	onAStandInBrowser(t, engine, string(answers))
}

// routeAnswer is one answer of POST /api/v1/auth/login, as the harness replays it.
type routeAnswer struct {
	Login      string          `json:"login"`
	Status     int             `json:"status"`
	Body       json.RawMessage `json:"body"`
	RetryAfter string          `json:"retryAfter,omitempty"`
}

// passwordAnswers are what the route answers, over a real PostgreSQL: alice's password where
// passkeys are optional, bob's where one is required and he holds none, a wrong password, a
// password where they are forbidden, and carol's eleventh attempt in a quarter of an hour.
func passwordAnswers(t *testing.T) map[string]routeAnswer {
	t.Helper()
	pool, _ := dbtest.Open(t)
	now := time.Now().UTC().Truncate(time.Second)
	clock := func() time.Time { return now }
	const publicURL = "https://agentiik.example.com"
	p, err := NewPrincipals(pool, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.AcceptSessions(publicURL); err != nil {
		t.Fatal(err)
	}
	rt, err := NewRouter(p, p.Identify)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewPasswords(rt, PasswordOptions{Pool: pool, PublicURL: publicURL, Now: clock, Identify: p.Identify}); err != nil {
		t.Fatal(err)
	}
	policy := func(passwords, passkeys string) {
		bound := false
		if err := pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
			return w.SetInstallationPolicy(ctx, db.AuthPolicy{Password: passwords, Passkey: passkeys, UserVerification: "required", DeviceBoundOnly: &bound, MinPasskeys: 2}, now)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		for _, login := range []string{"alice", "bob", "carol"} {
			if err := w.CreateUser(ctx, db.User{Login: login, DisplayName: login}); err != nil {
				return err
			}
			hash, err := password.Hash(login + "'s own")
			if err != nil {
				return err
			}
			if err := w.AddCredential(ctx, db.Credential{ID: login + "-password", Login: login, Type: db.CredentialPassword, PasswordHash: hash}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	signIn := func(login, secret string) routeAnswer {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"login": login, "password": secret})
		r := httptest.NewRequestWithContext(t.Context(), "POST", "/api/v1/auth/login", bytes.NewReader(body))
		r.Header.Set("Origin", publicURL)
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, r)
		return routeAnswer{Login: login, Status: w.Code, Body: bytes.TrimSpace(w.Body.Bytes()), RetryAfter: w.Header().Get("Retry-After")}
	}

	answers := map[string]routeAnswer{}
	policy("allowed", "optional")
	answers["full"] = signIn("alice", "alice's own")
	answers["wrong"] = signIn("alice", "not alice's")
	policy("allowed", "required")
	answers["enrolment"] = signIn("bob", "bob's own")
	for range 10 {
		signIn("carol", "a guess")
	}
	answers["tooMany"] = signIn("carol", "carol's own")
	policy("forbidden", "required")
	answers["forbidden"] = signIn("alice", "alice's own")

	for name, want := range map[string]int{
		"full": http.StatusOK, "enrolment": http.StatusOK, "wrong": http.StatusUnauthorized,
		"forbidden": http.StatusForbidden, "tooMany": http.StatusTooManyRequests,
	} {
		if answers[name].Status != want {
			t.Fatalf("the route answered %s with %d %s", name, answers[name].Status, answers[name].Body)
		}
	}
	return answers
}

// onAStandInBrowser runs page.js on testdata/page_harness.js, with the pages as the templates write
// them, and the route's answers where answers is not null, and fails the test with what the harness
// says went otherwise.
func onAStandInBrowser(t *testing.T, engine, answers string) {
	t.Helper()
	pages, err := template.ParseFS(signinFiles, "signin/*.html")
	if err != nil {
		t.Fatal(err)
	}
	tag := regexp.MustCompile(`<[a-z]+\s[^>]*>`)
	id := regexp.MustCompile(`\sid="([^"]+)"`)
	hidden := regexp.MustCompile(`\shidden[\s>]`)
	data := regexp.MustCompile(`\s(data-[a-z-]+)="([^"]*)"`)
	written := map[string]any{}
	for name, page := range map[string]struct {
		file string
		data pageData
	}{
		"sign-in":          {"sign-in.html", pageData{Scripts: true, Passkeys: "available", Password: "withheld"}},
		"sign-in-password": {"sign-in.html", pageData{Scripts: true, Passkeys: "available", Password: "offered"}},
		"sign-in-terminal": {"sign-in.html", pageData{Scripts: true, Passkeys: "available", Password: "withheld",
			Redirect: "http://127.0.0.1:53682/callback", Challenge: "Ibi4l3hyoxxry38-L3XZ59u9IdHegygM4WK38DG2YKk"}},
		"sign-in-password-terminal": {"sign-in.html", pageData{Scripts: true, Passkeys: "available", Password: "offered",
			Redirect: "http://127.0.0.1:53682/callback", Challenge: "Ibi4l3hyoxxry38-L3XZ59u9IdHegygM4WK38DG2YKk"}},
		"sign-in-ip": {"sign-in.html", pageData{Scripts: true, Passkeys: "unavailable", Password: "offered"}},
		"enrol":      {"enrol.html", pageData{Scripts: true, Passkeys: "available"}},
		"enrol-ip":   {"enrol.html", pageData{Scripts: true, Passkeys: "unavailable"}},
	} {
		var b bytes.Buffer
		if err := pages.ExecuteTemplate(&b, page.file, page.data); err != nil {
			t.Fatal(err)
		}
		elements, attributes := map[string]any{}, map[string]string{}
		for _, element := range tag.FindAllString(b.String(), -1) {
			named := id.FindStringSubmatch(element)
			if named == nil {
				continue
			}
			elements[named[1]] = map[string]bool{"hidden": hidden.MatchString(element)}
			if named[1] == "page" {
				for _, d := range data.FindAllStringSubmatch(element, -1) {
					attributes[d[1]] = html.UnescapeString(d[2])
				}
			}
		}
		written[name] = map[string]any{"elements": elements, "data": attributes}
	}
	vectors, err := json.Marshal(written)
	if err != nil {
		t.Fatal(err)
	}

	read := func(b []byte, err error) []byte {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var script bytes.Buffer
	for _, part := range [][]byte{
		read(signinFiles.ReadFile("signin/assets/codec.js")),
		[]byte("function loadPage() {"), read(signinFiles.ReadFile("signin/assets/page.js")), []byte("}"),
		[]byte("const pages = " + string(vectors) + ";"),
		[]byte("const answers = " + answers + ";"),
		read(os.ReadFile("testdata/page_harness.js")),
	} {
		script.Write(part)
		script.WriteString("\n")
	}
	file := filepath.Join(t.TempDir(), "page.js")
	if err := os.WriteFile(file, script.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(t.Context(), engine, file)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s ran the page's script and failed: %s\n%s%s", filepath.Base(engine), err, stderr.String(), stdout.String())
	}
	var out struct {
		Failures []string `json:"failures"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		t.Fatalf("the harness printed what does not read: %s\n%s", err, stdout.String())
	}
	for _, f := range out.Failures {
		t.Error(f)
	}
}

// qrCase is one case of testdata/qr_vectors.json: a text, the version and the mask asked for where
// one is, and what the reference encoder drew, its version, its mask, and the SHA-256 of its rows
// joined by line feeds, with the rows themselves for a short text.
type qrCase struct {
	Text    string `json:"text"`
	Version *int   `json:"version"`
	Mask    *int   `json:"mask"`
	Want    struct {
		Version int      `json:"version"`
		Mask    int      `json:"mask"`
		SHA256  string   `json:"sha256"`
		Rows    []string `json:"rows"`
	} `json:"want"`
}

type qrCases struct {
	Source string   `json:"source"`
	Cases  []qrCase `json:"cases"`
}

func qrVectors(t *testing.T) qrCases {
	t.Helper()
	raw, err := os.ReadFile("testdata/qr_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v qrCases
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// drawn is what the reference encoder drew for text with nothing forced, as the page draws it.
func (v qrCases) drawn(text string) (qrCase, bool) {
	for _, c := range v.Cases {
		if c.Text == text && c.Version == nil && c.Mask == nil {
			return c, true
		}
	}
	return qrCase{}, false
}

func rowsHash(rows []string) string {
	sum := sha256.Sum256([]byte(strings.Join(rows, "\n")))
	return hex.EncodeToString(sum[:])
}

// The page's QR code encoder, qr.js, on a JavaScript engine, against what a reference encoder draws
// for the same texts, testdata/qr_vectors.json, whose source says which and how: under each of the
// eight masks; in each of the forty versions, at the most bytes it holds, and one byte past that,
// which takes the next; in a version larger than the text needs; UTF-8 past ASCII; and nothing at
// all. Every module is compared, the finders, timing, alignment, format and version information and
// the codewords with their error correction, and so is the mask chosen by the penalty where none is
// asked for. A text too long for any version is refused rather than drawn.
func TestTheQRCodeIsTheReferenceEncoders(t *testing.T) {
	engine := javaScript(t)
	raw, err := os.ReadFile("testdata/qr_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	vectors := qrVectors(t)
	var script bytes.Buffer
	for _, part := range [][]byte{
		func() []byte {
			b, err := signinFiles.ReadFile("signin/assets/qr.js")
			if err != nil {
				t.Fatal(err)
			}
			return b
		}(),
		[]byte("const vectors = " + string(raw) + ";"),
		func() []byte {
			b, err := os.ReadFile("testdata/qr_harness.js")
			if err != nil {
				t.Fatal(err)
			}
			return b
		}(),
	} {
		script.Write(part)
		script.WriteString("\n")
	}
	file := filepath.Join(t.TempDir(), "qr.js")
	if err := os.WriteFile(file, script.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(t.Context(), engine, file)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s ran qr.js and failed: %s\n%s%s", filepath.Base(engine), err, stderr.String(), stdout.String())
	}
	var out struct {
		Results []struct {
			Version int      `json:"version"`
			Mask    int      `json:"mask"`
			Rows    []string `json:"rows"`
			Error   string   `json:"error"`
		} `json:"results"`
		Refused bool `json:"refused"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		t.Fatalf("the harness printed what does not read: %s\n%s", err, stdout.String())
	}
	if len(out.Results) != len(vectors.Cases) || len(vectors.Cases) < 50 {
		t.Fatalf("qr.js answered %d of %d cases", len(out.Results), len(vectors.Cases))
	}
	for i, c := range vectors.Cases {
		got := out.Results[i]
		name := fmt.Sprintf("case %d, %d bytes", i, len(c.Text))
		switch {
		case got.Error != "":
			t.Errorf("%s: qr.js refused it: %s", name, got.Error)
		case got.Version != c.Want.Version || got.Mask != c.Want.Mask:
			t.Errorf("%s: qr.js drew version %d under mask %d, and the reference version %d under mask %d", name, got.Version, got.Mask, c.Want.Version, c.Want.Mask)
		case rowsHash(got.Rows) != c.Want.SHA256:
			if c.Want.Rows != nil {
				t.Errorf("%s: qr.js drew\n%s\nand the reference\n%s", name, strings.Join(got.Rows, "\n"), strings.Join(c.Want.Rows, "\n"))
			} else {
				t.Errorf("%s: qr.js drew modules other than the reference's", name)
			}
		}
	}
	if !out.Refused {
		t.Error("a text too long for any version was not refused with a RangeError")
	}
}
