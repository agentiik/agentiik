package api

import (
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/password"
	"github.com/agentiik/agentiik/internal/ulid"
)

// Setting a password, where the policy that applies to the account allows passwords, which on an
// installation addressed by an IP address it always does, so that somebody can sign in there at all:
// from an enrolment code, POST /api/v1/auth/password/enrol, which the enrolment page calls beside the
// passkey ceremony, or in place of it where no ceremony runs; and from a browser's session, PUT and
// DELETE /api/v1/me/password, which the signed-in page offers. A TOTP generator is enrolled beside
// the password from a session: passwords_totp.go.
//
// # From an enrolment code
//
// The code is an enrolment link's or a recovery code, of any of the three kinds, since each is what
// an administrator gives somebody to get in with, and on an installation addressed by an IP address
// a password is the one thing to get in with. It is spent by the password it sets, in the
// transaction that records the password, and the password then opens the session a password opens:
// full where the policy is met, and enrolling passkeys and nothing else where it requires a
// passkey, as a password sign-in's would. A user suspended for holding no passkey where passwords
// were forbidden comes back by it: passwords are allowed again wherever one is set, so the password
// lifts that suspension, as a passkey enrolled from a code does, and signs them in. A user suspended
// for another reason sets a password with a code and opens no session, as they would enrolling a
// passkey.
//
// An administrator's code, the first administrator's link or a recovery code, ends the bootstrap
// token where it has not ended and the session the password opens is a full one: always on an
// installation addressed by an IP address, and where the policy requires no passkey. Where it
// requires one, the bootstrap stays until the administrator registers a passkey, from the session
// the password opened or a later one, which ends it then (passkeys.go), or until the password signs
// them in to a full session, or a session it opened is full at a request, the policy relaxed since
// (passwords.go, sessions.go): ended at the password, it would leave the installation to somebody
// who can do nothing but enrol.
//
// A code sets the password in place of one held, which only a recovery code can meet, since the
// other two are issued to a user holding no credential; the TOTP generator beside the old password
// goes with it, and so do the sessions the old password opened. A recovery code is what an
// administrator gives somebody who lost what signs them in, and the generator is half of that: kept,
// it would ask the new password for codes from a device the person may no longer hold.
//
// # From a session
//
// A browser's session sets one, a full session or one that may only enrol, which the router refuses
// everywhere else: such a session was opened by the password it would change, and proves the
// current one as any other session does. Changing a password held takes the current one, which is
// counted as a guess at it is at a sign-in, so that a session left open on a shared machine is not a
// way to try every password of the account's holder. Setting one where none is held, from a session
// a passkey opened, takes nothing more than the session.
//
// A password changed keeps its identifier, and so the TOTP generator beside it, and ends every other
// session it opened: whoever knew the one before may be who holds them. The session it is changed
// from goes on.
//
// Removing it is refused where the account would be left with fewer passkeys the policy accepts
// than min_passkeys, "credentials required before the password may be removed", which being at
// least one also means never with no credential; and always on an installation addressed by an IP
// address, where no passkey signs anybody in and the password is the one credential that does. It
// takes the TOTP generator beside it, and the sessions it opened, the one it is removed from among
// them where that one was.
//
// Where the policy requires a passkey and the account holds the min_passkeys passkeys it accepts, a
// password is set from neither and the request is a 409 naming passkey: every session it opened
// would only enrol, with nothing left to enrol for, and the next passkey would take it.
//
// A bearer token sets and removes nothing: a token that leaked would otherwise be a way to a
// credential that outlives it, and these routes are the sign-in page's, where a browser's session is.
// Neither does the bootstrap token's operator, who is nobody's account.

