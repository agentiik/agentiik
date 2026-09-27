package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/ulid"
	"github.com/agentiik/agentiik/internal/webauthn"
)

// The passkey ceremonies, POST /api/v1/auth/passkey/options and POST /api/v1/auth/passkey/verify:
// the options a browser hands navigator.credentials, and the verification of what it answers,
// with internal/webauthn. What a ceremony needs kept between the two is kept in the database, so
// that the options and the verification may reach two replicas of the API.
//
// # Who a ceremony is for
//
// An assertion names nobody: its options list no credential, so that the authenticator offers
// whichever passkey it holds for this Relying Party, no login is sent before the ceremony and none
// can be probed for. The passkey it signs with names the account, and the user handle it hands
// back has to be the one that account's passkeys were registered under.
//
// A registration names the user it enrols: the one an enrolment link's code was issued for, or
// the one whose session the request carries where it carries no code. A code decides over a
// session, since the page an enrolment link opens enrols the link's user whoever last signed in in
// that browser. The code is held against the challenge and spent by the verification that records
// the passkey, so a ceremony dismissed halfway does not burn the link; a registration an enrolment
// code started signs its user in, since they have just proved a passkey, with user verification
// where the policy requires it, and a first sign-in is what gives a user their personal namespace.
// A registration a session started opens no other session: its user is signed in already.
//
// Only a user registers a passkey, and so only a user signs in with one: a credential belongs to
// a row of users, and a service account holds API tokens and nothing else.
//
// # The Relying Party and the origin
//
// The Relying Party Identifier is the host of the public URL, AGK_PUBLIC_URL or AGK_PROXY_URL
// behind a proxy, and the one origin a ceremony is accepted from is the public URL's, where the
// API serves its sign-in page. An installation addressed by an IP address runs no ceremony, since
// a browser refuses an IP address as a Relying Party Identifier: both routes say so with 409.
//
// Both routes are refused, with 403, to a request whose Origin header is not the public URL's: the
// sign-in page asks with fetch, which sends it, and a form posted from another page, or from a
// sandboxed frame that sends Origin: null, would otherwise sign a browser in as whoever the other
// page's author chose.

// PasskeyOptions are what the passkey ceremonies are given.
type PasskeyOptions struct {
	Pool *db.Pool

	// PublicURL is AGK_PUBLIC_URL, or AGK_PROXY_URL behind a proxy: its host is the Relying Party
	// Identifier, and its origin the one a ceremony is accepted from.
	PublicURL string

	// Identify reads the session a registration with no code is made from. It is the
	// installation's own, Principals.Identify, and not the router's, which refuses a session that
	// may only enrol everywhere, where enrolling is the one thing such a session is for.
	Identify Identify

	// Now is the clock challenges lapse and sessions open by, the wall clock where it is nil.
	Now func() time.Time

	// Trouble is told what went wrong where a refusal is answered all the same: a failed sign-in
	// that could not be recorded.
	Trouble func(error)

	// SignIns is what the sign-in routes share: where a sign-in comes from, and the bound on the
	// failures they record. Nil is one of the ceremonies' own, reading no proxy's header.
	SignIns *SignIns
}

// PasskeyAPI is the passkey ceremonies.
type PasskeyAPI struct {
	pool     *db.Pool
	identify Identify
	now      func() time.Time
	trouble  func(error)

	// rpID and origin are the Relying Party Identifier and the one origin, and unavailable why no
	// ceremony runs, empty where they do.
	rpID, origin, unavailable string

	signIns *SignIns

	// checked is run between a registration's verification and the transaction that writes it,
	// where a test changes what was checked. Nil outside tests.
	checked func()
}

// The bytes a ceremony mints: a challenge and a user handle are each 32 random bytes, 256 bits,
// what every credential of the installation carries, and twice what WebAuthn asks of a challenge.
const (
	challengeBytes = 32
	handleBytes    = 32
)

// rpName is what an authenticator shows the person as the site a passkey is for.
const rpName = "Agentiik"

// labelMax is the longest label a passkey is given, in characters, as $defs/passkey holds it.
const labelMax = 256

// The sentences a ceremony is refused with.
const (
	// ipAddressed is an installation addressed by an IP address, where a browser runs no ceremony.
	ipAddressed = "this installation is addressed by an IP address, which a browser refuses as a Relying Party Identifier, so no passkey ceremony runs here: address it by a name, or sign in with a password"

	// ceremonyOrigin is a request not from the sign-in page.
	ceremonyOrigin = "a passkey ceremony is started and finished from the pages of this installation's public URL, and this request's Origin header names another or none"

	// noRegistrar is a registration with neither an enrolment code nor a session.
	noRegistrar = "a passkey is registered from a signed-in browser's session, or with the code of an enrolment link or a recovery code, and this request carries neither"

	// codeOpensNothing is an enrolment code used, lapsed, replaced or never issued.
	codeOpensNothing = "that code opens nothing: it was used already, or it has lapsed or been replaced by a fresher link. Ask an administrator for a new one"

	// noSignIn is every assertion that does not sign anybody in, one sentence for every reason, so
	// that a failure teaches nothing about which part was wrong.
	noSignIn = "that passkey signs nobody in: the sign-in lapsed or was answered already, the passkey is not one this installation registered, or its account opens no session. Start the sign-in again"

	// noRegistration is every registration that does not verify, one sentence for every reason.
	noRegistration = "that passkey was not registered: the ceremony lapsed or was answered already, or what started it opens nothing any more. Start again, from a fresh link where one started it"

	// syncedRefused is a synced passkey where device_bound_only applies to the account.
	syncedRefused = "this account signs in with device-bound passkeys alone, and this passkey is synced: its Backup Eligibility flag is set, so its provider may copy it off the device"
)

