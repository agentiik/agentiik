package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/password"
	"github.com/agentiik/agentiik/internal/totp"
)

// The password fallback, POST /api/v1/auth/login: "Password sign-in where the policy allows it.
// Refused outright when passwords are forbidden, not failed as a wrong password."
//
// # What is checked, in order
//
// The request comes from the sign-in page, as a passkey ceremony does: its Origin is the public
// URL's, since a form posted from another page would sign a browser in as whoever that page's author
// chose. Then the count of attempts, before anything is read (passwords_limits.go). Then the policy
// that applies to the account, which forbidding passwords answers 403 naming the setting, "before
// any password is compared", so that a client knows to stop offering the form. Then the password,
// against its Argon2id hash, for a login no user holds as for any other, and the TOTP code where the
// account holds a generator. Every refusal past the policy is the same 401 with the same sentence,
// "so that no login can be learnt by asking", and recorded as signin.fail with its reason, within
// the bound on what failed sign-ins append to the audit log that the passkey ceremonies share.
//
// A login no user holds is under the installation's policy alone, since it holds no grant: where a
// namespace forbids passwords and the installation does not, the 403 tells an account holding a
// grant in it from one nobody holds, which the OpenAPI document accepts in saying the refusal names
// the setting.
//
// # The session
//
// A password opens a session, recorded as opened by the password, which is full where the account's
// policy is satisfied and enrols passkeys and nothing else where the policy requires a passkey the
// account does not hold yet: "enrols passkeys, nothing else: cannot read a workflow, start a run or
// mint a token". Which of the two is not written anywhere: it is derived at every request from the
// credential that opened the session and the policy that applies then (Principals.identifySession),
// so that a policy changed, or a first passkey enrolled from the session, applies from the next
// request. The answer says which it was when the session opened.
//
// A registration from such a session is the one thing it may do, and the passkey ceremonies read it
// themselves for that; it opens no other session, and the one it was made from is full from the next
// request. A sign-in is a sign-in, so the first one gives the user their personal namespace whatever
// the session may do.
//
// # What the page does not name
//
// No route sets or changes a password, and none enrols a TOTP generator: the page names neither, so
// none is served. A password is a row of credentials of type password holding its hash as
// password.Hash writes it, and a generator one of type totp holding its secret sealed as the secret
// store seals one (TOTPSecrets).

// TOTPSecrets opens the secret of a TOTP generator, sealed under the master key as the credentials
// row holds it: "a code is checked against the secret itself, so it cannot be hashed, and a dump
// alone opens nothing". The secret store fills it, as it fills Secrets, since package api may not
// link the store.
type TOTPSecrets interface {
	OpenTOTP(login, id string, sealed []byte) ([]byte, error)
}

// PasswordOptions are what the password sign-in is given.
type PasswordOptions struct {
	Pool *db.Pool

	// PublicURL is AGK_PUBLIC_URL, or AGK_PROXY_URL behind a proxy: its origin is the sign-in
	// page's, which a sign-in is accepted from, and a host that is an IP address is an installation
	// where the policy is applied with passwords allowed and no passkey.
	PublicURL string

	// TOTP opens the secrets of TOTP generators. Nil opens none, and an account holding one is then
	// refused its sign-in, with a 500, rather than signed in with its password alone.
	TOTP TOTPSecrets

	// SignIns is what the sign-in routes share: where a sign-in comes from, and the bound on the
	// failures they record. Nil is one of this route's own, reading no proxy's header.
	SignIns *SignIns

	// Now is the clock attempts, codes and sessions are counted by, the wall clock where it is nil.
	Now func() time.Time

	// Trouble is told what went wrong where an answer is given all the same: a failed sign-in that
	// could not be recorded, or a hash or a secret that could not be read.
	Trouble func(error)
}