// The rules a password is set under, NIST SP 800-63B's for a secret its holder chooses: at least
// passwordMin characters, since length is what makes a guess expensive; at most passwordMaxBytes
// bytes, refused before anything is hashed, which is room for any passphrase and not for a body
// meant to make hashing cost; and not the login, the first thing anybody guessing tries. No
// composition rule, a digit, a capital or a symbol required, which 800-63B tells a verifier not to
// impose: they make a password harder to remember and hardly harder to guess, and push people to the
// same few substitutions everybody guesses. No expiry either, for the same reason: a password is
// changed when there is a reason to, which its holder decides. Characters are counted as Unicode code
// points, as 800-63B counts them, and the password is hashed as the bytes sent, prepared by no
// profile.
const (
	passwordMin      = 12
	passwordMaxBytes = 1024
)

// The sentences setting a password is refused with.
const (
	// enrolOrigin is a password set from an enrolment code not from the enrolment page.
	enrolOrigin = "a password is set from an enrolment code on the enrolment page of this installation's public URL, and this request's Origin header names another or none"

	// passwordsForbiddenToSet is a password set where the policy that applies to the account
	// forbids them, answered naming the setting.
	passwordsForbiddenToSet = "passwords are forbidden by the authentication policy that applies to this account, and none is set: enrol a passkey"

	// passwordOnlyEnrols is a password set from a session where the policy that applies to the
	// account requires a passkey and the account holds the min_passkeys passkeys it accepts,
	// answered naming the setting; codeOnlyEnrols is the same from an enrolment code, whose holder
	// has likely lost the passkeys the account holds, and registers one with the code instead.
	passwordOnlyEnrols = "the authentication policy that applies to this account requires a passkey, and the account holds the passkeys it asks for already: a password could only ever open a session that enrols one, and none is set. Sign in with a passkey"
	codeOnlyEnrols     = "the authentication policy that applies to this account requires a passkey, and the account holds the passkeys it asks for already: a password could only ever open a session that enrols one, and none is set. Register a passkey with this code instead"

	// credentialsFromASession is a password or a TOTP generator set or removed with a bearer
	// token.
	credentialsFromASession = "a password and a TOTP generator are set and removed from a browser's session, on the sign-in page, and this request carries a bearer token: a token that leaked would otherwise be a way to a credential that outlives it"

	// sessionOfACodeSets is a password set from a session an enrolment code opened.
	sessionOfACodeSets = "this session was opened by an enrolment link, and sets nothing: open the link again, whose code sets the password"

	// currentMismatch is a password changed with a current one that does not match.
	currentMismatch = "the current password does not match, and the password is left as it was"

	// passwordChanged is a password set while another request set or removed it.
	passwordChanged = "the password was set or removed by another request while this one was checked: try again"

	// noPasswordHeld is a password removed from an account that holds none.
	noPasswordHeld = "this account holds no password"

	// onlyCredentialHere is a password removed on an installation addressed by an IP address.
	onlyCredentialHere = "this installation is addressed by an IP address, where no passkey signs anybody in, and the password is the one credential that does: it is not removed"
)

// setting registers the routes that set a password and a TOTP generator.
func (s *PasswordAPI) setting(rt *Router) error {
	enrol := Public{Why: "a password set from an enrolment code is how somebody who holds no credential yet gets one, with the code it carries, as a registration from a code is"}
	if err := rt.Handle("POST", "/api/v1/auth/password/enrol", enrol, s.enrol); err != nil {
		return err
	}
	set := Public{Why: "a password is set from a browser's session, one that may only enrol included, which the router refuses everywhere else since enrolling is all it may do and its password is what it enrols with: it reads the session itself"}
	if err := rt.Handle("PUT", "/api/v1/me/password", set, s.setPassword); err != nil {
		return err
	}
	for _, r := range []struct {
		method, pattern string
		handler         OwnHandler
	}{
		{"DELETE", "/api/v1/me/password", s.removePassword},
		{"POST", "/api/v1/me/totp", s.startTOTP},
		{"POST", "/api/v1/me/totp/confirm", s.confirmTOTP},
		{"DELETE", "/api/v1/me/totp", s.removeTOTP},
	} {
		if err := rt.HandleOwn(r.method, r.pattern, Own{}, r.handler); err != nil {
			return err
		}
	}
	return nil
}

