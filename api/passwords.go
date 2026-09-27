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
	"strings"
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
// policy is satisfied and enrols passkeys and nothing else where the policy requires a passkey, which
// the account's passkeys are the way past and its password never is: "enrols passkeys, nothing else:
// cannot read a workflow, start a run or mint a token". Which of the two is not written anywhere: it
// is derived at every request from the credential that opened the session and the policy that
// applies then (Principals.identifySession), so that a policy changed applies from the next request.
// The answer says which it was when the session opened.
//
// A registration from such a session is what it is for, and the passkey ceremonies read it themselves
// for that; it opens no other session. The passkey that brings the account to min_passkeys takes the
// password, and the session with it: the account signs in with its passkeys from then on (policy.go).
// Setting the password that opened it is the one other thing it may do (passwords_set.go), which
// reads it itself as well. A sign-in is a sign-in, so the first one gives the user their personal
// namespace whatever the session may do.
//
// An administrator's sign-in to a full session ends the bootstrap token where it has not ended, as
// the enrolment that first gives an administrator one does: a password set while the policy required
// a passkey opened a session that only enrols and left the token going, and once the policy no
// longer requires one, the next sign-in of that password ends it. A session opened before, full from
// the same moment, ends nothing by itself: a sign-in or an enrolment is what ends the token.
//
// # Setting one
//
// A password is set from an enrolment code, on the enrolment page, and from a browser's session,
// and a TOTP generator is enrolled beside it from a session: passwords_set.go and passwords_totp.go.
// A password is a row of credentials of type password holding its hash as password.Hash writes it,
// and a generator one of type totp holding its secret sealed as the secret store seals one
// (TOTPSecrets).

// TOTPSecrets seals and opens the secret of a TOTP generator, under the master key as the
// credentials row holds it: "a code is checked against the secret itself, so it cannot be hashed,
// and a dump alone opens nothing". The secret store fills it, as it fills Secrets, since package api
// may not link the store.
type TOTPSecrets interface {
	SealTOTP(login, id string, secret []byte) ([]byte, error)
	OpenTOTP(login, id string, sealed []byte) ([]byte, error)
}

// PasswordOptions are what the password sign-in is given.
type PasswordOptions struct {
	Pool *db.Pool

	// PublicURL is AGK_PUBLIC_URL, or AGK_PROXY_URL behind a proxy: its origin is the sign-in
	// page's, which a sign-in is accepted from, and a host that is an IP address is an installation
	// where the policy is applied with passwords allowed and no passkey.
	PublicURL string

	// TOTP seals and opens the secrets of TOTP generators. Nil opens none, and an account holding
	// one is then refused its sign-in, with a 500, rather than signed in with its password alone;
	// nor does it enrol one.
	TOTP TOTPSecrets

	// Identify reads the session a password is set from. It is the installation's own,
	// Principals.Identify, and not the router's, which refuses a session that may only enrol
	// everywhere, and such a session, opened by the password it holds, may set another.
	Identify Identify

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
	pool     *db.Pool
	totp     TOTPSecrets
	identify Identify
	signIns  *SignIns
	now      func() time.Time
	trouble  func(error)

	// origin is the sign-in page's, host the public URL's, which a TOTP generator names its
	// account at, and ipAddressed an installation addressed by an IP address.
	origin, host string
	ipAddressed  bool

	attempts *attempts
	hashing  *hashing

	// nobody is the hash of a password nobody knows, verified where the login names no password,
	// so that such a sign-in takes as long as any other.
	nobody string

	// checked is run between the checks and the transaction of a sign-in, or of a password or a
	// TOTP generator set or removed, where a test changes what was checked. Nil outside tests.
	checked func()
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
	passwordsForbidden = "passwords are forbidden by the authentication policy that applies to this account, and none is accepted: sign in with a passkey"

	// noPasswordSignIn is every other refusal, one sentence for every reason.
	noPasswordSignIn = "that sign-in opens nothing: the login, the password or the TOTP code does not match, or the account opens no session. Try again, or sign in with a passkey"

	// tooManyAttempts is a sign-in past the count of attempts. When one more fits is Retry-After's
	// to say, in seconds, which the sign-in page turns into minutes for a person.
	tooManyAttempts = "too many password sign-ins were tried for this account or from this address in the last quarter of an hour: try again later, or sign in with a passkey"

	// hashingBusy is a sign-in that waited too long for its turn to hash.
	hashingBusy = "this installation is checking too many passwords at once to check this one in time: try again in a moment"
)

