package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// API tokens, console sessions and enrolment codes: what a request, a browser or an enrolment link
// presents, each kept as the SHA-256 of its value, which the caller computes. A lookup answers
// only one that still opens something, so that a caller who forgot to check an expiry or a
// revocation is handed nothing to forget it with.

// ErrNoToken is a token that opens nothing: never minted, revoked or expired.
var ErrNoToken = errors.New("db: no live token of that value")

// APIToken is a bearer credential of a user or a service account, without its value.
type APIToken struct {
	ID        string
	Hash      []byte
	Principal string

	// Permissions and Within narrow the token, and nil does not narrow it that way.
	Permissions []string
	Within      []string

	DeviceLabel string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	LastUsedAt  time.Time
	RevokedAt   time.Time
}

// MintToken writes a token for a principal, which is a user or a service account. CreatedAt and
// ExpiresAt are the caller's, read off one clock, since the table refuses an expiry more than a
// year after creation.
func (w *Wide) MintToken(ctx context.Context, t APIToken) error {
	tag, err := w.tx.Exec(ctx,
		`insert into api_tokens (id, hash, principal, principal_kind, scope_permissions, scope_within,
		                         device_label, created_at, expires_at)
		 select $1, $2, id, kind, $4, $5, $6, $7, $8 from principals where id = $3`,
		t.ID, t.Hash, t.Principal, t.Permissions, t.Within, nilIfEmpty(t.DeviceLabel), t.CreatedAt, t.ExpiresAt)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.ConstraintName == "api_tokens_principal_kind_check" {
		return fmt.Errorf("db: %s is a group, which holds no token", t.Principal)
	}
	if err != nil {
		return fmt.Errorf("db: a token of %s could not be minted: %w", t.Principal, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrNoPrincipal, t.Principal)
	}
	return nil
}

const tokenColumns = `id, hash, principal, scope_permissions, scope_within, coalesce(device_label, ''),
	created_at, expires_at, last_used_at, revoked_at`

func scanToken(row pgx.Row) (APIToken, error) {
	var t APIToken
	var used, revoked *time.Time
	err := row.Scan(&t.ID, &t.Hash, &t.Principal, &t.Permissions, &t.Within, &t.DeviceLabel,
		&t.CreatedAt, &t.ExpiresAt, &used, &revoked)
	if used != nil {
		t.LastUsedAt = *used
	}
	if revoked != nil {
		t.RevokedAt = *revoked
	}
	return t, err
}

// TokenByHash answers the token whose value hashes to hash, if it is neither revoked nor expired
// at now.
func (w *Wide) TokenByHash(ctx context.Context, hash []byte, now time.Time) (APIToken, error) {
	t, err := scanToken(w.tx.QueryRow(ctx,
		`select `+tokenColumns+` from api_tokens
		  where hash = $1 and revoked_at is null and expires_at > $2`, hash, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return APIToken{}, ErrNoToken
	}
	if err != nil {
		return APIToken{}, fmt.Errorf("db: a token could not be read: %w", err)
	}
	return t, nil
}

// TokenUsed records the last request a token authenticated.
func (w *Wide) TokenUsed(ctx context.Context, id string, at time.Time) error {
	if _, err := w.tx.Exec(ctx, `update api_tokens set last_used_at = $2 where id = $1`, id, at); err != nil {
		return fmt.Errorf("db: the use of token %s could not be recorded: %w", id, err)
	}
	return nil
}

// TokensOf answers a principal's, revoked and expired ones included, newest first, so that a
// listing shows what was minted and what became of it.
func (w *Wide) TokensOf(ctx context.Context, principal string) ([]APIToken, error) {
	rows, err := w.tx.Query(ctx,
		`select `+tokenColumns+` from api_tokens where principal = $1 order by created_at desc, id`, principal)
	if err != nil {
		return nil, fmt.Errorf("db: the tokens of %s could not be read: %w", principal, err)
	}
	all, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (APIToken, error) { return scanToken(row) })
	if err != nil {
		return nil, fmt.Errorf("db: the tokens of %s could not be read: %w", principal, err)
	}
	return all, nil
}