// newPassword refuses a password this installation does not set, before anything is hashed or read,
// for the member name it was sent as: the length rules, which the schema could state.
func newPassword(name, secret string) error {
	switch n := utf8.RuneCountInString(secret); {
	case len(secret) > passwordMaxBytes:
		return fmt.Errorf("%s: a password is at most %d bytes, and this one is longer", name, passwordMaxBytes)
	case n < passwordMin:
		// How long it is, and never what it is.
		return fmt.Errorf("%s: a password is at least %d characters, and this one is %d; a few words together make one easily remembered", name, passwordMin, n)
	}
	return nil
}

// notTheLogin refuses a password that is its account's login, whichever the case of its letters.
func notTheLogin(name, secret, login string) error {
	if strings.EqualFold(secret, login) {
		return fmt.Errorf("%s: a password is not the login it signs in, which is the first thing anybody guessing tries", name)
	}
	return nil
}

// PasswordHeld is a password or a TOTP generator as the API answers one, $defs/passwordCredential
// and $defs/totpCredential: that it exists, and never its hash or its secret.
type PasswordHeld struct {
	Type       string    `json:"type"`
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at,omitzero"`
}

func heldOf(c db.Credential) PasswordHeld {
	h := PasswordHeld{Type: c.Type, ID: c.ID, CreatedAt: c.CreatedAt.UTC()}
	if !c.LastUsedAt.IsZero() {
		h.LastUsedAt = c.LastUsedAt.UTC()
	}
	return h
}

// PasswordEnrolled is what a password set from an enrolment code answers: whose it is, what it is,
// and what the session it opened may do, which is absent where it opened none.
type PasswordEnrolled struct {
	Login      string       `json:"login"`
	Session    string       `json:"session,omitempty"`
	Credential PasswordHeld `json:"credential"`
}

// passwordEnrolment is the body of POST /api/v1/auth/password/enrol: the code and the password.
type passwordEnrolment struct {
	Code     string
	Password string
}

func (q *passwordEnrolment) field(b *body, name string) error {
	switch name {
	case "code":
		return text(b, &q.Code)
	case "password":
		return text(b, &q.Password)
	}
	return unknown(name)
}

// check refuses what the schema refuses: a code outside its grammar and a password outside its
// lengths.
func (q passwordEnrolment) check() error {
	if !enrolmentCode.MatchString(q.Code) {
		// The code is not repeated, since it may be most of a real one.
		return errors.New("code: an enrolment code is agkenrol_ and at least 43 base64url characters, as the link after its # carries it")
	}
	return newPassword("password", q.Password)
}

