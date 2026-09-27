package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/agentiik/agentiik/db"
)

// The bodies of the passkey ceremonies, read one token at a time as every body is. Both routes
// answer anybody, so what a body costs to read is what anybody on the network can make the API
// spend: each is capped at smallMaxBytes, and the objects a browser writes, which WebAuthn leaves
// open, are counted as their members are read.

// enrolmentCode is openapi.json's enrolmentCode: agkenrol_ and 256 bits of base64url.
var enrolmentCode = regexp.MustCompile(`^agkenrol_[A-Za-z0-9_-]{43,}$`)

// ceremonyAsked is openapi.json's passkeyOptionsRequest.
type ceremonyAsked struct {
	Ceremony string
	Code     string

	// coded is whether the request wrote a code, null being none.
	coded bool
}

func (q *ceremonyAsked) field(b *body, name string) error {
	switch name {
	case "ceremony":
		return text(b, &q.Ceremony)
	case "code":
		q.coded = b.d.PeekKind() != jsontext.KindNull
		return text(b, &q.Code)
	}
	return unknown(name)
}

// check refuses what the schema refuses: a ceremony of another name, a code beside an assertion,
// which names nobody, and a code outside its grammar.
func (q ceremonyAsked) check() error {
	switch {
	case q.Ceremony != db.CeremonyRegistration && q.Ceremony != db.CeremonyAssertion:
		return errors.New("ceremony: a ceremony is registration, to record a new passkey, or assertion, to sign in with one")
	case q.Ceremony == db.CeremonyAssertion && q.coded:
		return errors.New("code: an assertion names nobody, since the passkey the authenticator offers names the account, and carries no enrolment code")
	case q.coded && !enrolmentCode.MatchString(q.Code):
		// The code is not repeated, since it may be most of a real one.
		return errors.New("code: an enrolment code is agkenrol_ and at least 43 base64url characters, as the link after its # carries it")
	}
	return nil
}

// ceremonyAnswered is openapi.json's passkeyVerifyRequest.
type ceremonyAnswered struct {
	Ceremony   string
	Credential *publicKeyCredential
	Label      string

	// Terminal is what agk login opened the sign-in page with, for an assertion it started, and
	// nil for any other ceremony.
	Terminal *TerminalSignIn

	// labelled is whether the request wrote a label, null being none.
	labelled bool
}

func (q *ceremonyAnswered) field(b *body, name string) error {
	switch name {
	case "ceremony":
		return text(b, &q.Ceremony)
	case "credential":
		if b.d.PeekKind() == jsontext.KindNull {
			_, err := b.d.ReadToken()
			return malformed(err)
		}
		q.Credential = &publicKeyCredential{}
		return q.Credential.read(b)
	case "label":
		q.labelled = b.d.PeekKind() != jsontext.KindNull
		return text(b, &q.Label)
	case "terminal":
		return readTerminal(b, &q.Terminal)
	}
	return unknown(name)
}

// check refuses what the schema refuses before anything is verified: a ceremony of another name, a
// label beside an assertion, a label a passkey cannot be given, agk login's terminal beside a
// registration or outside its grammar, and a credential missing a member its ceremony needs.
func (q ceremonyAnswered) check() error {
	switch {
	case q.Ceremony != db.CeremonyRegistration && q.Ceremony != db.CeremonyAssertion:
		return errors.New("ceremony: a ceremony is registration, to record a new passkey, or assertion, to sign in with one")
	case q.Credential == nil:
		return errors.New("credential: the request carries no credential, which is what PublicKeyCredential.toJSON() returned")
	case q.labelled && q.Ceremony == db.CeremonyAssertion:
		return errors.New("label: a label names a new passkey, and an assertion registers none")
	case q.labelled && q.Label == "":
		return errors.New("label: a label is at least one character, or left out")
	case utf8.RuneCountInString(q.Label) > labelMax:
		return fmt.Errorf("label: a passkey's label is at most %d characters and this one is %d", labelMax, utf8.RuneCountInString(q.Label))
	case strings.ContainsFunc(q.Label, unicode.IsControl):
		return errors.New("label: it holds a line break or another control character, and it is one line, shown in a console as it is")
	case q.Terminal != nil && q.Ceremony == db.CeremonyRegistration:
		return errors.New("terminal: agk login signs in with an assertion, and a registration hands it no code")
	case q.Terminal != nil:
		if err := q.Terminal.check(); err != nil {
			return err
		}
	}
	return q.Credential.check(q.Ceremony)
}

// publicKeyCredential is what PublicKeyCredential.toJSON() returns, a RegistrationResponseJSON or
// an AuthenticationResponseJSON, holding the members the API reads, decoded from base64url.
type publicKeyCredential struct {
	ID    string
	RawID []byte
	Type  string

	ClientDataJSON    []byte
	AttestationObject []byte
	AuthenticatorData []byte
	Signature         []byte
	UserHandle        []byte
}

// openMembers is how many members an object a browser writes may hold here. A browser writes
// eight at most, and the rest are skipped unread, but skipping still reads them, so they are
// counted as a collection is.
const openMembers = 32

