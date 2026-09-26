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

	// A TOTP: its secret, sealed under the master key.
	TOTPSealed []byte
}

// AddCredential enrols one.
func (w *Wide) AddCredential(ctx context.Context, c Credential) error {
	var count *int64
	var eligible, state *bool
	if c.Type == CredentialPasskey {
		n := int64(c.SignCount)
		count, eligible, state = &n, &c.BackupEligible, &c.BackupState
	}
	_, err := w.tx.Exec(ctx,
		`insert into credentials (id, login, type, label, public_key, sign_count, aaguid,
		                          backup_eligible, backup_state, password_hash, totp_sealed)
		 values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		c.ID, c.Login, c.Type, nilIfEmpty(c.Label), nilIfNone(c.PublicKey), count, nilIfNone(c.AAGUID),
		eligible, state, nilIfEmpty(c.PasswordHash), nilIfNone(c.TOTPSealed))
	var pg *pgconn.PgError
	switch {
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
	coalesce(password_hash, ''), totp_sealed`

func scanCredential(row pgx.Row) (Credential, error) {
	var c Credential
	var used *time.Time
	var count *int64
	err := row.Scan(&c.ID, &c.Login, &c.Type, &c.Label, &c.CreatedAt, &used,
		&c.PublicKey, &count, &c.AAGUID, &c.BackupEligible, &c.BackupState, &c.PasswordHash, &c.TOTPSealed)
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

// PasskeyUsed records an assertion made with a passkey: the counter and the Backup State it
// reported, and when. Whether the counter went forward is the verifier's to judge before this.
func (w *Wide) PasskeyUsed(ctx context.Context, id string, count uint32, backedUp bool, at time.Time) error {
	tag, err := w.tx.Exec(ctx,
		`update credentials set sign_count = $2, backup_state = $3, last_used_at = $4
		  where id = $1 and type = 'passkey'`, id, int64(count), backedUp, at)
	if err != nil {
		return fmt.Errorf("db: the use of passkey %s could not be recorded: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoCredential
	}
	return nil
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
// Deleting a password is this: the row goes, and nothing is left to blank.
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