// PasswordAPI is the password sign-in.
type PasswordAPI struct {
	pool    *db.Pool
	totp    TOTPSecrets
	signIns *SignIns
	now     func() time.Time
	trouble func(error)

	// origin is the sign-in page's, and ipAddressed an installation addressed by an IP address.
	origin      string
	ipAddressed bool

	attempts *attempts
	hashing  *hashing

	// nobody is the hash of a password nobody knows, verified where the login names no password,
	// so that such a sign-in takes as long as any other.
	nobody string
}

// The kinds of session a password opens, as openapi.json's sessionKind writes them.
const (
	SessionFull      = "full"
	SessionEnrolment = "enrolment"
)

// The sentences a password sign-in is refused with.
const (
	// loginOrigin is a sign-in not from the sign-in page.
	loginOrigin = "a password sign-in comes from the sign-in page of this installation's public URL, and this request's Origin header names another or none"

	// passwordsForbidden is a sign-in where the policy that applies to the account forbids
	// passwords, answered naming the setting.
	passwordsForbidden = "passwords are forbidden by the authentication policy that applies to this account, and none is compared: sign in with a passkey"

	// noPasswordSignIn is every other refusal, one sentence for every reason.
	noPasswordSignIn = "that sign-in opens nothing: the login, the password or the TOTP code does not match, or the account opens no session. Try again, or sign in with a passkey"

	// tooManyAttempts is a sign-in past the count of attempts.
	tooManyAttempts = "too many password sign-ins were tried for this account or from this address: try again once the seconds Retry-After gives have passed, or sign in with a passkey"

	// hashingBusy is a sign-in that waited too long for its turn to hash.
	hashingBusy = "this installation is checking too many passwords at once to check this one in time: try again in a moment"
)

// passwordSetting is the setting passwordsForbidden names.
const passwordSetting = "password"