// RevokeToken revokes one of a principal's tokens, and answers whether it was live until now. A
// token of another principal is ErrNoToken, so that revoking one by one names only one's own.
func (w *Wide) RevokeToken(ctx context.Context, principal, id string, at time.Time) (bool, error) {
	var was *time.Time
	err := w.tx.QueryRow(ctx,
		`select revoked_at from api_tokens where principal = $1 and id = $2 for update`, principal, id).Scan(&was)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNoToken
	}
	if err != nil {
		return false, fmt.Errorf("db: token %s could not be read: %w", id, err)
	}
	if was != nil {
		return false, nil
	}
	if _, err := w.tx.Exec(ctx, `update api_tokens set revoked_at = $2 where id = $1`, id, at); err != nil {
		return false, fmt.Errorf("db: token %s could not be revoked: %w", id, err)
	}
	return true, nil
}

// ErrNoSession is a session that opens nothing: never opened, revoked, or idle past its expiry.
var ErrNoSession = errors.New("db: no live session of that identifier")

// Session is a console session, without its identifier. It was opened by a credential or, for a
// session that may only enrol a passkey, by an enrolment code.
type Session struct {
	Hash          []byte
	Login         string
	Credential    string
	EnrolmentCode []byte

	CreatedAt     time.Time
	IdleExpiresAt time.Time
}

// OpenSession writes one. CreatedAt is the caller's, as IdleExpiresAt is.
func (w *Wide) OpenSession(ctx context.Context, s Session) error {
	_, err := w.tx.Exec(ctx,
		`insert into sessions (hash, login, credential, enrolment_code, created_at, idle_expires_at)
		 values ($1, $2, $3, $4, $5, $6)`,
		s.Hash, s.Login, nilIfEmpty(s.Credential), nilIfNone(s.EnrolmentCode), s.CreatedAt, s.IdleExpiresAt)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == foreignKeyViolation {
		return fmt.Errorf("%w: %s holds nothing that session names", ErrNoCredential, s.Login)
	}
	if err != nil {
		return fmt.Errorf("db: a session of %s could not be opened: %w", s.Login, err)
	}
	return nil
}

// SessionByHash answers the session whose identifier hashes to hash, if it is neither revoked nor
// idle past its expiry at now.
func (w *Wide) SessionByHash(ctx context.Context, hash []byte, now time.Time) (Session, error) {
	var s Session
	var credential *string
	err := w.tx.QueryRow(ctx,
		`select hash, login, credential, enrolment_code, created_at, idle_expires_at from sessions
		  where hash = $1 and revoked_at is null and idle_expires_at > $2`, hash, now,
	).Scan(&s.Hash, &s.Login, &credential, &s.EnrolmentCode, &s.CreatedAt, &s.IdleExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNoSession
	}
	if err != nil {
		return Session{}, fmt.Errorf("db: a session could not be read: %w", err)
	}
	if credential != nil {
		s.Credential = *credential
	}
	return s, nil
}