// enrol is POST /api/v1/auth/password/enrol.
//
// The code and the policy that applies to its user are read in a transaction of their own, the
// password is hashed outside any, and the enrolment is then one transaction, in the order a passkey
// registered from a code takes its locks: the user's row, their personal namespace, the bootstrap
// state where the code is a first administrator's, then the code, which it spends; then the
// password, the end of the bootstrap where it ends, and the session, recorded in the audit log last.
func (s *PasswordAPI) enrol(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	if !s.fromThePage(r) {
		fail(w, http.StatusForbidden, enrolOrigin)
		return
	}
	var ask passwordEnrolment
	if err := readAtMost(r, &ask, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	if err := ask.check(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	refused := func() {
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, codeOpensNothing)
	}
	now := s.now().Truncate(time.Microsecond)
	sum := sha256.Sum256([]byte(ask.Code))
	codeHash := sum[:]

	var a account
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		code, err := wide.EnrolmentCodeByHash(ctx, codeHash, now)
		if err != nil {
			return err
		}
		a, err = s.readAccount(ctx, wide, code.Login, now, wide.User)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoEnrolmentCode):
		refused()
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the password could not be set")
		return
	case !a.exists:
		refused()
		return
	case a.policy.passwordsForbidden:
		failSetting(w, http.StatusForbidden, passwordsForbiddenToSet, passwordSetting)
		return
	case a.policy.enrolledPast(a.held):
		failSetting(w, http.StatusConflict, codeOnlyEnrols, passkeySetting)
		return
	}
	login := a.user.Login
	if err := notTheLogin("password", ask.Password, login); err != nil {
		fail(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	hash, ok := s.hash(w, r, ask.Password)
	if !ok {
		return
	}

	address := s.signIns.addressOf(r)
	s.betweenChecks()
	var answer PasswordEnrolled
	var cookie *http.Cookie
	err = s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		user, err := wide.HoldUser(ctx, login)
		if errors.Is(err, db.ErrNoPrincipal) {
			return &refusal{reason: "the user was removed"}
		}
		if err != nil {
			return err
		}
		// Lifted here, before the session is decided, and rolled back with everything else where
		// what is read below refuses the password: passwords forbidden again, or a code that opens
		// nothing.
		lifted := false
		if user.Suspended && user.SuspendedFor == db.SuspendedNoPasskey {
			if lifted, err = wide.LiftSuspension(ctx, login, db.SuspendedNoPasskey); err != nil {
				return err
			}
			if lifted {
				user.Suspended, user.SuspendedFor = false, ""
			}
		}
		signs := !user.Suspended
		var personal []entry
		if signs {
			if personal, err = personalNamespace(ctx, wide, login); err != nil {
				return err
			}
		}
		code, err := wide.EnrolmentCodeByHash(ctx, codeHash, now)
		if err != nil {
			return err
		}
		if code.Login != login {
			return &refusal{reason: "the code is another user's"}
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
		if code, err = wide.UseEnrolmentCode(ctx, codeHash, now); err != nil {
			return err
		}
		held, err := s.readAccount(ctx, wide, login, now, func(context.Context, string) (db.User, error) { return user, nil })
		if err != nil {
			return err
		}
		if held.policy.passwordsForbidden {
			return errForbidden
		}
		if held.policy.enrolledPast(held.held) {
			return errOnlyEnrols
		}
		set, replaced, err := wide.SetPassword(ctx, login, ulid.New(), hash, now)
		if err != nil {
			return err
		}
		enrolled := map[string]any{"type": db.CredentialPassword, "replaced": replaced}
		if lifted {
			enrolled["suspension_lifted"] = db.SuspendedNoPasskey
		}
		var removed []entry
		if replaced {
			ended, err := wide.EndSessionsOpenedBy(ctx, login, set.ID, nil, now)
			if err != nil {
				return err
			}
			enrolled["sessions_ended"] = ended
			if held.totp.ID != "" {
				if err := wide.RemoveCredential(ctx, login, held.totp.ID); err != nil {
					return err
				}
				removed = append(removed, entry{record: audit.Record{
					Actor: login, Action: audit.CredentialRemove, Target: held.totp.ID, Result: audit.Done,
					Detail: map[string]any{"type": db.CredentialTOTP, "reason": "a password set from an enrolment code replaced the one it stood beside"},
				}})
			}
		}
		entries := append([]entry{{record: audit.Record{
			Actor: login, Action: audit.CredentialEnrol, Target: set.ID, Result: audit.Done, Detail: enrolled,
		}}}, removed...)
		entries = append(entries, entry{record: audit.Record{
			Actor: login, Action: audit.EnrolmentUse, Target: login, Result: audit.Done,
			Detail: map[string]any{"kind": code.Kind, "issued_by": code.IssuedBy, "credential": set.ID},
		}})
		kind := SessionFull
		if held.policy.enrolling() {
			kind = SessionEnrolment
		}
		// The bootstrap ends where the first administrator can sign in to a full session, and
		// not before: see the top of this file.
		if user.Admin && !user.Suspended && kind == SessionFull {
			ended, err := wide.EndBootstrap(ctx, now)
			if err != nil {
				return err
			}
			if ended {
				entries = append(entries, entry{record: audit.Record{
					Actor: login, Action: audit.BootstrapEnd, Target: string(BootstrapOperator), Result: audit.Done,
					Detail: map[string]any{"first_administrator": login, "credential": set.ID},
				}})
			}
		}
		answer = PasswordEnrolled{Login: login, Credential: heldOf(set)}
		if signs {
			opened, signedIn, err := openSignedIn(ctx, wide, login, set.ID, address, now)
			if err != nil {
				return err
			}
			signedIn.record.Detail["session"] = kind
			cookie, answer.Session = opened, kind
			entries = append(append(entries, personal...), signedIn)
		}
		return appendEntries(ctx, wide, entries)
	})
	var refusedFor *refusal
	switch {
	case errors.Is(err, errForbidden):
		failSetting(w, http.StatusForbidden, passwordsForbiddenToSet, passwordSetting)
		return
	case errors.Is(err, errOnlyEnrols):
		failSetting(w, http.StatusConflict, codeOnlyEnrols, passkeySetting)
		return
	case errors.As(err, &refusedFor), errors.Is(err, db.ErrNoEnrolmentCode):
		// A code spent, lapsed or replaced since it was read, or a user removed: nothing was
		// written.
		refused()
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the password could not be set")
		return
	}
	if cookie != nil {
		http.SetCookie(w, cookie)
	}
	shownOnce(w, http.StatusOK, answer)
}

