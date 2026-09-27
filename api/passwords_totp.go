package api

import (
	"context"
	"encoding/base32"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/totp"
	"github.com/agentiik/agentiik/internal/ulid"
)

// Enrolling a TOTP generator beside the password, from a browser's full session, where the policy
// that applies to the account allows passwords: "TOTP exists only alongside a password", as the
// second factor of a password sign-in and of nothing else.
//
// POST /api/v1/me/totp mints the generator's secret and answers it once, with the otpauth:// URI an
// authenticator application scans, for the sign-in page to show as text and as a QR code. The
// generator is not the account's yet: it waits in a table of its own, ten minutes at most, and
// asks no sign-in for a code, since a secret that never reached an application would otherwise lock
// its holder out. POST /api/v1/me/totp/confirm, with a code the application then shows, proves it
// did, and enrols the generator, recording the step of that code, so that no code of it or before it
// signs anybody in afterwards. DELETE /api/v1/me/totp removes it, with a code it shows now, so that a
// session left open does not take a second factor away without the device that holds it; a wrong
// code is counted as a guess at a sign-in is.
//
// A generator's secret is 160 bits, the length RFC 4226 recommends and every authenticator
// application reads, sealed under the master key bound to its user and its identifier as the row of
// credentials it becomes will hold it.

// totpSecretBytes is the length of a generator's secret: 160 bits, RFC 4226's recommendation, the
// length of HMAC-SHA-1's own output.
const totpSecretBytes = 20

// totpIssuer is the issuer an authenticator application lists a generator under.
const totpIssuer = "Agentiik"

// The sentences enrolling and removing a TOTP generator are refused with.
const (
	// totpNeedsAPassword is a generator started or confirmed for an account holding no password.
	totpNeedsAPassword = "a TOTP generator is enrolled beside a password, as the second factor of a password sign-in, and this account holds none: set a password first"

	// totpHeld is a generator started or confirmed for an account holding one.
	totpHeld = "this account holds a TOTP generator already: remove it, with a code it shows, before enrolling another"

	// totpNotStarted is a confirmation with no generator waiting for it.
	totpNotStarted = "no TOTP generator is waiting for its first code: none was started, it was started again since, or its ten minutes are up. Start again"

	// totpNotShown is a confirmation with a code the generator started does not show.
	totpNotShown = "that code is not the one the generator shows now: check that the device's clock is right, and send the code it shows next"

	// totpNotHeld is a removal from an account holding no generator.
	totpNotHeld = "this account holds no TOTP generator"

	// totpRemoveMismatch is a removal with a code the generator does not show now, or one
	// accepted already.
	totpRemoveMismatch = "that code is not one the generator shows now, or it was accepted already, and the generator is left as it was"

	// totpChanged is a generator enrolled or removed by another request at the same moment.
	totpChanged = "the TOTP generator was enrolled or removed by another request while this one was checked: try again"

	// noMasterKey is a generator asked of an installation that seals no secret.
	noMasterKey = "this installation holds no master key to seal a TOTP generator's secret under"
)

// totpCode is the body of a confirmation and of a removal: one code of the generator.
type totpCode struct {
	TOTP string
}

func (q *totpCode) field(b *body, name string) error {
	if name == "totp" {
		return text(b, &q.TOTP)
	}
	return unknown(name)
}

// check refuses a code that is not six digits, and never repeats it.
func (q totpCode) check() error {
	if !sixDigits(q.TOTP) {
		return fmt.Errorf("totp: a TOTP code is %d digits, as the generator shows it", totp.Digits)
	}
	return nil
}

// TOTPStarted is what starting a generator answers, once: its identifier, its secret in base32 as
// an authenticator application takes it typed in, the otpauth:// URI it takes scanned, and when it
// stops waiting for its first code.
type TOTPStarted struct {
	ID        string    `json:"id"`
	Secret    string    `json:"secret"`
	URI       string    `json:"uri"`
	ExpiresAt time.Time `json:"expires_at"`
}

// base32Secret is how a secret is written for a person and in a URI: RFC 4648's alphabet, with no
// padding, which Google's Key Uri Format asks for.
var base32Secret = base32.StdEncoding.WithPadding(base32.NoPadding)