// NewPasskeys registers the passkey ceremonies on a router.
func NewPasskeys(rt *Router, o PasskeyOptions) (*PasskeyAPI, error) {
	switch {
	case rt == nil:
		return nil, errors.New("api: no router")
	case o.Pool == nil:
		return nil, errors.New("api: no database, and a challenge is kept there between the options and the verification")
	case o.Identify == nil:
		return nil, errors.New("api: no way to read a session, and a signed-in user registers a passkey from theirs")
	}
	origin, err := originOf(o.PublicURL)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(o.PublicURL)
	if err != nil {
		return nil, err
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	if o.SignIns == nil {
		o.SignIns = NewSignIns(false)
	}
	s := &PasskeyAPI{
		pool: o.Pool, identify: o.Identify, now: o.Now, trouble: o.Trouble,
		rpID: strings.ToLower(u.Hostname()), origin: origin, signIns: o.SignIns,
	}
	if net.ParseIP(s.rpID) != nil {
		s.rpID, s.unavailable = "", ipAddressed
	}

	public := Public{Why: "a passkey ceremony is how somebody proves who they are: an assertion is authenticated by the passkey it verifies, and a registration by the enrolment code it carries or the session it reads itself, since the router refuses a session that may only enrol, and enrolling is what such a session is for"}
	for _, r := range []struct {
		pattern string
		handler Handler
	}{
		{"/api/v1/auth/passkey/options", s.options},
		{"/api/v1/auth/passkey/verify", s.verify},
	} {
		if err := rt.Handle("POST", r.pattern, public, r.handler); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// fromThePage says whether a request comes from the pages of the public URL, the one origin a
// ceremony runs on, as its one Origin header says.
func (s *PasskeyAPI) fromThePage(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	return len(origins) == 1 && origins[0] == s.origin
}

// verification is how a ceremony's options write the user verification asked for.
func verification(required bool) string {
	if required {
		return "required"
	}
	return "preferred"
}

// randomBytes is n bytes of the operating system's generator.
func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("api: %d random bytes could not be read: %w", n, err)
	}
	return b, nil
}

// b64 is how WebAuthn writes bytes in JSON: base64url with no padding.
var b64 = base64.RawURLEncoding

// credentialParameter is one entry of a registration's pubKeyCredParams.
type credentialParameter struct {
	Type string `json:"type"`
	Alg  int    `json:"alg"`
}

// credentialDescriptor is PublicKeyCredentialDescriptorJSON, a credential named by its ID.
type credentialDescriptor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// creationOptions is PublicKeyCredentialCreationOptionsJSON, the members the API sets, spelled as
// the Recommendation spells them, since the browser reads them.
type creationOptions struct {
	RP struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"rp"`
	User struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	} `json:"user"`
	Challenge              string                 `json:"challenge"`
	PubKeyCredParams       []credentialParameter  `json:"pubKeyCredParams"`
	Timeout                int64                  `json:"timeout"`
	ExcludeCredentials     []credentialDescriptor `json:"excludeCredentials"`
	AuthenticatorSelection struct {
		ResidentKey        string `json:"residentKey"`
		RequireResidentKey bool   `json:"requireResidentKey"`
		UserVerification   string `json:"userVerification"`
	} `json:"authenticatorSelection"`
	Attestation string `json:"attestation"`
}

// requestOptions is PublicKeyCredentialRequestOptionsJSON.
type requestOptions struct {
	Challenge        string                 `json:"challenge"`
	Timeout          int64                  `json:"timeout"`
	RPID             string                 `json:"rpId"`
	AllowCredentials []credentialDescriptor `json:"allowCredentials"`
	UserVerification string                 `json:"userVerification"`
}

// CeremonyOptions is openapi.json's passkeyOptions: the options of the ceremony started.
type CeremonyOptions struct {
	Ceremony string `json:"ceremony"`
	Options  any    `json:"options"`
}

// options is POST /api/v1/auth/passkey/options.
func (s *PasskeyAPI) options(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	if s.unavailable != "" {
		fail(w, http.StatusConflict, s.unavailable)
		return
	}
	if !s.fromThePage(r) {
		fail(w, http.StatusForbidden, ceremonyOrigin)
		return
	}
	var ask ceremonyAsked
	if err := readAtMost(r, &ask, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	if err := ask.check(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	value, err := randomBytes(challengeBytes)
	if err != nil {
		fail(w, http.StatusInternalServerError, "the ceremony could not be started")
		return
	}
	// To the microsecond the database keeps, so that the challenge lapses when its row says.
	now := s.now().Truncate(time.Microsecond)
	issued := db.Challenge{Value: value, Ceremony: ask.Ceremony, IssuedAt: now, ExpiresAt: now.Add(db.ChallengeLife)}
	timeout := db.ChallengeLife.Milliseconds()

	if ask.Ceremony == db.CeremonyAssertion {
		var required bool
		err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
			var err error
			if required, err = wide.UserVerificationRequired(ctx); err != nil {
				return err
			}
			return wide.IssueChallenge(ctx, issued)
		})
		var full *db.TooManyChallenges
		switch {
		case errors.As(err, &full):
			tooManyCeremonies(w, full, now)
			return
		case err != nil:
			fail(w, http.StatusInternalServerError, "the sign-in could not be started")
			return
		}
		// Required where any policy requires it, since whose policy applies is known only once
		// the passkey names its account: asked for less, an authenticator could skip the
		// verification a namespace's policy then refuses the sign-in for.
		shownOnce(w, http.StatusOK, CeremonyOptions{Ceremony: ask.Ceremony, Options: requestOptions{
			Challenge: b64.EncodeToString(value), Timeout: timeout, RPID: s.rpID,
			AllowCredentials: []credentialDescriptor{}, UserVerification: verification(required),
		}})
		return
	}

	var codeHash []byte
	login := ""
	if ask.coded {
		sum := sha256.Sum256([]byte(ask.Code))
		codeHash = sum[:]
	} else {
		as, err := s.registrar(r)
		if err != nil {
			fail(w, http.StatusInternalServerError, "the request could not be authenticated")
			return
		}
		if as.Principal == "" {
			unauthenticated(w, as)
			return
		}
		// A passkey registered from a session is a way in that outlives it, which the session
		// alone does not give: see proofLife.
		if !provedSince(as.ProvedAt, now) {
			askAgain(w)
			return
		}
		login = string(as.Principal)
	}
	fresh, err := randomBytes(handleBytes)
	if err != nil {
		fail(w, http.StatusInternalServerError, "the ceremony could not be started")
		return
	}
	var o creationOptions
	err = s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		if codeHash != nil {
			code, err := wide.EnrolmentCodeByHash(ctx, codeHash, now)
			if err != nil {
				return err
			}
			login, issued.EnrolmentCode = code.Login, codeHash
		}
		user, err := wide.User(ctx, login)
		if err != nil {
			return err
		}
		handle, err := wide.PasskeyHandle(ctx, login, fresh)
		if err != nil {
			return err
		}
		held, err := wide.CredentialsOf(ctx, login)
		if err != nil {
			return err
		}
		policy, err := policyFor(ctx, wide, login, now, false)
		if err != nil {
			return err
		}
		issued.Login = login
		if err := wide.IssueChallenge(ctx, issued); err != nil {
			return err
		}

		o.RP.ID, o.RP.Name = s.rpID, rpName
		o.User.ID, o.User.Name, o.User.DisplayName = b64.EncodeToString(handle), user.Login, user.DisplayName
		o.Challenge, o.Timeout, o.Attestation = b64.EncodeToString(value), timeout, "none"
		for _, alg := range webauthn.Algorithms() {
			o.PubKeyCredParams = append(o.PubKeyCredParams, credentialParameter{Type: "public-key", Alg: int(alg)})
		}
		// The passkeys the user holds, so that an authenticator holding one of them makes no
		// second for the same account.
		o.ExcludeCredentials = []credentialDescriptor{}
		for _, c := range held {
			if c.Type == db.CredentialPasskey {
				o.ExcludeCredentials = append(o.ExcludeCredentials, credentialDescriptor{Type: "public-key", ID: c.ID})
			}
		}
		// Discoverable, so that signing in needs no login typed first, and asked for in both of
		// its spellings, since an authenticator of Level 1 reads only the second.
		o.AuthenticatorSelection.ResidentKey, o.AuthenticatorSelection.RequireResidentKey = "required", true
		o.AuthenticatorSelection.UserVerification = verification(policy.userVerification)
		return nil
	})
	var full *db.TooManyChallenges
	switch {
	case errors.As(err, &full):
		tooManyCeremonies(w, full, now)
	case ask.coded && (errors.Is(err, db.ErrNoEnrolmentCode) || errors.Is(err, db.ErrNoPrincipal)):
		// A code that opens nothing, or whose user was removed between the code being read and
		// the challenge being issued, which took the code with them: a sign-in refused, since a
		// registration a code starts signs its user in, and the code's row says why.
		s.refuseCode(r, codeFailure{code: codeHash, detail: map[string]any{"credential_type": db.CredentialPasskey}}, now)
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, codeOpensNothing)
	case errors.Is(err, db.ErrNoPrincipal):
		// A user removed between the session being read and the challenge being issued, which
		// the removal ended.
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, noSession)
	case err != nil:
		fail(w, http.StatusInternalServerError, "the registration could not be started")
	default:
		shownOnce(w, http.StatusOK, CeremonyOptions{Ceremony: ask.Ceremony, Options: o})
	}
}

// tooManyCeremonies answers a ceremony refused because the installation keeps as many challenges
// open as it may, db.ChallengesLive: 503 with Retry-After, the whole seconds until the oldest
// lapses, rounded up, since a client asking again a moment early is refused again. Nothing was
// written, and nothing is recorded: the ceremony did not start, and what refused it is the
// installation's load rather than anybody's credential.
func tooManyCeremonies(w http.ResponseWriter, full *db.TooManyChallenges, now time.Time) {
	wait := max(full.FreeAt.Sub(now), time.Second)
	w.Header().Set("Retry-After", strconv.Itoa(int((wait+time.Second-1)/time.Second)))
	fail(w, http.StatusServiceUnavailable, fmt.Sprintf("the installation has %d passkey ceremonies under way, as many as it keeps at once, and starts another once the oldest lapses, within %d minutes: try again then", db.ChallengesLive, int(db.ChallengeLife/time.Minute)))
}

// registrar is who a registration with no code is for: the user whose session the request
// carries. A bearer token registers nothing, since a passkey is registered in a browser, on the
// sign-in page, and a request carrying one is answered as one carrying nothing that could; so is the
// bootstrap token's operator, who is nobody's account.
//
// The session a password opened that may only enrol is the one this reads Identify rather than the
// router's for, since the router refuses it everywhere, and it registers here like any other.
func (s *PasskeyAPI) registrar(r *http.Request) (Identity, error) {
	if _, bearer := bearerOf(r); bearer {
		return Identity{Refused: noRegistrar}, nil
	}
	as, err := s.identify(r)
	if err != nil {
		return Identity{}, err
	}
	switch {
	case as.Principal == "" && as.Refused != "":
		// A session that opens nothing, or one this request may not carry, answered as
		// Identify says.
		return as, nil
	case as.Principal == "" || as.Token != "" || as.Principal == BootstrapOperator:
		return Identity{Refused: noRegistrar}, nil
	}
	return as, nil
}

// Passkey is a passkey as the API answers one, $defs/passkey: never its public key.
type Passkey struct {
	Type           string    `json:"type"`
	ID             string    `json:"id"`
	Label          string    `json:"label,omitempty"`
	BackupEligible bool      `json:"backup_eligible"`
	BackupState    bool      `json:"backup_state"`
	Kind           string    `json:"kind"`
	CreatedAt      time.Time `json:"created_at"`
	LastUsedAt     time.Time `json:"last_used_at,omitzero"`
}

func passkeyOf(c db.Credential) Passkey {
	p := Passkey{
		Type: db.CredentialPasskey, ID: c.ID, Label: c.Label,
		BackupEligible: c.BackupEligible, BackupState: c.BackupState,
		Kind:      string(webauthn.Credential{BackupEligible: c.BackupEligible}.Kind()),
		CreatedAt: c.CreatedAt.UTC(),
	}
	if !c.LastUsedAt.IsZero() {
		p.LastUsedAt = c.LastUsedAt.UTC()
	}
	return p
}

// Verified is openapi.json's passkeyVerified: what a verified ceremony did.
type Verified struct {
	Ceremony   string   `json:"ceremony"`
	Login      string   `json:"login"`
	Credential *Passkey `json:"credential,omitempty"`

	// RedirectTo is where the sign-in page sends the browser next, for an assertion agk login
	// started: its loopback address with a one-time code (handOff).
	RedirectTo string `json:"redirect_to,omitempty"`
}

// verify is POST /api/v1/auth/passkey/verify.
func (s *PasskeyAPI) verify(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	if s.unavailable != "" {
		fail(w, http.StatusConflict, s.unavailable)
		return
	}
	if !s.fromThePage(r) {
		fail(w, http.StatusForbidden, ceremonyOrigin)
		return
	}
	var ask ceremonyAnswered
	if err := readAtMost(r, &ask, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	if err := ask.check(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	now := s.now().Truncate(time.Microsecond)
	if ask.Ceremony == db.CeremonyRegistration {
		s.register(w, r, ask, now)
		return
	}
	s.signIn(w, r, ask, now)
}

// challengeOf is the challenge the client data answers, which finds the ceremony it belongs to.
// Read here only to find it: internal/webauthn reads the client data whole, and holds the challenge
// to the one found. Client data that names none, or not in base64url, names a challenge that was
// never issued.
func challengeOf(clientData []byte) []byte {
	var cd struct {
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(bytes.TrimPrefix(clientData, []byte("\xef\xbb\xbf")), &cd); err != nil {
		return nil
	}
	c, err := b64.Strict().DecodeString(cd.Challenge)
	if err != nil || len(c) != challengeBytes {
		return nil
	}
	return c
}

// errSynced is a synced passkey the policy came to refuse while its registration was verified.
var errSynced = errors.New("api: the passkey is synced and device_bound_only applies")

// refusal is a ceremony refused for a reason the request could not have been answered otherwise
// for, rather than for a failure of the API's own.
type refusal struct {
	reason string

	// clone is an assertion refused for its signature counter.
	clone bool
}

func (r *refusal) Error() string { return r.reason }

// register finishes a registration, POST /api/v1/auth/passkey/verify with ceremony registration.
//
// The challenge is taken in a transaction of its own, so that it is spent whatever follows. The
// attestation is verified outside any transaction, since verifying costs a signature check and
// holds nothing. What the registration writes is then one transaction: the lifting of a suspension
// made for holding no passkey, where a code started it; the passkey; the password, where the passkey
// brings its account to min_passkeys under a policy taking it off passwords (retirePassword); the
// code it spent; the end of the bootstrap where an administrator enrols while it lives; and, where a
// code started it, the sign-in, recorded in the audit log last.
//
// One registered from a session needs the session signed in to within proofLife of the options, as
// the options did (sessions.go).
func (s *PasskeyAPI) register(w http.ResponseWriter, r *http.Request, ask ceremonyAnswered, now time.Time) {
	var took db.Challenge
	presented := b64.EncodeToString(ask.Credential.RawID)
	if len(presented) > presentedMax {
		presented = presented[:presentedMax]
	}
	// signedIn records a refusal as a failed sign-in where the registration would have signed
	// somebody in, one an enrolment code started, and where nothing says what started it, a
	// challenge that names nothing, which is anybody's to answer as an assertion's is. One a
	// session started signs nobody in: its user is signed in already, and is told in the answer.
	signedIn := func(reason string) {
		if took.EnrolmentCode == nil && took.Login != "" {
			return
		}
		f := codeFailure{code: took.EnrolmentCode, target: took.Login, reason: reason, detail: map[string]any{
			"credential_type": db.CredentialPasskey, "credential": presented,
		}}
		if f.code == nil {
			f.target = presented
		}
		s.refuseCode(r, f, now)
	}
	refused := func(reason string) {
		signedIn(reason)
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, noRegistration)
	}
	synced := func() {
		signedIn("the passkey is synced and device_bound_only applies to the account")
		failSetting(w, http.StatusForbidden, syncedRefused, deviceBoundOnly)
	}
	var policy accountPolicy
	unanswered := ""
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		var err error
		took, err = wide.TakeChallenge(ctx, challengeOf(ask.Credential.ClientDataJSON), now)
		switch {
		case errors.Is(err, db.ErrNoChallenge):
			took, unanswered = db.Challenge{}, "the challenge was never issued, was answered already or lapsed"
			return nil
		case err != nil:
			return err
		case took.Ceremony != db.CeremonyRegistration:
			took, unanswered = db.Challenge{}, "the challenge was issued for an assertion"
			return nil
		}
		policy, err = policyFor(ctx, wide, took.Login, now, false)
		return err
	})
	switch {
	case err != nil:
		fail(w, http.StatusInternalServerError, "the passkey could not be registered")
		return
	case took.Login == "":
		refused(unanswered)
		return
	}
	if took.EnrolmentCode == nil {
		// Started from a session, and finished from the same user's, which has to be live still.
		as, err := s.registrar(r)
		if err != nil {
			fail(w, http.StatusInternalServerError, "the request could not be authenticated")
			return
		}
		if string(as.Principal) != took.Login {
			refused("")
			return
		}
		// Proved within proofLife of the options, as the options asked, since the challenge
		// they issued is what this registration answers.
		if !provedSince(as.ProvedAt, took.IssuedAt) {
			askAgain(w)
			return
		}
	}
	made, err := webauthn.VerifyRegistration(
		webauthn.Ceremony{RPID: s.rpID, Origin: s.origin, Challenge: took.Value, RequireUserVerification: policy.userVerification},
		webauthn.Registration{ClientDataJSON: ask.Credential.ClientDataJSON, AttestationObject: ask.Credential.AttestationObject})
	switch {
	case err != nil:
		refused("the registration does not verify: " + err.Error())
		return
	case !bytes.Equal(made.ID, ask.Credential.RawID):
		refused("the credential ID is not the one the attestation names")
		return
	case policy.deviceBoundOnly && made.BackupEligible:
		synced()
		return
	}

	id := b64.EncodeToString(made.ID)
	address := s.signIns.addressOf(r)
	if s.checked != nil {
		s.checked()
	}
	var answer Verified
	var cookie *http.Cookie
	err = s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		// The user's row first, as issuing a link takes it, then the personal namespace, which
		// takes the lock every user and namespace created takes, then the bootstrap state, then
		// the code: the order issuing a first administrator's link takes the last two in, so that
		// neither waits on what the other holds.
		user, err := wide.HoldUser(ctx, took.Login)
		if errors.Is(err, db.ErrNoPrincipal) {
			return &refusal{reason: "the user was removed"}
		}
		if err != nil {
			return err
		}
		coded := took.EnrolmentCode != nil
		// A session was read before the user's row was held, and a suspension since, forbidding
		// passwords to an account holding no passkey, ended it: a suspended user enrols with a
		// code alone.
		if !coded && user.Suspended {
			return &refusal{reason: "the user was suspended"}
		}
		// The policy read again under the user's row, since a synced passkey refused only by a
		// policy read before it would be written beside a policy refusing it, and lift a
		// suspension with a passkey that signs nobody in.
		applies, err := policyFor(ctx, wide, user.Login, now, false)
		if err != nil {
			return err
		}
		if applies.deviceBoundOnly && made.BackupEligible {
			return errSynced
		}
		// A suspended user enrols with a code, since "enrolling is how an account suspended
		// for having no passkey comes back": a suspension made for having none is lifted by the
		// passkey, which the policy accepts, and the user is then signed in as any other; one
		// made for another reason is not the passkey's to lift, and its user opens no session
		// while it lasts. The code is spent below, and a code that opens nothing rolls the
		// lifting back with everything else.
		lifted := false
		if coded && user.Suspended && user.SuspendedFor == db.SuspendedNoPasskey {
			if lifted, err = wide.LiftSuspension(ctx, user.Login, db.SuspendedNoPasskey); err != nil {
				return err
			}
			if lifted {
				user.Suspended, user.SuspendedFor = false, ""
			}
		}
		signs := coded && !user.Suspended
		var personal []entry
		if signs {
			if personal, err = personalNamespace(ctx, wide, user.Login); err != nil {
				return err
			}
		}
		var code db.EnrolmentCode
		if coded {
			if code, err = wide.EnrolmentCodeByHash(ctx, took.EnrolmentCode, now); err != nil {
				return err
			}
			if code.Kind == db.EnrolmentFirstAdministrator {
				b, err := wide.HoldBootstrapToEnd(ctx)
				if err != nil {
					return err
				}
				if b.Ended() {
					return &refusal{reason: "the bootstrap ended"}
				}
			}
			if code, err = wide.UseEnrolmentCode(ctx, took.EnrolmentCode, now); err != nil {
				return err
			}
		}
		label := ""
		if ask.labelled {
			label = ask.Label
		}
		if err := wide.AddCredential(ctx, db.Credential{
			ID: id, Login: user.Login, Type: db.CredentialPasskey, Label: label,
			PublicKey: made.PublicKey, SignCount: made.SignCount, AAGUID: made.AAGUID[:],
			BackupEligible: made.BackupEligible, BackupState: made.BackupState,
		}); err != nil {
			return err
		}
		// Read back, since when it was enrolled is the database's to say.
		recorded, err := wide.Credential(ctx, id)
		if err != nil {
			return err
		}
		passkey := passkeyOf(recorded)
		answer = Verified{Ceremony: db.CeremonyRegistration, Login: user.Login, Credential: &passkey}
		enrolled := map[string]any{
			"kind": passkey.Kind, "label": label, "backup_eligible": made.BackupEligible,
			"backup_state": made.BackupState, "aaguid": fmt.Sprintf("%x", made.AAGUID),
		}
		if lifted {
			enrolled["suspension_lifted"] = db.SuspendedNoPasskey
		}
		entries := []entry{{record: audit.Record{
			Actor: user.Login, Action: audit.CredentialEnrol, Target: id, Result: audit.Done, Detail: enrolled,
		}}}
		off, err := retirePassword(ctx, wide, user.Login, now)
		if err != nil {
			return err
		}
		entries = append(entries, off...)
		if coded {
			entries = append(entries, entry{record: audit.Record{
				Actor: user.Login, Action: audit.EnrolmentUse, Target: user.Login, Result: audit.Done,
				Detail: map[string]any{"kind": code.Kind, "issued_by": code.IssuedBy, "credential": id},
			}})
		}
		// The bootstrap ends at the enrolment of the first administrator who can sign in, and not
		// before: ended at a suspended one's, it would leave the installation with nobody to
		// administer it, the lockout ending it at an enrolment rather than at a creation avoids.
		// Whatever brought them to it: the first administrator's link, a recovery code, or the
		// session a password they set from their code opened where the policy requires a
		// passkey, which may only enrol, so that nobody administers from it (passwords_set.go). Every
		// administrator enrolling while the token lives is one it created, and once it has
		// ended, ending it again ends nothing.
		if user.Admin && !user.Suspended {
			ended, err := wide.EndBootstrap(ctx, now)
			if err != nil {
				return err
			}
			if ended {
				// The bootstrap token's principal is what ends, recorded as the target.
				entries = append(entries, entry{record: audit.Record{
					Actor: user.Login, Action: audit.BootstrapEnd, Target: string(BootstrapOperator), Result: audit.Done,
					Detail: map[string]any{"first_administrator": user.Login, "credential": id},
				}})
			}
		}
		if signs {
			opened, signedIn, err := openSignedIn(ctx, wide, user.Login, id, address, now)
			if err != nil {
				return err
			}
			cookie, entries = opened, append(append(entries, personal...), signedIn)
		}
		return appendEntries(ctx, wide, entries)
	})
	var refusedFor *refusal
	switch {
	case errors.Is(err, errSynced):
		synced()
		return
	case errors.As(err, &refusedFor):
		// A user removed or suspended, or a bootstrap ended: nothing was written, and the
		// ceremony starts again.
		refused(refusedFor.reason)
		return
	case errors.Is(err, db.ErrNoEnrolmentCode):
		// A code spent, lapsed or replaced since the options were issued, which its row says.
		refused("")
		return
	case errors.Is(err, db.ErrCredentialExists):
		refused("the passkey is registered already")
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the passkey could not be registered")
		return
	}
	if cookie != nil {
		http.SetCookie(w, cookie)
	}
	shownOnce(w, http.StatusOK, answer)
}

// signIn finishes an assertion, POST /api/v1/auth/passkey/verify with ceremony assertion.
//
// The challenge is taken, and the passkey and its account's policy read, in a transaction of their
// own, so that the challenge is spent whatever follows; the assertion is verified outside any
// transaction; and the sign-in is then one transaction, which records the counter and the Backup
// State the passkey reported, the sign-in on the user's row, their personal namespace where it is
// their first, and the session. A refusal writes nothing of that, and is recorded in a transaction
// of its own.
func (s *PasskeyAPI) signIn(w http.ResponseWriter, r *http.Request, ask ceremonyAnswered, now time.Time) {
	id := b64.EncodeToString(ask.Credential.RawID)
	failed := signInFailure{address: s.signIns.addressOf(r), credential: id}
	var took db.Challenge
	var stored db.Credential
	var handle []byte
	var policy accountPolicy
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		var err error
		took, err = wide.TakeChallenge(ctx, challengeOf(ask.Credential.ClientDataJSON), now)
		switch {
		case errors.Is(err, db.ErrNoChallenge):
			failed.reason = "the challenge was never issued, was answered already or lapsed"
			return nil
		case err != nil:
			return err
		case took.Ceremony != db.CeremonyAssertion:
			failed.reason = "the challenge was issued for a registration"
			return nil
		}
		stored, err = wide.Credential(ctx, id)
		if errors.Is(err, db.ErrNoCredential) || (err == nil && stored.Type != db.CredentialPasskey) {
			failed.reason = "no passkey of that credential ID is registered"
			return nil
		}
		if err != nil {
			return err
		}
		failed.login = stored.Login
		if handle, err = wide.PasskeyHandleOf(ctx, stored.Login); err != nil {
			return err
		}
		policy, err = policyFor(ctx, wide, stored.Login, now, false)
		return err
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the sign-in could not be verified")
		return
	}
	if failed.reason != "" {
		s.refuseSignIn(w, r, failed, now)
		return
	}
	// §7.2 step 6: the account the user handle names holds the credential.
	if len(handle) == 0 || !bytes.Equal(handle, ask.Credential.UserHandle) {
		failed.reason = "the user handle is not the one the passkey was registered under"
		s.refuseSignIn(w, r, failed, now)
		return
	}
	raw, err := b64.DecodeString(stored.ID)
	if err != nil {
		fail(w, http.StatusInternalServerError, "the sign-in could not be verified")
		return
	}
	var aaguid [16]byte
	copy(aaguid[:], stored.AAGUID)
	updated, err := webauthn.VerifyAssertion(
		webauthn.Ceremony{RPID: s.rpID, Origin: s.origin, Challenge: took.Value, RequireUserVerification: policy.userVerification},
		webauthn.Credential{
			ID: raw, PublicKey: stored.PublicKey, SignCount: stored.SignCount, AAGUID: aaguid,
			BackupEligible: stored.BackupEligible, BackupState: stored.BackupState,
		},
		webauthn.Assertion{
			CredentialID: ask.Credential.RawID, ClientDataJSON: ask.Credential.ClientDataJSON,
			AuthenticatorData: ask.Credential.AuthenticatorData, Signature: ask.Credential.Signature,
		})
	if err != nil {
		// A counter that did not move forward is the one refusal of an assertion valid in every
		// other respect, its signature included.
		failed.reason, failed.clone = err.Error(), errors.Is(err, webauthn.ErrPossibleClone)
		failed.verified = failed.clone
		s.refuseSignIn(w, r, failed, now)
		return
	}
	if policy.deviceBoundOnly && stored.BackupEligible {
		failed.reason, failed.synced, failed.verified = "the passkey is synced and device_bound_only applies to the account", true, true
		s.refuseSignIn(w, r, failed, now)
		return
	}

	var cookie *http.Cookie
	var redirect string
	err = s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		// The user's row first, as every act on an account takes it, then the passkey's.
		user, err := wide.HoldUser(ctx, stored.Login)
		switch {
		case errors.Is(err, db.ErrNoPrincipal):
			return &refusal{reason: "the account was removed"}
		case err != nil:
			return err
		case user.Suspended:
			return &refusal{reason: "the account is suspended"}
		}
		err = wide.PasskeyUsed(ctx, id, updated.SignCount, updated.BackupState, now)
		switch {
		case errors.Is(err, db.ErrSignCountBehind):
			// Another assertion recorded a counter at or past this one's since it was read: two
			// copies of the key in use, or two assertions verified out of order, which look
			// the same.
			return &refusal{reason: fmt.Sprintf("%s: another sign-in recorded a counter at or past %d first", webauthn.ErrPossibleClone, updated.SignCount), clone: true}
		case errors.Is(err, db.ErrNoCredential):
			return &refusal{reason: "the passkey was removed"}
		case err != nil:
			return err
		}
		entries, err := personalNamespace(ctx, wide, user.Login)
		if err != nil {
			return err
		}
		opened, signedIn, err := openSignedIn(ctx, wide, user.Login, id, failed.address, now)
		if err != nil {
			return err
		}
		cookie = opened
		// A passkey's session is a full one, which agk login's sign-in hands a code beside.
		if ask.Terminal != nil {
			if redirect, err = handOff(ctx, wide, ask.Terminal, user.Login, id, now); err != nil {
				return err
			}
		}
		return appendEntries(ctx, wide, append(entries, signedIn))
	})
	var refusedFor *refusal
	switch {
	case errors.As(err, &refusedFor):
		failed.reason, failed.clone, failed.verified = refusedFor.reason, refusedFor.clone, true
		s.refuseSignIn(w, r, failed, now)
		return
	case errors.Is(err, db.ErrSessionRefused):
		failed.reason, failed.verified = "the account opens no session", true
		s.refuseSignIn(w, r, failed, now)
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the sign-in could not be completed")
		return
	}
	http.SetCookie(w, cookie)
	shownOnce(w, http.StatusOK, Verified{Ceremony: db.CeremonyAssertion, Login: stored.Login, RedirectTo: redirect})
}

// entry is one entry of the audit log an act appends, in the namespace it names, or on the
// installation where it names none.
type entry struct {
	namespace string
	record    audit.Record
}

// appendEntries appends an act's entries in order, as the last statements of its transaction.
func appendEntries(ctx context.Context, wide *db.Wide, entries []entry) error {
	for _, e := range entries {
		var err error
		if e.namespace != "" {
			err = wide.AuditIn(ctx, e.namespace, e.record)
		} else {
			err = wide.Audit(ctx, e.record)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// personalNamespace creates login's personal namespace where it does not exist yet: "every user
// owns a personal namespace named after their login, created on first sign-in", with its built-in
// identity as every namespace has one, and its user given the owner role on it, as an owner is
// given it on a namespace an administrator creates, so that they can act in it and share it. The
// installation creates it, and is recorded as having done so: nobody asked for it.
//
// It answers the entries that record it, for its caller to append last. A namespace another sign-in
// of the same user created a moment ago, which the insert waits for and then finds, is theirs
// already, with its grant, and answers none.
func personalNamespace(ctx context.Context, wide *db.Wide, login string) ([]entry, error) {
	_, err := wide.NamespaceNamed(ctx, login)
	if err == nil || !errors.Is(err, db.ErrNoNamespace) {
		return nil, err
	}
	made, err := wide.CreateNamespace(ctx, db.Namespace{Name: login, Kind: db.NamespacePersonal, Owner: login})
	if err != nil || !made {
		return nil, err
	}
	grant := access.Grant{
		ID: ulid.New(), Principal: login, Scope: access.Scope{Namespace: login},
		Role: access.Owner, GrantedBy: installationActor,
	}
	if err := wide.GrantAccess(ctx, grant); err != nil {
		return nil, err
	}
	created, err := wide.NamespaceNamed(ctx, login)
	if err != nil {
		return nil, err
	}
	return []entry{
		{namespace: login, record: audit.Record{
			Actor: installationActor, Action: audit.GrantCreate, Target: grant.ID, Result: audit.Done,
			Detail: map[string]any{"principal": grant.Principal, "scope": grant.Scope.String(), "role": string(grant.Role)},
		}},
		{record: audit.Record{
			Actor: installationActor, Action: audit.NamespaceCreate, Target: login, Result: audit.Done,
			Detail: map[string]any{"namespace": recordOf(created), "owner_grant": grant.ID, "first_sign_in": login},
		}},
	}, nil
}

// openSignedIn signs login in with the passkey credential: the sign-in recorded on their row, and a
// full session opened by the passkey. It answers the cookie to set once the transaction commits,
// and the entry recording the sign-in.
func openSignedIn(ctx context.Context, wide *db.Wide, login, credential, address string, now time.Time) (*http.Cookie, entry, error) {
	if err := wide.SignedIn(ctx, login, now); err != nil {
		return nil, entry{}, err
	}
	cookie, err := OpenSession(ctx, wide, login, OpenedBy{Credential: credential}, now)
	if err != nil {
		return nil, entry{}, err
	}
	return cookie, entry{record: audit.Record{
		Actor: login, Action: audit.SigninSucceed, Target: login, Result: audit.Done,
		Detail: map[string]any{"credential": credential, "address": address},
	}}, nil
}

// signInFailure is an assertion refused, and what its entry in the audit log records.
type signInFailure struct {
	reason string

	// address is where the request came from, credential the credential ID it presented, and
	// login the account that credential names, empty where it names none.
	address, credential, login string

	// verified is a refusal of an assertion whose signature verified, which only whoever holds
	// the passkey's private key can make: clone and synced are two of them.
	verified, clone, synced bool
}

// presentedMax is the most of a credential ID naming no passkey an entry keeps, in characters: 48
// bytes, more than the IDs authenticators mint, which a person searching the log recognises by.
const presentedMax = 64

// failureReasonMax is the most of a refusal's reason the audit log keeps, in bytes: a reason quotes what
// the client data said, the origin a page named for one, and an entry recording one is written for
// anybody who asks.
const failureReasonMax = 256

// refuseSignIn answers an assertion refused, and records it: signin.fail in the audit log, in a
// transaction of its own, since the sign-in it records wrote nothing, and, for a counter that did
// not move forward, a passkey_counter_refused notification for the passkey's user, who is the one
// who knows whether the other copy is theirs. The passkey is not locked: a lock would let a faulty
// authenticator shut its owner out.
//
// The entries refusals before a signature verified append are bounded, as failedSignIns says: past
// the bound, a refusal is answered all the same and counted, and the next entry says how many went
// unrecorded. A credential ID naming no passkey is recorded cut to presentedMax characters, since
// it is whatever the sender wrote, up to a kibibyte of it.
func (s *PasskeyAPI) refuseSignIn(w http.ResponseWriter, r *http.Request, f signInFailure, now time.Time) {
	unrecorded, recorded := 0, true
	if f.verified {
		unrecorded = s.signIns.failures.recordedAnyway()
	} else {
		unrecorded, recorded = s.signIns.failures.admit(f.address, now)
	}
	if f.login == "" && len(f.credential) > presentedMax {
		f.credential = f.credential[:presentedMax]
	}
	if recorded {
		reason := f.reason
		for len(reason) > failureReasonMax {
			_, size := utf8.DecodeLastRuneInString(reason)
			reason = reason[:len(reason)-size]
		}
		target := f.login
		if target == "" {
			target = f.credential
		}
		detail := map[string]any{"reason": reason, "credential": f.credential, "address": f.address}
		if unrecorded > 0 {
			detail["unrecorded"] = unrecorded
		}
		err := s.pool.Installation(context.WithoutCancel(r.Context()), db.Identity, func(ctx context.Context, wide *db.Wide) error {
			if f.clone && f.login != "" {
				if err := wide.TellPasskeyRefused(ctx, f.login, f.credential, now); err != nil {
					return err
				}
			}
			return wide.Audit(ctx, audit.Record{
				Actor: f.address, Action: audit.SigninFail, Target: target, Result: audit.Done, Detail: detail,
			})
		})
		if err != nil && s.trouble != nil {
			s.trouble(fmt.Errorf("a failed sign-in could not be recorded: %w", err))
		}
	}
	if f.synced {
		failSetting(w, http.StatusForbidden, syncedRefused, deviceBoundOnly)
		return
	}
	w.Header().Set("WWW-Authenticate", "Bearer")
	fail(w, http.StatusUnauthorized, noSignIn)
}

// refuseCode records a sign-in an enrolment code started, or a registration answering a challenge
// that names nothing, refused (SignIns.refuseCode), telling Trouble where it could not be recorded:
// the refusal is answered all the same.
func (s *PasskeyAPI) refuseCode(r *http.Request, f codeFailure, now time.Time) {
	if err := s.signIns.refuseCode(r.Context(), s.pool, s.signIns.addressOf(r), f, now); err != nil && s.trouble != nil {
		s.trouble(fmt.Errorf("a failed sign-in could not be recorded: %w", err))
	}
}

// failSetting answers a refusal whose reason is one setting of the authentication policy, which
// openapi.json's error names beside the sentence.
func failSetting(w http.ResponseWriter, status int, message, setting string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, "{\"error\":%q,\"setting\":%q}\n", message, setting)
}