// errOnlyEnrols is a password set, in the transaction that would record it, where the policy requires
// a passkey and the account holds the passkeys it asks for, as a passkey registered since the account
// was read may have brought it to.
var errOnlyEnrols = errors.New("api: a password here would only enrol")

// hash hashes a password set, in its turn, and answers false where it has answered the request
// already: 503 where no turn came in time, 500 where the hash could not be made.
func (s *PasswordAPI) hash(w http.ResponseWriter, r *http.Request, secret string) (string, bool) {
	done, turned := s.hashing.turn(r.Context())
	if !turned {
		fail(w, http.StatusServiceUnavailable, hashingBusy)
		return "", false
	}
	hash, err := password.Hash(secret)
	done()
	if err != nil {
		s.report(fmt.Errorf("a password could not be hashed: %w", err))
		fail(w, http.StatusInternalServerError, "the password could not be set")
		return "", false
	}
	return hash, true
}

// sessionUser answers the user a request setting or removing a password or a TOTP generator is
// from, as the router identified them, and false where it has refused it: a bearer token and the
// bootstrap token's operator set nothing.
func sessionUser(w http.ResponseWriter, caller Caller) (string, bool) {
	if caller.Token != "" || caller.Principal == BootstrapOperator {
		fail(w, http.StatusForbidden, credentialsFromASession)
		return "", false
	}
	return string(caller.Principal), true
}

// setter is who a request setting a password is from: the user whose browser's session it carries,
// a full one or one that may only enrol, and false where it has answered the request already. A
// bearer token is refused whatever it names, as a request carrying one and a session is refused
// everywhere; a session an enrolment code opened is refused as the registration ceremony refuses it,
// since the code, not the session, is what sets a password from a link.
func (s *PasswordAPI) setter(w http.ResponseWriter, r *http.Request) (Identity, bool) {
	as, err := s.identify(r)
	switch {
	case err != nil:
		fail(w, http.StatusInternalServerError, "the request could not be authenticated")
		return Identity{}, false
	case as.Principal == "":
		unauthenticated(w, as)
		return Identity{}, false
	case as.Token != "" || as.Principal == BootstrapOperator:
		fail(w, http.StatusForbidden, credentialsFromASession)
		return Identity{}, false
	case as.OpenedByCode:
		fail(w, http.StatusForbidden, sessionOfACodeSets)
		return Identity{}, false
	}
	return as, true
}