// NewPasswords registers the password sign-in on a router.
func NewPasswords(rt *Router, o PasswordOptions) (*PasswordAPI, error) {
	switch {
	case rt == nil:
		return nil, errors.New("api: no router")
	case o.Pool == nil:
		return nil, errors.New("api: no database, and the password and the policy are kept there")
	case o.Identify == nil:
		return nil, errors.New("api: no way to read a session, and a signed-in user sets a password from theirs")
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
		pool: o.Pool, totp: o.TOTP, identify: o.Identify, signIns: o.SignIns, now: o.Now, trouble: o.Trouble,
		origin: origin, host: strings.ToLower(u.Hostname()), ipAddressed: net.ParseIP(u.Hostname()) != nil,
		attempts: newAttempts(), hashing: newHashing(hashingTurns(), hashingWait), nobody: nobody,
	}
	public := Public{Why: "a password sign-in is how somebody proves who they are, with the password it carries, and opens the session every other route is then authorised by"}
	if err := rt.Handle("POST", "/api/v1/auth/login", public, s.login); err != nil {
		return nil, err
	}
	if err := s.setting(rt); err != nil {
		return nil, err
	}
	return s, nil
}

// passwordAsked is openapi.json's passwordLoginRequest.
type passwordAsked struct {
	Login    string
	Password string
	TOTP     string

	// Terminal is what agk login opened the sign-in page with, for a sign-in it started: the way it
	// signs in on an installation addressed by an IP address, where no passkey can be used.
	Terminal *TerminalSignIn

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
	case "terminal":
		return readTerminal(b, &q.Terminal)
	}
	return unknown(name)
}

// check refuses what the schema refuses: a login no user can hold, a service account's among them,
// no password, a code that is not six digits, and agk login's terminal outside its grammar.
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
	case q.Terminal != nil:
		return q.Terminal.check()
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

	// RedirectTo is where the sign-in page sends the browser next, for a sign-in agk login started
	// that opened a full session: its loopback address with a one-time code (handOff). Never beside
	// a session that may only enrol, which mints no token.
	RedirectTo string `json:"redirect_to,omitempty"`
}

