package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/token"
	"github.com/agentiik/agentiik/internal/ulid"
)

// agk login's hand-off, from the sign-in page back to a terminal, and POST /api/v1/auth/exchange.
//
// "agk login listens on a port of the loopback, makes a one-time verifier and opens the browser at
// the sign-in page, handing it the loopback address and the verifier's SHA-256." The page passes the
// two on, as terminal, with the sign-in it completes, a passkey's assertion or a password; the route
// that signs the browser in then mints a one-time code in the transaction that opens the session,
// and answers redirect_to, the loopback address with the code as its code query parameter, which
// the page follows. agk trades the code and the verifier here for an API token, PKCE's exchange
// (RFC 7636): "a code seen on the way, in the browser's history or by another process on the
// machine, opens nothing without the verifier, which never left agk".
//
// A code is agkcode_ and 256 bits, kept as its SHA-256, single use and good for a minute, and bound
// to the challenge it was minted against and to who signed in with which credential. The exchange
// takes it once the request holds to its schema and before anything else, so that a code is spent
// by the first presentation the schema accepts, whatever verifier it carries, and a code, a verifier
// or a principal that does not hold is one 401 with one sentence, as a token that opens nothing is.
//
// What the exchange mints is decided when it is asked, as what a session may do is decided at each
// request: a code a password minted where the policy has since come to require a passkey the
// account does not hold is a 403, as is one where passwords have since been forbidden, and a code
// of a user suspended or removed since opens nothing, as does one of a credential removed, or of a
// password set anew, since. A sign-in whose session may only enrol mints no code at all: "enrols
// passkeys, nothing else: cannot read a workflow, start a run or mint a token". The token is minted
// as POST /api/v1/auth/tokens mints one for its caller, for 90 days, within the same bound on live
// tokens, and audited as api_token.create by the user who signed in.
//
// An exchange refused once its request holds to the schema is a sign-in that failed, and is recorded
// as signin.fail with why, as the sign-in routes record theirs and within the bound they share on
// what failures append (failures.go): by the address it came from, about the account the code was
// minted for, or the code's SHA-256, as it is kept, where it names none, so that no code is written
// in the log. A refusal before the verifier answered the code is anybody's to send, and is held to
// the bound; one after is only whoever signed in and holds the verifier's to make, and is recorded
// whatever the bound says, as an assertion refused after its signature verified is.

// exchangeCode is openapi.json's exchangeCode: agkcode_ and 256 bits of base64url.
var exchangeCode = regexp.MustCompile(`^agkcode_[A-Za-z0-9_-]{43,}$`)

// codeVerifier is openapi.json's codeVerifier: RFC 7636's 43 to 128 unreserved characters.
var codeVerifier = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

// The refusals of the exchange that say something of their own.
const (
	// noExchange is every code that opens nothing, one sentence for every reason, as the
	// OpenAPI document asks: used already, past its minute, never minted, presented with a
	// verifier that is not the one it was minted against, or of an account that opens nothing.
	noExchange = "that code opens nothing: it was used already, is older than a minute, was never issued, or does not answer this verifier. Run agk login again"

	// enrolsOnlyMintsNothing is a code a password minted where the policy has since come to
	// require a passkey.
	enrolsOnlyMintsNothing = "this sign-in was made with a password where the policy requires a passkey, and enrols passkeys and nothing else: it mints no token. Enrol a passkey on the sign-in page, then run agk login again"
)

// The reasons an exchange refused is recorded with, in signin.fail's detail.
const (
	noLiveCode          = "no code of that value is live: used already, past its minute, or never issued"
	verifierMismatch    = "the verifier does not answer the challenge the code was minted against"
	codeAccountRemoved  = "the account was removed since the sign-in"
	codeAccountSuspends = "the account was suspended since the sign-in"
	codeCredentialGone  = "the credential that signed in was removed or replaced since"
	codeForbidden       = "passwords are forbidden by the policy that applies to the account"
	codeEnrolsOnly      = "the sign-in was a password's where the policy requires a passkey, and may only enrol"
	codeSynced          = "the passkey that signed in is synced, and device_bound_only applies"
	codeTokensMost      = "the account holds the most live tokens one principal may hold"
)

