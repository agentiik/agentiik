package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Credentials, one row each: a passkey, a password's hash, a TOTP's sealed secret. Verifying any
// of them is the API's; what is here is what verifying needs, kept and handed back as it was
// written.

// The types of credential, as credentials.type and $defs/credential write them.
const (
	CredentialPasskey  = "passkey"
	CredentialPassword = "password"
	CredentialTOTP     = "totp"
)

// ErrNoCredential is a credential nobody enrolled, or one of somebody else's.
var ErrNoCredential = errors.New("db: no credential of that identifier")

// ErrCredentialExists is a credential ID already enrolled, by anybody, or a second password or
// TOTP for one user.
var ErrCredentialExists = errors.New("db: that credential is enrolled already")

// ErrNoPassword is a TOTP generator enrolled for a user holding no password: "TOTP exists only
// alongside a password".
var ErrNoPassword = errors.New("db: a TOTP generator is enrolled beside a password, and that user holds none")

// Credential is one credential of one user. The fields of its type are set and the others are
// empty.
type Credential struct {
	ID    string
	Login string
	Type  string
	Label string

	CreatedAt  time.Time
	LastUsedAt time.Time

	// A passkey: the COSE public key as the authenticator sent it, the signature counter, the
	// AAGUID as received, and the Backup Eligibility and Backup State flags.
	PublicKey      []byte
	SignCount      uint32
	AAGUID         []byte
	BackupEligible bool
	BackupState    bool

	// A password: its hash, in the self-describing form its hasher writes.
	PasswordHash string

	// A TOTP: its secret, sealed under the master key, and the time step its code was last
	// accepted at, zero where none was, which is before every step a clock reads since 1970.
	TOTPSealed []byte
	TOTPStep   int64
}

// AddCredential enrols one. A TOTP generator is enrolled beside a password alone, and is
// ErrNoPassword for a user holding none; one enrolled with a step, the step of the code that
// confirmed it, accepts no code of that step or of one before it.
//
// Enrolling a TOTP locks its user's password, which a sign-in writes once it holds the user's row:
// its caller holds the user's row first (HoldUser), as every act on an account does, or the two wait
// on each other.
func (w *Wide) AddCredential(ctx context.Context, c Credential) error {
	var count *int64
	var eligible, state *bool
	if c.Type == CredentialPasskey {
		n := int64(c.SignCount)
		count, eligible, state = &n, &c.BackupEligible, &c.BackupState
	}
	var step *int64
	if c.TOTPStep != 0 {
		step = &c.TOTPStep
	}
	_, err := w.tx.Exec(ctx,
		`insert into credentials (id, login, type, label, public_key, sign_count, aaguid,
		                          backup_eligible, backup_state, password_hash, totp_sealed, totp_step)
		 values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		c.ID, c.Login, c.Type, nilIfEmpty(c.Label), nilIfNone(c.PublicKey), count, nilIfNone(c.AAGUID),
		eligible, state, nilIfEmpty(c.PasswordHash), nilIfNone(c.TOTPSealed), step)
	var pg *pgconn.PgError
	switch {
	case errors.As(err, &pg) && pg.ConstraintName == "credentials_totp_beside_a_password":
		return fmt.Errorf("%w: %s", ErrNoPassword, c.Login)
	case errors.As(err, &pg) && pg.Code == uniqueViolation:
		return fmt.Errorf("%w: %s", ErrCredentialExists, c.ID)
	case errors.As(err, &pg) && pg.Code == foreignKeyViolation:
		return fmt.Errorf("%w: %s", ErrNoPrincipal, c.Login)
	case err != nil:
		return fmt.Errorf("db: credential %s could not be enrolled: %w", c.ID, err)
	}
	return nil
}

func nilIfNone(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

const credentialColumns = `id, login, type, coalesce(label, ''), created_at, last_used_at,
	public_key, sign_count, aaguid, coalesce(backup_eligible, false), coalesce(backup_state, false),
	coalesce(password_hash, ''), totp_sealed, coalesce(totp_step, 0)`

func scanCredential(row pgx.Row) (Credential, error) {
	var c Credential
	var used *time.Time
	var count *int64
	err := row.Scan(&c.ID, &c.Login, &c.Type, &c.Label, &c.CreatedAt, &used,
		&c.PublicKey, &count, &c.AAGUID, &c.BackupEligible, &c.BackupState, &c.PasswordHash, &c.TOTPSealed, &c.TOTPStep)
	if used != nil {
		c.LastUsedAt = *used
	}
	if count != nil {
		c.SignCount = uint32(*count)
	}
	return c, err
}

// Credential reads one by its identifier, which for a passkey is the credential ID an assertion
// names.
func (w *Wide) Credential(ctx context.Context, id string) (Credential, error) {
	c, err := scanCredential(w.tx.QueryRow(ctx, `select `+credentialColumns+` from credentials where id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, ErrNoCredential
	}
	if err != nil {
		return Credential{}, fmt.Errorf("db: credential %s could not be read: %w", id, err)
	}
	return c, nil
}