// account is what a password sign-in reads of the account a login names: whether a user holds it,
// their credentials, their password and their TOTP generator among them where they hold one, and the
// policy that applies to them.
type account struct {
	user     db.User
	exists   bool
	held     []db.Credential
	password db.Credential
	totp     db.Credential
	policy   accountPolicy
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
	if a.policy, err = policyFor(ctx, wide, login, now, s.ipAddressed); err != nil || !a.exists {
		return a, err
	}
	if a.held, err = wide.CredentialsOf(ctx, login); err != nil {
		return account{}, err
	}
	for _, c := range a.held {
		switch c.Type {
		case db.CredentialPassword:
			a.password = c
		case db.CredentialTOTP:
			a.totp = c
		}
	}
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
	// account; kept where it was a guess. A refusal by a policy forbidding passwords is kept by the
	// address alone: it compared nothing, but it tells a login holding a grant where passwords are
	// forbidden from one nobody holds, and a question that answers who has an account is counted
	// like a guess where it is asked from.
	guessed, asked := false, false
	defer func() {
		switch {
		case asked:
			s.attempts.forgetLogin(ask.Login, now)
		case !guessed:
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
	if a.policy.passwordsForbidden {
		asked = true
		s.refuse(r, ask.Login, address, "passwords are forbidden by the policy that applies to the account", now)
		if a.password.ID != "" {
			s.passwordGoes(r, ask.Login, now)
		}
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
	case a.user.Suspended:
		// Refused here as well as under the user's row, so that a right password for a
		// suspended account is answered as soon as a wrong one, rather than after a
		// transaction a wrong one never reaches.
		failed("the account is suspended")
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

	s.betweenChecks()
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
		case held.policy.passwordsForbidden:
			return errForbidden
		case held.password.ID != a.password.ID || held.password.PasswordHash != a.password.PasswordHash:
			return &refusal{reason: "the password was changed or removed during the sign-in"}
		case held.totp.ID != a.totp.ID:
			return &refusal{reason: "the TOTP generator was enrolled or removed during the sign-in"}
		}
		cookie, answer, err = s.signIn(ctx, wide, held, address, step, ask.Terminal, now)
		return err
	})
	var refusedFor *refusal
	switch {
	case errors.Is(err, errForbidden):
		guessed = true
		s.refuse(r, ask.Login, address, "passwords were forbidden by the policy that applies to the account during the sign-in", now)
		s.passwordGoes(r, ask.Login, now)
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

// betweenChecks runs checked, where a test gave one.
func (s *PasswordAPI) betweenChecks() {
	if s.checked != nil {
		s.checked()
	}
}

// forbiddenGoes is why a password goes when a sign-in finds passwords forbidden to its account.
const forbiddenGoes = "passwords are forbidden by the policy that applies to the account, and a sign-in found one"

// passwordGoes deletes login's password, and the TOTP generator beside it, where a sign-in found
// passwords forbidden to them and a password still held. Forbidding passwords deletes those it
// reaches when it comes to forbid them; an account that came under a namespace forbidding them
// since, by a grant or a group, still holds one, which goes the first time it is offered rather
// than signing in again once that grant has ended: "forbidden deletes the stored hash".
//
// In a transaction of its own, the refusal having written nothing, under the user's row with the
// policy read again, and recorded as credential.remove by the installation, whose policy it is. What
// goes wrong is the installation's trouble, the refusal being answered all the same.
func (s *PasswordAPI) passwordGoes(r *http.Request, login string, now time.Time) {
	err := s.pool.Installation(context.WithoutCancel(r.Context()), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		a, err := s.readAccount(ctx, wide, login, now, wide.HoldUser)
		if err != nil || !a.exists || !a.policy.passwordsForbidden || a.password.ID == "" {
			return err
		}
		// The TOTP generator goes with the password's row, as the table holds it.
		if err := wide.RemoveCredential(ctx, login, a.password.ID); err != nil {
			return err
		}
		entries := []entry{{record: audit.Record{
			Actor: installationActor, Action: audit.CredentialRemove, Target: a.password.ID, Result: audit.Done,
			Detail: map[string]any{"type": db.CredentialPassword, "login": login, "reason": forbiddenGoes},
		}}}
		if a.totp.ID != "" {
			entries = append(entries, entry{record: audit.Record{
				Actor: installationActor, Action: audit.CredentialRemove, Target: a.totp.ID, Result: audit.Done,
				Detail: map[string]any{"type": db.CredentialTOTP, "login": login, "reason": "removed with the password it stood beside"},
			}})
		}
		return appendEntries(ctx, wide, entries)
	})
	if err != nil {
		s.report(fmt.Errorf("the password of %s, which the policy forbids, could not be deleted: %w", login, err))
	}
}

// errForbidden is a sign-in whose account's policy came to forbid passwords while it was checked.
var errForbidden = errors.New("api: passwords are forbidden")

// signIn signs a checked account in at now, in the transaction wide is: the code's step, the
// password's use, the sign-in, the personal namespace where it is the first, the end of the bootstrap
// token where an administrator's session is a full one, a session opened by the password, full or
// enrolment-only as the policy says, recorded as signin.succeed, and, for a sign-in agk login started
// that opened a full session, its one-time code. It answers the cookie to set once the transaction
// commits, and what the answer says.
func (s *PasswordAPI) signIn(ctx context.Context, wide *db.Wide, a account, address string, step int64, terminal *TerminalSignIn, now time.Time) (*http.Cookie, SignedIn, error) {
	login := a.user.Login
	if a.totp.ID != "" {
		if err := wide.TOTPUsed(ctx, a.totp.ID, step, now); err != nil {
			return nil, SignedIn{}, err
		}
	}
	if err := wide.CredentialUsed(ctx, a.password.ID, now); err != nil {
		return nil, SignedIn{}, err
	}
	personal, err := personalNamespace(ctx, wide, login)
	if err != nil {
		return nil, SignedIn{}, err
	}
	kind := SessionFull
	if a.policy.enrolling() {
		kind = SessionEnrolment
	}
	// An administrator whose password opens a full session can administer from it, so the
	// bootstrap token ends here, as it ends at the enrolment that first gives an administrator a
	// full session (passkeys.go, passwords_set.go). Setting the password did not end it where the
	// policy then required a passkey; ended at none of this administrator's sign-ins once the
	// policy relaxed, the token would go on beside the administrator it made. Taken after the rows
	// the sign-in writes and before the audit log, and ending nothing once ended. A suspended
	// account never reaches here.
	var entries []entry
	if a.user.Admin && kind == SessionFull {
		ended, err := wide.EndBootstrap(ctx, now)
		if err != nil {
			return nil, SignedIn{}, err
		}
		if ended {
			entries = append(entries, entry{record: audit.Record{
				Actor: login, Action: audit.BootstrapEnd, Target: string(BootstrapOperator), Result: audit.Done,
				Detail: map[string]any{"first_administrator": login, "credential": a.password.ID},
			}})
		}
	}
	cookie, signedIn, err := openSignedIn(ctx, wide, login, a.password.ID, address, now)
	if err != nil {
		return nil, SignedIn{}, err
	}
	signedIn.record.Detail["session"] = kind
	answer := SignedIn{Login: login, Session: kind}
	if terminal != nil && kind == SessionFull {
		if answer.RedirectTo, err = handOff(ctx, wide, terminal, login, a.password.ID, now); err != nil {
			return nil, SignedIn{}, err
		}
	}
	return cookie, answer, appendEntries(ctx, wide, append(append(entries, personal...), signedIn))
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
	// The credential's type rather than its identifier, which "credential" holds where a passkey's
	// failure names one: a password's is not known for a login that names none.
	detail := map[string]any{"reason": reason, "address": address, "credential_type": db.CredentialPassword}
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

// refuseCode records a password set from an enrolment code refused, as a failed sign-in
// (SignIns.refuseCode), telling Trouble where it could not be recorded: the refusal is answered all
// the same.
func (s *PasswordAPI) refuseCode(r *http.Request, f codeFailure, now time.Time) {
	if err := s.signIns.refuseCode(r.Context(), s.pool, s.signIns.addressOf(r), f, now); err != nil {
		s.report(fmt.Errorf("a failed sign-in could not be recorded: %w", err))
	}
}

// report says one thing, through whatever Trouble was given.
func (s *PasswordAPI) report(err error) {
	if s.trouble != nil {
		s.trouble(err)
	}
}