// exchangeCredential is what signin.fail's detail says an exchange presented, where a password's
// says password: agk login's one-time code, as openapi.json's exchangeCode names it.
const exchangeCredential = "exchange_code"

// field reads terminal's members, openapi.json's terminalSignIn.
func (t *TerminalSignIn) field(b *body, name string) error {
	switch name {
	case "redirect_uri":
		return text(b, &t.RedirectURI)
	case "code_challenge":
		return text(b, &t.CodeChallenge)
	}
	return unknown(name)
}

// readTerminal reads a sign-in's terminal, null being none.
func readTerminal(b *body, into **TerminalSignIn) error {
	if absent, err := null(b); absent || err != nil {
		return err
	}
	t := &TerminalSignIn{}
	if err := b.fields(t); err != nil {
		return err
	}
	*into = t
	return nil
}

// check refuses what the schema refuses, on the grammar the sign-in page held the two to before it
// was served: "the two travel together: a loopback address without a challenge would be a code
// anybody on the machine could redeem", and a loopback address is the one place a code is handed.
func (t TerminalSignIn) check() error {
	switch {
	case t.RedirectURI == "" || t.CodeChallenge == "":
		return errors.New("terminal: agk login's sign-in carries both redirect_uri and code_challenge, as the sign-in page was opened with them")
	case !loopbackURI.MatchString(t.RedirectURI):
		// Not repeated, since it is whatever the page's address carried.
		return errors.New("terminal: redirect_uri is where agk login listens, http on a port of 127.0.0.1 or [::1], and this one names another place, where no code is handed")
	case !codeChallenge.MatchString(t.CodeChallenge):
		return errors.New("terminal: code_challenge is the SHA-256 of agk login's verifier, 43 base64url characters, and this one is not")
	}
	return nil
}

// handOff mints agk login's code for login, who has just signed in with credential, in the
// transaction wide is, the one that opens their session, and answers where the sign-in page sends
// the browser: the loopback address agk gave, with the code as its code query parameter. The API
// writes the whole address rather than the page assembling it, "so that the page can never be made
// to send a code anywhere the API did not check".
func handOff(ctx context.Context, wide *db.Wide, t *TerminalSignIn, login, credential string, now time.Time) (string, error) {
	code, _, err := token.New(token.Code, "")
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(code))
	if err := wide.IssueExchangeCode(ctx, db.ExchangeCode{
		Hash: hash[:], Login: login, Credential: credential, CodeChallenge: t.CodeChallenge,
		IssuedAt: now, ExpiresAt: now.Add(db.ExchangeCodeLife),
	}); err != nil {
		return "", err
	}
	return t.RedirectURI + "?code=" + code, nil
}

// ExchangeOptions are what the exchange is given.
type ExchangeOptions struct {
	Pool *db.Pool

	// PublicURL is AGK_PUBLIC_URL, or AGK_PROXY_URL behind a proxy, whose host, where it is an IP
	// address, is an installation where the policy is applied with passwords allowed and no
	// passkey required, as the sign-in that minted the code applied it.
	PublicURL string

	// SignIns is what the sign-in routes share: where a request comes from, and the bound on the
	// entries failures append to the audit log, which a refused exchange is held to as a refused
	// sign-in is. One of its own where it is nil.
	SignIns *SignIns

	// Now is the clock codes lapse and tokens are minted by, the wall clock where it is nil.
	Now func() time.Time

	// Trouble is told what could not be done beside the answer, a refusal that could not be
	// recorded, and nothing where it is nil.
	Trouble func(error)
}

// ExchangeAPI is POST /api/v1/auth/exchange.
type ExchangeAPI struct {
	pool        *db.Pool
	now         func() time.Time
	ipAddressed bool
	signIns     *SignIns
	trouble     func(error)
}