// CredentialsOf answers a user's, in the order they were enrolled.
func (w *Wide) CredentialsOf(ctx context.Context, login string) ([]Credential, error) {
	rows, err := w.tx.Query(ctx,
		`select `+credentialColumns+` from credentials where login = $1 order by created_at, id`, login)
	if err != nil {
		return nil, fmt.Errorf("db: the credentials of %s could not be read: %w", login, err)
	}
	all, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Credential, error) { return scanCredential(row) })
	if err != nil {
		return nil, fmt.Errorf("db: the credentials of %s could not be read: %w", login, err)
	}
	return all, nil
}

// ErrSignCountBehind is an assertion whose signature counter did not move past the one recorded,
// which is what a cloned authenticator's does. Two authenticators that count nothing both report
// zero, and are not refused for it.
var ErrSignCountBehind = errors.New("db: that passkey's signature counter did not move forward")

// PasskeyUsed records an assertion made with a passkey: the counter and the Backup State it
// reported, and when. A counter that did not move past the recorded one is ErrSignCountBehind and
// nothing is recorded, checked in the statement that writes it, so that of two assertions verified
// against one reading the second is still refused.
func (w *Wide) PasskeyUsed(ctx context.Context, id string, count uint32, backedUp bool, at time.Time) error {
	tag, err := w.tx.Exec(ctx,
		`update credentials set sign_count = $2, backup_state = $3, last_used_at = $4
		  where id = $1 and type = 'passkey' and ($2 > sign_count or ($2 = 0 and sign_count = 0))`,
		id, int64(count), backedUp, at)
	if err != nil {
		return fmt.Errorf("db: the use of passkey %s could not be recorded: %w", id, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var passkey bool
	if err := w.tx.QueryRow(ctx,
		`select exists (select from credentials where id = $1 and type = 'passkey')`, id).Scan(&passkey); err != nil {
		return fmt.Errorf("db: passkey %s could not be read: %w", id, err)
	}
	if passkey {
		return ErrSignCountBehind
	}
	return ErrNoCredential
}

// ErrTOTPSpent is a TOTP code of a step at or before the one a code was last accepted at: accepted
// once already, or older than one that was.
var ErrTOTPSpent = errors.New("db: a code of that step, or of a later one, was accepted already")

// TOTPUsed records a TOTP code accepted at step, and when: the step is kept, so that no code of it
// or of a step before it is accepted again. One at or before the step recorded is ErrTOTPSpent and
// nothing is recorded, checked in the statement that writes it, so that of two sign-ins with one
// code, verified against one reading, the second is still refused.
func (w *Wide) TOTPUsed(ctx context.Context, id string, step int64, at time.Time) error {
	tag, err := w.tx.Exec(ctx,
		`update credentials set totp_step = $2, last_used_at = $3
		  where id = $1 and type = 'totp' and (totp_step is null or totp_step < $2)`,
		id, step, at)
	if err != nil {
		return fmt.Errorf("db: the use of TOTP %s could not be recorded: %w", id, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var totp bool
	if err := w.tx.QueryRow(ctx,
		`select exists (select from credentials where id = $1 and type = 'totp')`, id).Scan(&totp); err != nil {
		return fmt.Errorf("db: TOTP %s could not be read: %w", id, err)
	}
	if totp {
		return ErrTOTPSpent
	}
	return ErrNoCredential
}

// CredentialUsed records when a password or a TOTP was last used.
func (w *Wide) CredentialUsed(ctx context.Context, id string, at time.Time) error {
	tag, err := w.tx.Exec(ctx, `update credentials set last_used_at = $2 where id = $1`, id, at)
	if err != nil {
		return fmt.Errorf("db: the use of credential %s could not be recorded: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoCredential
	}
	return nil
}

// RemoveCredential deletes one of a user's credentials, and with it every session it opened.
// Deleting a password is this: the row goes, and nothing is left to blank, and the user's TOTP
// generator goes with it, since a TOTP exists only alongside a password.
func (w *Wide) RemoveCredential(ctx context.Context, login, id string) error {
	tag, err := w.tx.Exec(ctx, `delete from credentials where login = $1 and id = $2`, login, id)
	if err != nil {
		return fmt.Errorf("db: credential %s could not be removed: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoCredential
	}
	return nil
}

// SetPassword sets login's password to hash at at, and answers it as recorded and whether it
// replaced one. A password held is replaced in its row, which keeps its identifier, and so the
// sessions it opened, which its caller ends or keeps, and the TOTP generator beside it, which goes
// wherever the password's row goes; it is recorded as set at at and used by nobody since, as
// $defs/passwordCredential's created_at is "when the password was last set". Where none is held,
// one is enrolled under id.
//
// Its caller holds the user's row first (HoldUser), as every act on an account does, so that two
// settings at once take turns rather than both enrolling one.
func (w *Wide) SetPassword(ctx context.Context, login, id, hash string, at time.Time) (Credential, bool, error) {
	c, err := scanCredential(w.tx.QueryRow(ctx,
		`update credentials set password_hash = $2, created_at = $3, last_used_at = null
		  where login = $1 and type = 'password'
		 returning `+credentialColumns, login, hash, at))
	switch {
	case err == nil:
		return c, true, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return Credential{}, false, fmt.Errorf("db: the password of %s could not be set: %w", login, err)
	}
	c, err = scanCredential(w.tx.QueryRow(ctx,
		`insert into credentials (id, login, type, password_hash, created_at) values ($1, $2, 'password', $3, $4)
		 returning `+credentialColumns, id, login, hash, at))
	var pg *pgconn.PgError
	switch {
	case errors.As(err, &pg) && pg.Code == uniqueViolation:
		return Credential{}, false, fmt.Errorf("%w: %s", ErrCredentialExists, id)
	case errors.As(err, &pg) && pg.Code == foreignKeyViolation:
		return Credential{}, false, fmt.Errorf("%w: %s", ErrNoPrincipal, login)
	case err != nil:
		return Credential{}, false, fmt.Errorf("db: the password of %s could not be set: %w", login, err)
	}
	return c, false, nil
}

// EndSessionsOpenedBy revokes at at the live sessions of login that credential opened, but the one
// whose identifier hashes to kept where it is given, and answers how many it revoked: what a
// password set anew ends, since whoever knew the one before may be who holds them, and the person
// setting it keeps the session they set it from.
func (w *Wide) EndSessionsOpenedBy(ctx context.Context, login, credential string, kept []byte, at time.Time) (int, error) {
	tag, err := w.tx.Exec(ctx,
		`update sessions set revoked_at = $4
		  where login = $1 and credential = $2 and revoked_at is null and ($3::bytea is null or hash <> $3)`,
		login, credential, nilIfNone(kept), at)
	if err != nil {
		return 0, fmt.Errorf("db: the sessions credential %s opened could not be ended: %w", credential, err)
	}
	return int(tag.RowsAffected()), nil
}

// TOTPEnrolmentLife is how long a TOTP generator waits for the code that confirms it: ten minutes,
// the time a person takes to find their phone, open an authenticator application and scan, as the
// table's constraint holds it.
const TOTPEnrolmentLife = 10 * time.Minute

// ErrNoTOTPEnrolment is a TOTP generator nobody started, or one whose minutes are up.
var ErrNoTOTPEnrolment = errors.New("db: no TOTP generator is waiting for its first code")

// TOTPEnrolment is a TOTP generator started and not confirmed: its secret, sealed as the row of
// credentials it becomes will hold it, under the identifier it will be enrolled with. It signs
// nobody in and asks no sign-in for a code until its first code confirms it.
type TOTPEnrolment struct {
	Login     string
	ID        string
	Sealed    []byte
	StartedAt time.Time
	ExpiresAt time.Time
}

// StartTOTP keeps a generator started, in place of the one its user had started before, whose
// secret was shown and may never have reached an application.
func (w *Wide) StartTOTP(ctx context.Context, e TOTPEnrolment) error {
	_, err := w.tx.Exec(ctx,
		`insert into totp_enrolments (login, id, totp_sealed, started_at, expires_at) values ($1, $2, $3, $4, $5)
		 on conflict (login) do update
		   set id = excluded.id, totp_sealed = excluded.totp_sealed,
		       started_at = excluded.started_at, expires_at = excluded.expires_at`,
		e.Login, e.ID, e.Sealed, e.StartedAt, e.ExpiresAt)
	var pg *pgconn.PgError
	switch {
	case errors.As(err, &pg) && pg.Code == foreignKeyViolation:
		return fmt.Errorf("%w: %s", ErrNoPrincipal, e.Login)
	case err != nil:
		return fmt.Errorf("db: a TOTP generator of %s could not be started: %w", e.Login, err)
	}
	return nil
}

// TOTPEnrolmentOf answers the generator login started, if it is still waiting at now.
func (w *Wide) TOTPEnrolmentOf(ctx context.Context, login string, now time.Time) (TOTPEnrolment, error) {
	var e TOTPEnrolment
	err := w.tx.QueryRow(ctx,
		`select login, id, totp_sealed, started_at, expires_at from totp_enrolments where login = $1 and expires_at > $2`,
		login, now).Scan(&e.Login, &e.ID, &e.Sealed, &e.StartedAt, &e.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TOTPEnrolment{}, ErrNoTOTPEnrolment
	}
	if err != nil {
		return TOTPEnrolment{}, fmt.Errorf("db: the TOTP generator %s started could not be read: %w", login, err)
	}
	return e, nil
}

// EndTOTPEnrolment forgets the generator login started under id, once confirmed; one started since
// under another identifier is kept.
func (w *Wide) EndTOTPEnrolment(ctx context.Context, login, id string) error {
	if _, err := w.tx.Exec(ctx, `delete from totp_enrolments where login = $1 and id = $2`, login, id); err != nil {
		return fmt.Errorf("db: the TOTP generator %s started could not be forgotten: %w", login, err)
	}
	return nil
}

// SpentFirstAdministratorLink says whether login enrolled with a first administrator's link: the
// one the bootstrap token issues, whose enrolment ends it. A link spent by a password where the
// policy requires a passkey leaves the bootstrap to the first passkey its user then registers, which
// is how that registration knows it is the first administrator's.
func (w *Wide) SpentFirstAdministratorLink(ctx context.Context, login string) (bool, error) {
	var spent bool
	if err := w.tx.QueryRow(ctx,
		`select exists (select from enrolment_codes where login = $1 and kind = 'first-administrator' and used_at is not null)`,
		login).Scan(&spent); err != nil {
		return false, fmt.Errorf("db: the enrolment links of %s could not be read: %w", login, err)
	}
	return spent, nil
}