// otpauth is the Key Uri Format of a generator: the issuer and the account, login@host, so that two
// installations' generators are told apart in one application, and every parameter at the value
// every application assumes, written all the same for those that read them. The format allows no
// colon in the account, which it reads as the one between the issuer and the account, so an
// installation addressed by an IPv6 address names the login alone.
func (s *PasswordAPI) otpauth(login string, secret []byte) string {
	account := login + "@" + s.host
	if strings.Contains(s.host, ":") {
		account = login
	}
	label := totpIssuer + ":" + url.PathEscape(account)
	q := url.Values{
		"secret":    {base32Secret.EncodeToString(secret)},
		"issuer":    {totpIssuer},
		"algorithm": {"SHA1"},
		"digits":    {strconv.Itoa(totp.Digits)},
		"period":    {strconv.Itoa(int(totp.Step / time.Second))},
	}
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// totpAllowed says why login may not enrol a generator now, as the status and sentence to answer
// with, and 0 where they may: passwords allowed by the policy that applies to them, a password held,
// and no generator held.
func totpAllowed(a account) (int, string) {
	switch {
	case a.policy.passwordsForbidden:
		return http.StatusForbidden, passwordsForbiddenToSet
	case a.password.ID == "":
		return http.StatusConflict, totpNeedsAPassword
	case a.totp.ID != "":
		return http.StatusConflict, totpHeld
	}
	return 0, ""
}

// answerAllowed answers a refusal totpAllowed said, naming the setting where it is the policy's.
func answerAllowed(w http.ResponseWriter, status int, why string) {
	if why == passwordsForbiddenToSet {
		failSetting(w, status, why, passwordSetting)
		return
	}
	fail(w, status, why)
}

// startTOTP is POST /api/v1/me/totp.
func (s *PasswordAPI) startTOTP(w http.ResponseWriter, r *http.Request, caller Caller) {
	login, ok := sessionUser(w, caller)
	if !ok {
		return
	}
	// A generator is a factor that outlives the session, which the session alone does not give:
	// see proofLife.
	if !provedSince(caller.ProvedAt, s.now()) {
		askAgain(w)
		return
	}
	if s.totp == nil {
		s.report(errors.New("a TOTP generator could not be started: no master key is attached"))
		fail(w, http.StatusInternalServerError, noMasterKey)
		return
	}
	now := s.now().Truncate(time.Microsecond)
	secret, err := randomBytes(totpSecretBytes)
	if err != nil {
		fail(w, http.StatusInternalServerError, "the TOTP generator could not be started")
		return
	}
	defer clear(secret)
	id := ulid.New()
	sealed, err := s.totp.SealTOTP(login, id, secret)
	if err != nil {
		s.report(fmt.Errorf("the TOTP generator of %s could not be sealed: %w", login, err))
		fail(w, http.StatusInternalServerError, "the TOTP generator could not be started")
		return
	}
	started := db.TOTPEnrolment{Login: login, ID: id, Sealed: sealed, StartedAt: now, ExpiresAt: now.Add(db.TOTPEnrolmentLife)}
	var status int
	var why string
	err = s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		a, err := s.readAccount(ctx, wide, login, now, wide.HoldUser)
		switch {
		case err != nil:
			return err
		case !a.exists:
			return db.ErrNoPrincipal
		}
		if status, why = totpAllowed(a); status != 0 {
			return nil
		}
		return wide.StartTOTP(ctx, started)
	})
	switch {
	case errors.Is(err, db.ErrNoPrincipal):
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, noSession)
	case err != nil:
		fail(w, http.StatusInternalServerError, "the TOTP generator could not be started")
	case status != 0:
		answerAllowed(w, status, why)
	default:
		shownOnce(w, http.StatusOK, TOTPStarted{
			ID: id, Secret: base32Secret.EncodeToString(secret), URI: s.otpauth(login, secret), ExpiresAt: started.ExpiresAt.UTC(),
		})
	}
}

// confirmTOTP is POST /api/v1/me/totp/confirm.
//
// The generator waiting is read, and its code checked, outside the transaction that enrols it,
// which reads it again under the user's row and enrols it only where it is the one checked and the
// account may still hold it.
func (s *PasswordAPI) confirmTOTP(w http.ResponseWriter, r *http.Request, caller Caller) {
	login, ok := sessionUser(w, caller)
	if !ok {
		return
	}
	var ask totpCode
	if err := readAtMost(r, &ask, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	if err := ask.check(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.totp == nil {
		s.report(errors.New("a TOTP generator could not be confirmed: no master key is attached"))
		fail(w, http.StatusInternalServerError, noMasterKey)
		return
	}
	now := s.now().Truncate(time.Microsecond)
	var waiting db.TOTPEnrolment
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		var err error
		waiting, err = wide.TOTPEnrolmentOf(ctx, login, now)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoTOTPEnrolment):
		fail(w, http.StatusConflict, totpNotStarted)
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the TOTP generator could not be confirmed")
		return
	case !provedSince(caller.ProvedAt, waiting.StartedAt):
		// Proved within proofLife of the start, as the start asked, or since.
		askAgain(w)
		return
	}
	secret, err := s.totp.OpenTOTP(login, waiting.ID, waiting.Sealed)
	if err != nil {
		s.report(fmt.Errorf("the TOTP generator %s of %s could not be opened: %w", waiting.ID, login, err))
		fail(w, http.StatusInternalServerError, "the TOTP generator could not be confirmed")
		return
	}
	step, matched := totp.Match(secret, ask.TOTP, now, 0)
	clear(secret)
	if !matched {
		fail(w, http.StatusUnprocessableEntity, totpNotShown)
		return
	}
	s.betweenChecks()

	var status int
	var why string
	var enrolled db.Credential
	err = s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		a, err := s.readAccount(ctx, wide, login, now, wide.HoldUser)
		switch {
		case err != nil:
			return err
		case !a.exists:
			return db.ErrNoPrincipal
		}
		if status, why = totpAllowed(a); status != 0 {
			return nil
		}
		still, err := wide.TOTPEnrolmentOf(ctx, login, now)
		if errors.Is(err, db.ErrNoTOTPEnrolment) || (err == nil && still.ID != waiting.ID) {
			status, why = http.StatusConflict, totpNotStarted
			return nil
		}
		if err != nil {
			return err
		}
		if err := wide.AddCredential(ctx, db.Credential{
			ID: waiting.ID, Login: login, Type: db.CredentialTOTP, TOTPSealed: waiting.Sealed, TOTPStep: step,
		}); err != nil {
			return err
		}
		if err := wide.EndTOTPEnrolment(ctx, login, waiting.ID); err != nil {
			return err
		}
		// Read back, since when it was enrolled is the database's to say.
		if enrolled, err = wide.Credential(ctx, waiting.ID); err != nil {
			return err
		}
		return wide.Audit(ctx, audit.Record{
			Actor: login, Action: audit.CredentialEnrol, Target: waiting.ID, Result: audit.Done,
			Detail: map[string]any{"type": db.CredentialTOTP},
		})
	})
	switch {
	case errors.Is(err, db.ErrNoPrincipal):
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, noSession)
	case errors.Is(err, db.ErrNoPassword):
		fail(w, http.StatusConflict, totpNeedsAPassword)
	case errors.Is(err, db.ErrCredentialExists):
		fail(w, http.StatusConflict, totpChanged)
	case err != nil:
		fail(w, http.StatusInternalServerError, "the TOTP generator could not be confirmed")
	case status != 0:
		answerAllowed(w, status, why)
	default:
		shownOnce(w, http.StatusOK, heldOf(enrolled))
	}
}