// NewExchange registers the exchange on a router.
func NewExchange(rt *Router, o ExchangeOptions) (*ExchangeAPI, error) {
	switch {
	case rt == nil:
		return nil, errors.New("api: no router")
	case o.Pool == nil:
		return nil, errors.New("api: no database, and the codes and the tokens are kept there")
	}
	if _, err := originOf(o.PublicURL); err != nil {
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
	s := &ExchangeAPI{pool: o.Pool, now: o.Now, ipAddressed: net.ParseIP(u.Hostname()) != nil, signIns: o.SignIns, trouble: o.Trouble}
	public := Public{Why: "agk login trades its one-time code for an API token here, before it holds any credential: the request is authenticated by the code and the verifier it carries, which nobody but agk holds together"}
	if err := rt.Handle("POST", "/api/v1/auth/exchange", public, s.exchange); err != nil {
		return nil, err
	}
	return s, nil
}

// exchangeAsked is openapi.json's exchangeRequest.
type exchangeAsked struct {
	Code        string
	Verifier    string
	DeviceLabel string
}

func (q *exchangeAsked) field(b *body, name string) error {
	switch name {
	case "code":
		return text(b, &q.Code)
	case "code_verifier":
		return text(b, &q.Verifier)
	case "device_label":
		return deviceLabel(b, &q.DeviceLabel)
	}
	return unknown(name)
}

// check refuses what the schema refuses. Neither value is repeated, since each may be most of a
// real one.
func (q exchangeAsked) check() error {
	switch {
	case !exchangeCode.MatchString(q.Code):
		return errors.New("code: agk login's code is agkcode_ and at least 43 base64url characters, as the sign-in page handed it to the loopback address")
	case !codeVerifier.MatchString(q.Verifier):
		return errors.New("code_verifier: a verifier is 43 to 128 unreserved characters, as RFC 7636 writes one")
	}
	return nil
}

// exchangeOpensNothing is a code whose account opens nothing since the sign-in that minted it, and
// why: removed, suspended, or the credential that signed in gone.
type exchangeOpensNothing struct{ reason string }

func (e *exchangeOpensNothing) Error() string {
	return "api: the code's account opens nothing: " + e.reason
}

// errEnrolsOnly is a code a password minted where the policy now requires a passkey.
var errEnrolsOnly = errors.New("api: the code's sign-in may only enrol")

// exchange is POST /api/v1/auth/exchange.
//
// The code is taken in a transaction of its own, so that it is spent whatever follows, then held to
// the verifier outside any. What the exchange writes is one transaction: the account read again
// under its user's row, which every act on an account takes first, the policy that applies to it
// now where a password signed in, and the token.
func (s *ExchangeAPI) exchange(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	var ask exchangeAsked
	if err := readAtMost(r, &ask, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	if err := ask.check(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	opensNothing := func() {
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, noExchange)
	}

	hash := sha256.Sum256([]byte(ask.Code))
	failed := exchangeFailure{address: s.signIns.addressOf(r), target: hex.EncodeToString(hash[:])}
	var code db.ExchangeCode
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		var err error
		code, err = wide.TakeExchangeCode(ctx, hash[:], now)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoExchangeCode):
		failed.reason = noLiveCode
		s.refuse(r, failed, now)
		opensNothing()
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the code could not be read")
		return
	}
	failed.target, failed.credential = code.Login, code.Credential
	// RFC 7636 §4.6: the verifier's SHA-256, base64url with no padding, is the challenge. Compared
	// in constant time, as every credential is.
	answered := sha256.Sum256([]byte(ask.Verifier))
	if subtle.ConstantTimeCompare([]byte(b64.EncodeToString(answered[:])), []byte(code.CodeChallenge)) != 1 {
		failed.reason = verifierMismatch
		s.refuse(r, failed, now)
		opensNothing()
		return
	}
	failed.verified = true

	clear, _, err := token.New(token.API, "")
	if err != nil {
		fail(w, http.StatusInternalServerError, "a token could not be minted")
		return
	}
	minted := sha256.Sum256([]byte(clear))
	row := db.APIToken{
		ID: ulid.New(), Hash: minted[:], Principal: code.Login, DeviceLabel: ask.DeviceLabel,
		CreatedAt: now, ExpiresAt: now.AddDate(0, 0, tokenDefaultDays),
	}
	err = s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		user, err := wide.HoldUser(ctx, code.Login)
		switch {
		case errors.Is(err, db.ErrNoPrincipal):
			return &exchangeOpensNothing{reason: codeAccountRemoved}
		case err != nil:
			return err
		case user.Suspended:
			return &exchangeOpensNothing{reason: codeAccountSuspends}
		}
		held, err := wide.CredentialsOf(ctx, code.Login)
		if err != nil {
			return err
		}
		var opened *db.Credential
		for i := range held {
			if held[i].ID == code.Credential {
				opened = &held[i]
			}
		}
		if opened == nil {
			// Removed since the sign-in, which removes the code with it, and removed between
			// the code taken and this.
			return &exchangeOpensNothing{reason: codeCredentialGone}
		}
		// What the session the sign-in opened may do now, as Principals reads it at each request.
		if opened.Type == db.CredentialPassword || (opened.Type == db.CredentialPasskey && opened.BackupEligible) {
			policy, err := policyFor(ctx, wide, code.Login, now, s.ipAddressed)
			switch {
			case err != nil:
				return err
			case opened.Type == db.CredentialPasskey && policy.deviceBoundOnly:
				return errSynced
			case opened.Type == db.CredentialPasskey:
			case policy.passwordsForbidden:
				return errForbidden
			case policy.enrolling():
				return errEnrolsOnly
			}
		}
		return mintToken(ctx, wide, row, code.Login, map[string]any{"credential": code.Credential})
	})
	var gone *exchangeOpensNothing
	switch {
	case errors.As(err, &gone):
		failed.reason = gone.reason
	case errors.Is(err, errForbidden):
		failed.reason = codeForbidden
	case errors.Is(err, errEnrolsOnly):
		failed.reason = codeEnrolsOnly
	case errors.Is(err, errSynced):
		failed.reason = codeSynced
	case errors.Is(err, errTokensMost):
		failed.reason = codeTokensMost
	}
	if failed.reason != "" {
		s.refuse(r, failed, now)
	}
	switch {
	case gone != nil:
		opensNothing()
		return
	case errors.Is(err, errForbidden):
		failSetting(w, http.StatusForbidden, passwordsForbidden, passwordSetting)
		return
	case errors.Is(err, errEnrolsOnly):
		fail(w, http.StatusForbidden, enrolsOnlyMintsNothing)
		return
	case errors.Is(err, errSynced):
		failSetting(w, http.StatusForbidden, syncedRefused, deviceBoundOnly)
		return
	case errors.Is(err, errTokensMost):
		fail(w, http.StatusConflict, fmt.Sprintf("%s holds %d live tokens, the most one principal may hold: revoke one no longer used with agk token revoke, from wherever one is kept, and run agk login again", code.Login, tokensMost))
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the token could not be minted")
		return
	}
	// It exists in this answer and nowhere else.
	shownOnce(w, http.StatusCreated, IssuedToken{Token: clear, APIToken: listedToken(row)})
}