// TouchSession moves the idle expiry of a session live at now to until. One already idle past
// its expiry stays expired, so that a request arriving late opens nothing again.
func (w *Wide) TouchSession(ctx context.Context, hash []byte, now, until time.Time) error {
	tag, err := w.tx.Exec(ctx,
		`update sessions set idle_expires_at = $3
		  where hash = $1 and revoked_at is null and idle_expires_at > $2`, hash, now, until)
	if err != nil {
		return fmt.Errorf("db: a session could not be kept open: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoSession
	}
	return nil
}

// RevokeSessions revokes a user's live sessions, or the one whose identifier hashes to hash where
// hash is given, and answers how many it revoked.
func (w *Wide) RevokeSessions(ctx context.Context, login string, hash []byte, at time.Time) (int, error) {
	tag, err := w.tx.Exec(ctx,
		`update sessions set revoked_at = $3
		  where login = $1 and ($2::bytea is null or hash = $2) and revoked_at is null`,
		login, nilIfNone(hash), at)
	if err != nil {
		return 0, fmt.Errorf("db: the sessions of %s could not be revoked: %w", login, err)
	}
	return int(tag.RowsAffected()), nil
}

// The kinds of enrolment code, as enrolment_codes.kind writes them.
const (
	EnrolmentFirstAdministrator = "first-administrator"
	EnrolmentRecovery           = "recovery"
)

// ErrNoEnrolmentCode is a code that opens nothing: never issued, used, revoked or expired.
var ErrNoEnrolmentCode = errors.New("db: no open enrolment code of that value")

// EnrolmentCode lets one user enrol a passkey, once, within the hour.
type EnrolmentCode struct {
	Hash     []byte
	Login    string
	Kind     string
	IssuedBy string

	IssuedAt  time.Time
	ExpiresAt time.Time
	UsedAt    time.Time
}

// IssueEnrolmentCode writes a code, revoking at its IssuedAt the one of the same kind still open
// for the same user, and answers whether there was one: a link issued again leaves the one before
// it unusable.
func (w *Wide) IssueEnrolmentCode(ctx context.Context, c EnrolmentCode) (bool, error) {
	tag, err := w.tx.Exec(ctx,
		`update enrolment_codes set revoked_at = $3
		  where login = $1 and kind = $2 and used_at is null and revoked_at is null`,
		c.Login, c.Kind, c.IssuedAt)
	if err != nil {
		return false, fmt.Errorf("db: the enrolment codes of %s could not be revoked: %w", c.Login, err)
	}
	_, err = w.tx.Exec(ctx,
		`insert into enrolment_codes (hash, login, kind, issued_by, issued_at, expires_at)
		 values ($1, $2, $3, $4, $5, $6)`,
		c.Hash, c.Login, c.Kind, c.IssuedBy, c.IssuedAt, c.ExpiresAt)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == foreignKeyViolation {
		return false, fmt.Errorf("%w: %s", ErrNoPrincipal, c.Login)
	}
	if err != nil {
		return false, fmt.Errorf("db: an enrolment code for %s could not be issued: %w", c.Login, err)
	}
	return tag.RowsAffected() > 0, nil
}

const enrolmentColumns = `hash, login, kind, issued_by, issued_at, expires_at`

func scanEnrolment(row pgx.Row) (EnrolmentCode, error) {
	var c EnrolmentCode
	err := row.Scan(&c.Hash, &c.Login, &c.Kind, &c.IssuedBy, &c.IssuedAt, &c.ExpiresAt)
	return c, err
}

// EnrolmentCodeByHash answers the code whose value hashes to hash, if it is open at now: neither
// used, revoked nor expired.
func (w *Wide) EnrolmentCodeByHash(ctx context.Context, hash []byte, now time.Time) (EnrolmentCode, error) {
	c, err := scanEnrolment(w.tx.QueryRow(ctx,
		`select `+enrolmentColumns+` from enrolment_codes
		  where hash = $1 and used_at is null and revoked_at is null and expires_at > $2`, hash, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return EnrolmentCode{}, ErrNoEnrolmentCode
	}
	if err != nil {
		return EnrolmentCode{}, fmt.Errorf("db: an enrolment code could not be read: %w", err)
	}
	return c, nil
}

// UseEnrolmentCode spends an open code at now and answers it. Of two uses at once, one spends it
// and the other is answered ErrNoEnrolmentCode.
func (w *Wide) UseEnrolmentCode(ctx context.Context, hash []byte, now time.Time) (EnrolmentCode, error) {
	c, err := scanEnrolment(w.tx.QueryRow(ctx,
		`update enrolment_codes set used_at = $2
		  where hash = $1 and used_at is null and revoked_at is null and expires_at > $2
		 returning `+enrolmentColumns, hash, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return EnrolmentCode{}, ErrNoEnrolmentCode
	}
	if err != nil {
		return EnrolmentCode{}, fmt.Errorf("db: an enrolment code could not be used: %w", err)
	}
	c.UsedAt = now
	return c, nil
}