// removeTOTP is DELETE /api/v1/me/totp.
//
// The code is a guess at the generator like a sign-in's, counted against the account and the
// address before the secret is opened; it is checked outside the transaction that removes the
// generator, which records its step first, so that of two removals with one code, or a removal and a
// sign-in, one is refused, and a generator removed or replaced since it was checked is refused too.
func (s *PasswordAPI) removeTOTP(w http.ResponseWriter, r *http.Request, caller Caller) {
	login, ok := sessionUser(w, caller)
	if !ok {
		return
	}
	var ask totpCode
	if err := readAtMost(r, &ask, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	if err := ask.check(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
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
		fail(w, http.StatusInternalServerError, "the TOTP generator could not be removed")
		return
	case !a.exists:
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, noSession)
		return
	case a.totp.ID == "":
		fail(w, http.StatusNotFound, totpNotHeld)
		return
	case s.totp == nil:
		s.report(errors.New("a TOTP generator could not be removed: no master key is attached"))
		fail(w, http.StatusInternalServerError, noMasterKey)
		return
	}
	address := s.signIns.addressOf(r)
	wait, took := s.attempts.take(login, address, now)
	if !took {
		w.Header().Set("Retry-After", strconv.Itoa(int((wait+time.Second-1)/time.Second)))
		fail(w, http.StatusTooManyRequests, tooManyAttempts)
		return
	}
	secret, err := s.totp.OpenTOTP(login, a.totp.ID, a.totp.TOTPSealed)
	if err != nil {
		s.attempts.forgive(login, address, now)
		s.report(fmt.Errorf("the TOTP generator %s of %s could not be opened: %w", a.totp.ID, login, err))
		fail(w, http.StatusInternalServerError, "the TOTP generator could not be removed")
		return
	}
	step, matched := totp.Match(secret, ask.TOTP, now, a.totp.TOTPStep)
	clear(secret)
	if !matched {
		fail(w, http.StatusForbidden, totpRemoveMismatch)
		return
	}
	s.betweenChecks()

	err = s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		// The user's row first, as every act on an account takes it; the generator checked is then
		// there still, or recording its step is ErrNoCredential.
		if _, err := wide.HoldUser(ctx, login); err != nil {
			return err
		}
		if err := wide.TOTPUsed(ctx, a.totp.ID, step, now); err != nil {
			return err
		}
		if err := wide.RemoveCredential(ctx, login, a.totp.ID); err != nil {
			return err
		}
		return wide.Audit(ctx, audit.Record{
			Actor: login, Action: audit.CredentialRemove, Target: a.totp.ID, Result: audit.Done,
			Detail: map[string]any{"type": db.CredentialTOTP},
		})
	})
	switch {
	case errors.Is(err, db.ErrTOTPSpent):
		fail(w, http.StatusForbidden, totpRemoveMismatch)
	case errors.Is(err, db.ErrNoCredential):
		s.attempts.forgive(login, address, now)
		fail(w, http.StatusConflict, totpChanged)
	case errors.Is(err, db.ErrNoPrincipal):
		s.attempts.forgive(login, address, now)
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, noSession)
	case err != nil:
		s.attempts.forgive(login, address, now)
		fail(w, http.StatusInternalServerError, "the TOTP generator could not be removed")
	default:
		// A right code gives back its own attempt and no other: the generator may be one whoever
		// holds the session enrolled, whose codes would otherwise start the account's count again
		// between guesses at its password.
		s.attempts.forgive(login, address, now)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}
}