// NewPasswords registers the password sign-in on a router.
func NewPasswords(rt *Router, o PasswordOptions) (*PasswordAPI, error) {
	switch {
	case rt == nil:
		return nil, errors.New("api: no router")
	case o.Pool == nil:
		return nil, errors.New("api: no database, and the password and the policy are kept there")
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
	unknowable := make([]byte, 32)
	if _, err := rand.Read(unknowable); err != nil {
		return nil, fmt.Errorf("api: the password nobody knows could not be drawn: %w", err)
	}
	nobody, err := password.Hash(base64.RawURLEncoding.EncodeToString(unknowable))
	if err != nil {
		return nil, err
	}
	s := &PasswordAPI{
		pool: o.Pool, totp: o.TOTP, signIns: o.SignIns, now: o.Now, trouble: o.Trouble,
		origin: origin, ipAddressed: net.ParseIP(u.Hostname()) != nil,
		attempts: newAttempts(), hashing: newHashing(hashingTurns(), hashingWait), nobody: nobody,
	}
	public := Public{Why: "a password sign-in is how somebody proves who they are, with the password it carries, and opens the session every other route is then authorised by"}
	if err := rt.Handle("POST", "/api/v1/auth/login", public, s.login); err != nil {
		return nil, err
	}
	return s, nil
}

// passwordAsked is openapi.json's passwordLoginRequest. terminal, agk login's, is not read yet, and
// refused as any member a route does not read is.
type passwordAsked struct {
	Login    string
	Password string
	TOTP     string

	// coded is whether the request wrote a TOTP code, null being none.
	coded bool
}

func (q *passwordAsked) field(b *body, name string) error {
	switch name {
	case "login":
		return text(b, &q.Login)
	case "password":
		return text(b, &q.Password)
	case "totp":
		q.coded = b.d.PeekKind() != jsontext.KindNull
		return text(b, &q.TOTP)
	}
	return unknown(name)
}

// check refuses what the schema refuses: a login no user can hold, a service account's among them,
// no password, and a code that is not six digits.
func (q passwordAsked) check() error {
	if err := LoginName(q.Login); err != nil {
		return err
	}
	switch {
	case q.Password == "":
		return errors.New("password: a password sign-in carries the password, at least one character")
	case q.coded && !sixDigits(q.TOTP):
		// The code is not repeated, since it may be most of a real one.
		return fmt.Errorf("totp: a TOTP code is %d digits, as the generator shows it", totp.Digits)
	}
	return nil
}

func sixDigits(code string) bool {
	if len(code) != totp.Digits {
		return false
	}
	for _, c := range []byte(code) {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// SignedIn is openapi.json's signIn: who signed in with a password, and what the session opened may
// do when it opened.
type SignedIn struct {
	Login   string `json:"login"`
	Session string `json:"session"`
}

// passwordPolicy is what the policy that applies to one account says of passwords: whether they are
// forbidden, and whether a passkey is required, each the stricter of the installation's and that of
// every namespace the account holds a grant in.
type passwordPolicy struct {
	forbidden       bool
	passkeyRequired bool
}

// enrolling says whether a session a password opened for an account holding passkeys passkeys may
// only enrol: where a passkey is required and it holds none.
func (p passwordPolicy) enrolling(passkeys int) bool { return p.passkeyRequired && passkeys == 0 }

// passwordPolicyOf is the policy that applies to login's password. On an installation addressed by
// an IP address, where a browser runs no passkey ceremony, it is passwords allowed and no passkey
// required, whatever the stored policy says, as the page says the API applies it there.
func passwordPolicyOf(ctx context.Context, wide *db.Wide, login string, now time.Time, ipAddressed bool) (passwordPolicy, error) {
	if ipAddressed {
		return passwordPolicy{}, nil
	}
	installation, tightening, err := policiesOf(ctx, wide, login, now)
	if err != nil {
		return passwordPolicy{}, err
	}
	p := passwordPolicy{forbidden: installation.Password == "forbidden", passkeyRequired: installation.Passkey == "required"}
	for _, tightened := range tightening {
		p.forbidden = p.forbidden || tightened.Password == "forbidden"
		p.passkeyRequired = p.passkeyRequired || tightened.Passkey == "required"
	}
	return p, nil
}

// passkeysIn counts the passkeys among a user's credentials.
func passkeysIn(held []db.Credential) int {
	n := 0
	for _, c := range held {
		if c.Type == db.CredentialPasskey {
			n++
		}
	}
	return n
}

// account is what a password sign-in reads of the account a login names: whether a user holds it,
// their password and their TOTP generator where they hold one, how many passkeys they hold, and the
// policy that applies to them.
type account struct {
	user     db.User
	exists   bool
	password db.Credential
	totp     db.Credential
	passkeys int
	policy   passwordPolicy
}

// readAccount reads it, the user as user reads them, first: db.Wide.User, or HoldUser to hold their
// row, which every act on an account takes before what it holds.
func (s *PasswordAPI) readAccount(ctx context.Context, wide *db.Wide, login string, now time.Time,
	user func(context.Context, string) (db.User, error)) (account, error) {
	var a account
	var err error
	a.user, err = user(ctx, login)
	switch {
	case errors.Is(err, db.ErrNoPrincipal):
	case err != nil:
		return account{}, err
	default:
		a.exists = true
	}
	if a.policy, err = passwordPolicyOf(ctx, wide, login, now, s.ipAddressed); err != nil || !a.exists {
		return a, err
	}
	held, err := wide.CredentialsOf(ctx, login)
	if err != nil {
		return account{}, err
	}
	for _, c := range held {
		switch c.Type {
		case db.CredentialPassword:
			a.password = c
		case db.CredentialTOTP:
			a.totp = c
		}
	}
	a.passkeys = passkeysIn(held)
	return a, nil
}

// fromThePage says whether a request comes from the sign-in page, as its one Origin header says.
func (s *PasswordAPI) fromThePage(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	return len(origins) == 1 && origins[0] == s.origin
}

// login is POST /api/v1/auth/login.
//
// What the account is and the policy that applies to it are read in a transaction of their own; the
// password is hashed outside any, since a hash holds 19 MiB for tens of milliseconds and may wait its
// turn; and the sign-in is then one transaction, which reads the account again under its user's row
// and signs in only where what was checked is what is there: the same password, the same generator,
// passwords still allowed. It records the code's step, the password's use, the sign-in on the user's
// row, their personal namespace where it is their first, and the session.
func (s *PasswordAPI) login(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	if !s.fromThePage(r) {
		fail(w, http.StatusForbidden, loginOrigin)
		return
	}
	var ask passwordAsked
	if err := readAtMost(r, &ask, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	if err := ask.check(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	now := s.now().Truncate(time.Microsecond)
	address := s.signIns.addressOf(r)
	wait, took := s.attempts.take(ask.Login, address, now)
	if !took {
		w.Header().Set("Retry-After", strconv.Itoa(int((wait+time.Second-1)/time.Second)))
		fail(w, http.StatusTooManyRequests, tooManyAttempts)
		return
	}
	// Given back where the attempt ends before a password was compared, or on the API's own
	// account; kept where it was a guess.
	guessed := false
	defer func() {
		if !guessed {
			s.attempts.forgive(ask.Login, address, now)
		}
	}()
	failed := func(reason string) {
		guessed = true
		s.refuse(r, ask.Login, address, reason, now)
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, noPasswordSignIn)
	}

	var a account
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		var err error
		a, err = s.readAccount(ctx, wide, ask.Login, now, wide.User)
		return err
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the sign-in could not be checked")
		return
	}
	if a.policy.forbidden {
		s.refuse(r, ask.Login, address, "passwords are forbidden by the policy that applies to the account", now)
		failSetting(w, http.StatusForbidden, passwordsForbidden, passwordSetting)
		return
	}

	hash := a.password.PasswordHash
	if hash == "" {
		hash = s.nobody
	}
	done, turned := s.hashing.turn(r.Context())
	if !turned {
		fail(w, http.StatusServiceUnavailable, hashingBusy)
		return
	}
	matched, err := password.Verify(hash, ask.Password)
	done()
	switch {
	case err != nil:
		s.report(fmt.Errorf("the password of %s could not be checked: %w", ask.Login, err))
		fail(w, http.StatusInternalServerError, "the sign-in could not be checked")
		return
	case !a.exists:
		failed("no user holds that login")
		return
	case a.password.ID == "":
		failed("the account holds no password")
		return
	case !matched:
		failed("the password does not match")
		return
	}

	var step int64
	switch {
	case a.totp.ID == "" && ask.coded:
		failed("a TOTP code was sent, and the account holds no TOTP generator")
		return
	case a.totp.ID != "" && !ask.coded:
		failed("the account holds a TOTP generator, and no code was sent")
		return
	case a.totp.ID != "":
		if s.totp == nil {
			s.report(fmt.Errorf("the TOTP generator of %s could not be opened: no master key is attached", ask.Login))
			fail(w, http.StatusInternalServerError, "the sign-in could not be checked")
			return
		}
		secret, err := s.totp.OpenTOTP(ask.Login, a.totp.ID, a.totp.TOTPSealed)
		if err != nil {
			s.report(fmt.Errorf("the TOTP generator of %s could not be opened: %w", ask.Login, err))
			fail(w, http.StatusInternalServerError, "the sign-in could not be checked")
			return
		}
		var ok bool
		step, ok = totp.Match(secret, ask.TOTP, now, a.totp.TOTPStep)
		clear(secret)
		if !ok {
			failed("the TOTP code does not match, or a code of its step or a later one was accepted already")
			return
		}
	}

	var cookie *http.Cookie
	var answer SignedIn
	err = s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		held, err := s.readAccount(ctx, wide, ask.Login, now, wide.HoldUser)
		switch {
		case err != nil:
			return err
		case !held.exists:
			return &refusal{reason: "the account was removed"}
		case held.user.Suspended:
			return &refusal{reason: "the account is suspended"}
		case held.policy.forbidden:
			return errForbidden
		case held.password.ID != a.password.ID || held.password.PasswordHash != a.password.PasswordHash:
			return &refusal{reason: "the password was changed or removed during the sign-in"}
		case held.totp.ID != a.totp.ID:
			return &refusal{reason: "the TOTP generator was enrolled or removed during the sign-in"}
		}
		cookie, answer, err = s.signIn(ctx, wide, held, address, step, now)
		return err
	})
	var refusedFor *refusal
	switch {
	case errors.Is(err, errForbidden):
		guessed = true
		s.refuse(r, ask.Login, address, "passwords were forbidden by the policy that applies to the account during the sign-in", now)
		failSetting(w, http.StatusForbidden, passwordsForbidden, passwordSetting)
		return
	case errors.As(err, &refusedFor):
		failed(refusedFor.reason)
		return
	case errors.Is(err, db.ErrTOTPSpent):
		failed("a code of the TOTP code's step or a later one was accepted already")
		return
	case errors.Is(err, db.ErrSessionRefused):
		failed("the account opens no session")
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the sign-in could not be completed")
		return
	}
	guessed = true
	s.attempts.signedIn(ask.Login, address, now)
	http.SetCookie(w, cookie)
	shownOnce(w, http.StatusOK, answer)
}

// errForbidden is a sign-in whose account's policy came to forbid passwords while it was checked.
var errForbidden = errors.New("api: passwords are forbidden")

// signIn signs a checked account in at now, in the transaction wide is: the code's step, the
// password's use, the sign-in, the personal namespace where it is the first, and a session opened by
// the password, full or enrolment-only as the policy says, recorded as signin.succeed. It answers
// the cookie to set once the transaction commits, and what the answer says.
func (s *PasswordAPI) signIn(ctx context.Context, wide *db.Wide, a account, address string, step int64, now time.Time) (*http.Cookie, SignedIn, error) {
	login := a.user.Login
	if a.totp.ID != "" {
		if err := wide.TOTPUsed(ctx, a.totp.ID, step, now); err != nil {
			return nil, SignedIn{}, err
		}
	}
	if err := wide.CredentialUsed(ctx, a.password.ID, now); err != nil {
		return nil, SignedIn{}, err
	}
	entries, err := personalNamespace(ctx, wide, login)
	if err != nil {
		return nil, SignedIn{}, err
	}
	cookie, signedIn, err := openSignedIn(ctx, wide, login, a.password.ID, address, now)
	if err != nil {
		return nil, SignedIn{}, err
	}
	kind := SessionFull
	if a.policy.enrolling(a.passkeys) {
		kind = SessionEnrolment
	}
	signedIn.record.Detail["session"] = kind
	return cookie, SignedIn{Login: login, Session: kind}, appendEntries(ctx, wide, append(entries, signedIn))
}

// refuse records a password sign-in refused, as signin.fail in a transaction of its own, since the
// sign-in it records wrote nothing: by the address it came from, about the login it named, with the
// reason, within the bound on the entries failed sign-ins append that the passkey ceremonies share
// (failures.go). Every such refusal is anybody's to send.
func (s *PasswordAPI) refuse(r *http.Request, login, address, reason string, now time.Time) {
	unrecorded, recorded := s.signIns.failures.admit(address, now)
	if !recorded {
		return
	}
	detail := map[string]any{"reason": reason, "address": address, "credential": db.CredentialPassword}
	if unrecorded > 0 {
		detail["unrecorded"] = unrecorded
	}
	err := s.pool.Installation(context.WithoutCancel(r.Context()), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		return wide.Audit(ctx, audit.Record{
			Actor: address, Action: audit.SigninFail, Target: login, Result: audit.Done, Detail: detail,
		})
	})
	if err != nil {
		s.report(fmt.Errorf("a failed sign-in could not be recorded: %w", err))
	}
}

// report says one thing, through whatever Trouble was given.
func (s *PasswordAPI) report(err error) {
	if s.trouble != nil {
		s.trouble(err)
	}
}
