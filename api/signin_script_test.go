package api

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/internal/webauthn"
	"github.com/agentiik/agentiik/internal/webauthn/webauthntest"
)

// The page's conversions, codec.js, between the JSON of a ceremony and what navigator.credentials
// takes and answers, run on a JavaScript engine against what the API writes and reads: base64url
// as Go's strict decoder reads it, byte for byte and refusal for refusal; the options the API
// issues, their bytes decoded and every other member passed on as written; and a browser's
// credentials, written as toJSON() writes them and read back by the API's own reader into the bytes
// they started as. page.js is parsed on the same engine, which is what a syntax error would stop.
//
// A browser is not needed for any of it, and no JavaScript engine is a dependency of the module:
// the test runs where node is, as it is on the hosted runners CI uses, or macOS's own jsc, and
// skips elsewhere, saying so.

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