// exchangeFailure is an exchange refused, and what its entry in the audit log records.
type exchangeFailure struct {
	reason string

	// address is where the request came from; target the account the code was minted for, or the
	// code's SHA-256 in hexadecimal where it names none; credential the credential that signed in,
	// where the code names one.
	address, target, credential string

	// verified is a refusal after the verifier answered the code, which only whoever signed in and
	// holds the verifier can make.
	verified bool
}

// refuse records an exchange refused, as signin.fail in a transaction of its own, since the exchange
// it records wrote nothing, within the bound the sign-in routes share (failures.go), a refusal after
// the verifier answered the code recorded whatever the bound says.
func (s *ExchangeAPI) refuse(r *http.Request, f exchangeFailure, now time.Time) {
	unrecorded, recorded := 0, true
	if f.verified {
		unrecorded = s.signIns.failures.recordedAnyway()
	} else {
		unrecorded, recorded = s.signIns.failures.admit(f.address, now)
	}
	if !recorded {
		return
	}
	detail := map[string]any{"reason": f.reason, "address": f.address, "credential_type": exchangeCredential}
	if f.credential != "" {
		detail["credential"] = f.credential
	}
	if unrecorded > 0 {
		detail["unrecorded"] = unrecorded
	}
	err := s.pool.Installation(context.WithoutCancel(r.Context()), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		return wide.Audit(ctx, audit.Record{
			Actor: f.address, Action: audit.SigninFail, Target: f.target, Result: audit.Done, Detail: detail,
		})
	})
	if err != nil && s.trouble != nil {
		s.trouble(fmt.Errorf("a refused exchange could not be recorded: %w", err))
	}
}
