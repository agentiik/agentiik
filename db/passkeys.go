package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/internal/ulid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// What the passkey ceremonies keep between the options and the verification, and what a
// verification writes beside the credential: the handle a user's passkeys are registered under,
// the challenges, and what a sign-in refused for a passkey's counter tells its user. Verifying is
// the API's, with internal/webauthn; what is here is what it needs kept.

// The ceremonies, as webauthn_challenges.ceremony and openapi.json's ceremony write them.
const (
	CeremonyRegistration = "registration"
	CeremonyAssertion    = "assertion"
)

// ChallengeLife is how long a challenge is taken: five minutes, the time a person takes to find
// an authenticator and unlock it, and short enough that one copied off a screen is dead by the
// time anybody could use it. The table holds it too.
const ChallengeLife = 5 * time.Minute

// ErrNoChallenge is a challenge that opens nothing: never issued, taken already, or past its
// minutes.
var ErrNoChallenge = errors.New("db: no open challenge of that value")

// Challenge is a challenge the options issued, and what it was issued for.
type Challenge struct {
	Value    []byte
	Ceremony string

	// Login is the user a registration enrols, and empty for an assertion, which names nobody
	// until the passkey does.
	Login string

	// EnrolmentCode is the SHA-256 of the code that let a registration start where no session
	// did, which the verification spends; nil for one a session started, and for an assertion.
	EnrolmentCode []byte

	IssuedAt  time.Time
	ExpiresAt time.Time
}

// lapsedSwept is how many challenges past their minutes issuing one removes at most: more than
// one, so that the sweep outruns what lapses, since every challenge lapses once and is issued once.
const lapsedSwept = 16

// IssueChallenge keeps a challenge until it is taken or its minutes pass, and removes some of those
// whose minutes passed before c was issued, so that the table holds what the last few minutes of
// ceremonies started and little older.
//
// The removal skips a row another transaction holds rather than waiting for it: two ceremonies
// started at once each find the same lapsed rows, and one waiting on the other's removal, or the
// two taking them in two orders, would make ceremonies anybody may start wait on each other.
func (w *Wide) IssueChallenge(ctx context.Context, c Challenge) error {
	if _, err := w.tx.Exec(ctx,
		`delete from webauthn_challenges
		  where challenge in (select challenge from webauthn_challenges where expires_at <= $1
		                       limit $2 for update skip locked)`, c.IssuedAt, lapsedSwept); err != nil {
		return fmt.Errorf("db: the challenges past their minutes could not be removed: %w", err)
	}
	_, err := w.tx.Exec(ctx,
		`insert into webauthn_challenges (challenge, ceremony, login, enrolment_code, issued_at, expires_at)
		 values ($1, $2, $3, $4, $5, $6)`,
		c.Value, c.Ceremony, nilIfEmpty(c.Login), nilIfNone(c.EnrolmentCode), c.IssuedAt, c.ExpiresAt)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == foreignKeyViolation {
		if pg.ConstraintName == "webauthn_challenges_login_fkey" {
			return fmt.Errorf("%w: %s", ErrNoPrincipal, c.Login)
		}
		return fmt.Errorf("%w: it is not %s's", ErrNoEnrolmentCode, c.Login)
	}
	if err != nil {
		return fmt.Errorf("db: a challenge could not be issued: %w", err)
	}
	return nil
}

// TakeChallenge answers the challenge value names, if it is open at now, and removes it in the
// same statement: of two verifications presenting it at once, one takes it and the other is
// answered ErrNoChallenge.
func (w *Wide) TakeChallenge(ctx context.Context, value []byte, now time.Time) (Challenge, error) {
	var c Challenge
	var login *string
	err := w.tx.QueryRow(ctx,
		`delete from webauthn_challenges where challenge = $1 and expires_at > $2
		 returning challenge, ceremony, login, enrolment_code, issued_at, expires_at`, value, now,
	).Scan(&c.Value, &c.Ceremony, &login, &c.EnrolmentCode, &c.IssuedAt, &c.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Challenge{}, ErrNoChallenge
	}
	if err != nil {
		return Challenge{}, fmt.Errorf("db: a challenge could not be taken: %w", err)
	}
	if login != nil {
		c.Login = *login
	}
	return c, nil
}

// PasskeyHandle answers the handle login's passkeys are registered under, keeping fresh as that
// handle where the user has none yet, and the one kept where they have: it is minted once, on the
// first registration started for them, and never changes, since every passkey registered under it
// hands it back. A login no user has is ErrNoPrincipal.
func (w *Wide) PasskeyHandle(ctx context.Context, login string, fresh []byte) ([]byte, error) {
	var handle []byte
	err := w.tx.QueryRow(ctx,
		`update users set webauthn_handle = coalesce(webauthn_handle, $2) where login = $1
		 returning webauthn_handle`, login, fresh).Scan(&handle)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNoPrincipal, login)
	}
	if err != nil {
		return nil, fmt.Errorf("db: the passkey handle of %s could not be kept: %w", login, err)
	}
	return handle, nil
}

// PasskeyHandleOf answers the handle login's passkeys are registered under, and nil where no
// registration was ever started for them. A login no user has is ErrNoPrincipal.
func (w *Wide) PasskeyHandleOf(ctx context.Context, login string) ([]byte, error) {
	var handle []byte
	err := w.tx.QueryRow(ctx, `select webauthn_handle from users where login = $1`, login).Scan(&handle)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNoPrincipal, login)
	}
	if err != nil {
		return nil, fmt.Errorf("db: the passkey handle of %s could not be read: %w", login, err)
	}
	return handle, nil
}

// UserVerificationRequired says whether any authentication policy of the installation requires
// user verification, its own or a namespace's: what an assertion's options ask for, since they are
// issued before the passkey names whose policy applies.
func (w *Wide) UserVerificationRequired(ctx context.Context) (bool, error) {
	var required bool
	if err := w.tx.QueryRow(ctx,
		`select coalesce(bool_or(user_verification = 'required'), false) from auth_policy`).Scan(&required); err != nil {
		return false, fmt.Errorf("db: whether a policy requires user verification could not be read: %w", err)
	}
	return required, nil
}

// TellPasskeyRefused tells login, in their GET /api/v1/me, that a sign-in with their passkey
// credential was refused at at because its signature counter did not move forward: the wire's
// passkey_counter_refused. The passkey is named by its credential ID and not referred to, so that
// removing it, which is what its user may do on reading this, leaves what they were told.
func (w *Wide) TellPasskeyRefused(ctx context.Context, login, credential string, at time.Time) error {
	if _, err := w.tx.Exec(ctx,
		`insert into notifications (id, recipient, kind, at, credential)
		 values ($1, $2, 'passkey_counter_refused', $3, $4)`,
		ulid.New(), login, at, credential); err != nil {
		return fmt.Errorf("db: %s could not be told of the refusal of passkey %s: %w", login, credential, err)
	}
	return nil
}