// credentialIDMax is the longest credential ID, in bytes, as WebAuthn bounds one (§5.1).
const credentialIDMax = 1023

func (c *publicKeyCredential) read(b *body) error {
	return b.open(map[string]func() error{
		"id":    func() error { return text(b, &c.ID) },
		"rawId": func() error { return b.base64url(&c.RawID, "rawId") },
		"type":  func() error { return text(b, &c.Type) },
		"response": func() error {
			if b.d.PeekKind() == jsontext.KindNull {
				_, err := b.d.ReadToken()
				return malformed(err)
			}
			return b.open(map[string]func() error{
				"clientDataJSON":    func() error { return b.base64url(&c.ClientDataJSON, "clientDataJSON") },
				"attestationObject": func() error { return b.base64url(&c.AttestationObject, "attestationObject") },
				"authenticatorData": func() error { return b.base64url(&c.AuthenticatorData, "authenticatorData") },
				"signature":         func() error { return b.base64url(&c.Signature, "signature") },
				"userHandle":        func() error { return b.base64url(&c.UserHandle, "userHandle") },
			})
		},
	})
}

// check refuses a credential missing what its ceremony reads: the ID, carried twice and held to
// itself, the type, the client data, and the attestation object of a registration or the
// authenticator data, the signature and the user handle of an assertion. The user handle is
// required, since the options list no credential, and an assertion answering them names its
// account by it alone.
func (c publicKeyCredential) check(ceremony string) error {
	var missing []string
	for _, m := range []struct {
		name  string
		empty bool
		in    string
	}{
		{"rawId", len(c.RawID) == 0, ""},
		{"response.clientDataJSON", len(c.ClientDataJSON) == 0, ""},
		{"response.attestationObject", len(c.AttestationObject) == 0, db.CeremonyRegistration},
		{"response.authenticatorData", len(c.AuthenticatorData) == 0, db.CeremonyAssertion},
		{"response.signature", len(c.Signature) == 0, db.CeremonyAssertion},
		{"response.userHandle", len(c.UserHandle) == 0, db.CeremonyAssertion},
	} {
		if m.empty && (m.in == "" || m.in == ceremony) {
			missing = append(missing, m.name)
		}
	}
	switch {
	case c.Type != "public-key":
		return errors.New("credential.type: a passkey's credential is of type public-key")
	case len(missing) > 0:
		return fmt.Errorf("credential: %s's credential carries %s, and this one does not", ceremonyNoun(ceremony), strings.Join(missing, ", "))
	case len(c.RawID) > credentialIDMax:
		return fmt.Errorf("credential.rawId: a credential ID is at most %d bytes and this one is %d", credentialIDMax, len(c.RawID))
	case c.ID != base64.RawURLEncoding.EncodeToString(c.RawID):
		return errors.New("credential.id: the ID is carried twice, as id and as rawId, and these two are not the same")
	}
	return nil
}

// ceremonyNoun is a ceremony's name after its article.
func ceremonyNoun(ceremony string) string {
	if ceremony == db.CeremonyRegistration {
		return "a registration"
	}
	return "an assertion"
}

// open reads an object a browser writes, which WebAuthn leaves open: a member read has a reader for
// is read, once, and any other is skipped unread, since refusing a member a newer browser adds would
// refuse the browser. Null is an object with no members, as it is to object.
func (b *body) open(read map[string]func() error) error {
	var seen []string
	return b.object(openMembers, fmt.Sprintf("an object a browser writes holds at most %d members here", openMembers), func(name string) error {
		f, known := read[name]
		if !known {
			return malformed(b.d.SkipValue())
		}
		if slices.Contains(seen, name) {
			return fmt.Errorf("the request body writes %.64q twice in one object, and a member written twice is refused rather than decided by whichever came last", name)
		}
		seen = append(seen, name)
		return f()
	})
}

// base64url reads what WebAuthn's JSON writes bytes as: base64url with no padding, in a string,
// in the one encoding a value has, so that two spellings of the same bytes are not two answers.
// Null is nil.
//
// Decoded from the body where it lies, as bytes reads standard base64, since a client data or an
// attestation object is most of a body anybody may send.
func (b *body) base64url(into *[]byte, what string) error {
	v, err := b.d.ReadValue()
	if err != nil {
		return malformed(err)
	}
	switch v.Kind() {
	case jsontext.KindNull:
		*into = nil
		return nil
	case jsontext.KindString:
	default:
		return b.mistyped(v.Kind(), "a string of base64url")
	}
	encoded := []byte(v[1 : len(v)-1])
	if bytes.IndexByte(encoded, '\\') >= 0 {
		if encoded, err = jsontext.AppendUnquote(nil, v); err != nil {
			return malformed(err)
		}
	}
	strict := base64.RawURLEncoding.Strict()
	out := make([]byte, strict.DecodedLen(len(encoded)))
	n, err := strict.Decode(out, encoded)
	if err != nil {
		return fmt.Errorf("%s: it is not base64url with no padding, which is how WebAuthn writes bytes", what)
	}
	*into = out[:n]
	return nil
}