// sessionHash is the SHA-256 of the one session r carries, which the router has read already: the
// session a password is set from, which setting it ends no more than it ends the request.
func sessionHash(r *http.Request) []byte {
	for _, c := range r.CookiesNamed(SessionCookie) {
		if c.Value != "" {
			sum := sha256.Sum256([]byte(c.Value))
			return sum[:]
		}
	}
	return nil
}

// passwordSet is the body of PUT /api/v1/me/password: the password, and the current one where the
// account holds one.
type passwordSet struct {
	Password string
	Current  string

	// current is whether the request wrote a current password, null being none.
	current bool
}

func (q *passwordSet) field(b *body, name string) error {
	switch name {
	case "password":
		return text(b, &q.Password)
	case "current_password":
		q.current = b.d.PeekKind() != jsontext.KindNull
		return text(b, &q.Current)
	}
	return unknown(name)
}

// check refuses what the schema refuses: a password outside its lengths, and a current one longer
// than any password is.
func (q passwordSet) check() error {
	if len(q.Current) > passwordMaxBytes {
		return fmt.Errorf("current_password: a password is at most %d bytes, and this one is longer", passwordMaxBytes)
	}
	return newPassword("password", q.Password)
}

// setPassword is PUT /api/v1/me/password.
//
// The account is read in a transaction of its own, the current password checked and the new one
// hashed outside any, and the password then set in one transaction under the user's row, where what
// was checked is what is there: the same password, passwords still allowed.
func (s *PasswordAPI) setPassword(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	as, ok := s.setter(w, r)
	if !ok {
		return
	}
	login := string(as.Principal)
	var ask passwordSet
	if err := readAtMost(r, &ask, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	if err := ask.check(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := notTheLogin("password", ask.Password, login); err != nil {
		fail(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	now := s.now().Truncate(time.Microsecond)
	var a account
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		var err error
		a, err = s.readAccount(ctx, wide, login, now, wide.User)
		return err
	})
	switch {
	case err != nil:
		fail(w, http.StatusInternalServerError, "the password could not be set")
		return
	case !a.exists:
		// Removed since the session was read, which the removal ended.
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, noSession)
		return
	case a.policy.passwordsForbidden:
		failSetting(w, http.StatusForbidden, passwordsForbiddenToSet, passwordSetting)
		return
	case a.policy.enrolledPast(a.held):
		failSetting(w, http.StatusConflict, passwordOnlyEnrols, passkeySetting)
		return
	case a.password.ID != "" && !ask.current:
		fail(w, http.StatusBadRequest, "current_password: this account holds a password, and setting another takes the current one")
		return
	case a.password.ID == "" && ask.current:
		fail(w, http.StatusBadRequest, "current_password: this account holds no password, so there is no current one to send")
		return
	case a.password.ID == "" && !provedSince(as.ProvedAt, now):
		// A first password is a way in that outlives the session, which the session alone does
		// not give; a password changed is sent beside the one it replaces, which proves it.
		askAgain(w)
		return
	}

	address := s.signIns.addressOf(r)
	if a.password.ID != "" {
		// A guess at the current password, counted as a sign-in's is, before it is hashed.
		wait, took := s.attempts.take(login, address, now)
		if !took {
			w.Header().Set("Retry-After", strconv.Itoa(int((wait+time.Second-1)/time.Second)))
			fail(w, http.StatusTooManyRequests, tooManyAttempts)
			return
		}
		done, turned := s.hashing.turn(r.Context())
		if !turned {
			s.attempts.forgive(login, address, now)
			fail(w, http.StatusServiceUnavailable, hashingBusy)
			return
		}
		matched, err := password.Verify(a.password.PasswordHash, ask.Current)
		done()
		switch {
		case err != nil:
			s.attempts.forgive(login, address, now)
			s.report(fmt.Errorf("the password of %s could not be checked: %w", login, err))
			fail(w, http.StatusInternalServerError, "the password could not be set")
			return
		case !matched:
			fail(w, http.StatusForbidden, currentMismatch)
			return
		}
		s.attempts.signedIn(login, address, now)
	}
	hash, ok := s.hash(w, r, ask.Password)
	if !ok {
		return
	}

	kept := sessionHash(r)
	s.betweenChecks()
	var set db.Credential
	err = s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		held, err := s.readAccount(ctx, wide, login, now, wide.HoldUser)
		switch {
		case err != nil:
			return err
		case !held.exists:
			return db.ErrNoPrincipal
		case held.policy.passwordsForbidden:
			return errForbidden
		case held.policy.enrolledPast(held.held):
			return errOnlyEnrols
		case held.password.ID != a.password.ID || held.password.PasswordHash != a.password.PasswordHash:
			return &refusal{reason: passwordChanged}
		}
		var replaced bool
		if set, replaced, err = wide.SetPassword(ctx, login, ulid.New(), hash, now); err != nil {
			return err
		}
		detail := map[string]any{"type": db.CredentialPassword, "replaced": replaced}
		if replaced {
			ended, err := wide.EndSessionsOpenedBy(ctx, login, set.ID, kept, now)
			if err != nil {
				return err
			}
			detail["sessions_ended"] = ended
		}
		return wide.Audit(ctx, audit.Record{Actor: login, Action: audit.CredentialEnrol, Target: set.ID, Result: audit.Done, Detail: detail})
	})
	var refusedFor *refusal
	switch {
	case errors.Is(err, errForbidden):
		failSetting(w, http.StatusForbidden, passwordsForbiddenToSet, passwordSetting)
	case errors.Is(err, errOnlyEnrols):
		failSetting(w, http.StatusConflict, passwordOnlyEnrols, passkeySetting)
	case errors.As(err, &refusedFor), errors.Is(err, db.ErrCredentialExists):
		fail(w, http.StatusConflict, passwordChanged)
	case errors.Is(err, db.ErrNoPrincipal):
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, noSession)
	case err != nil:
		fail(w, http.StatusInternalServerError, "the password could not be set")
	default:
		shownOnce(w, http.StatusOK, heldOf(set))
	}
}

// errNoPassword is a removal of a password the account does not hold.
var errNoPassword = errors.New(noPasswordHeld)

// removePassword is DELETE /api/v1/me/password.
func (s *PasswordAPI) removePassword(w http.ResponseWriter, r *http.Request, caller Caller) {
	login, ok := sessionUser(w, caller)
	if !ok {
		return
	}
	now := s.now().Truncate(time.Microsecond)
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		a, err := s.readAccount(ctx, wide, login, now, wide.HoldUser)
		switch {
		case err != nil:
			return err
		case !a.exists:
			return db.ErrNoPrincipal
		case a.password.ID == "":
			return errNoPassword
		}
		// The rule a removal of any credential is held to (credentials.go).
		if err := removable(a.policy, a.held, a.password, s.ipAddressed); err != nil {
			return err
		}
		// The TOTP generator goes with the password's row, as the table holds it.
		if err := wide.RemoveCredential(ctx, login, a.password.ID); err != nil {
			return err
		}
		return appendEntries(ctx, wide, removedEntries(login, a.password, a.totp))
	})
	switch {
	case answerRemoval(w, err):
	case errors.Is(err, errNoPassword), errors.Is(err, db.ErrNoCredential):
		fail(w, http.StatusNotFound, noPasswordHeld)
	case errors.Is(err, db.ErrNoPrincipal):
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, noSession)
	case err != nil:
		fail(w, http.StatusInternalServerError, "the password could not be removed")
	default:
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}
}
