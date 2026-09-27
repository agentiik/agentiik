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
// only one that still opens something, so that a caller who forgot to check an expiry, a
// revocation or a suspension is handed nothing to forget it with.

// liveUser is the condition a token or a session of a user is answered on: the user is not
// suspended, since "a suspended account opens no session", and a token of theirs is no way around
// it. A service account has no row of users, and is never suspended.
const liveUser = `not exists (select from users u where u.login = %s and u.suspended)`

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
// at now, and its principal is not a suspended user.
func (w *Wide) TokenByHash(ctx context.Context, hash []byte, now time.Time) (APIToken, error) {
	t, err := scanToken(w.tx.QueryRow(ctx,
		`select `+tokenColumns+` from api_tokens
		  where hash = $1 and revoked_at is null and expires_at > $2
		    and `+fmt.Sprintf(liveUser, "api_tokens.principal"), hash, now))
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

// ErrSessionRefused is a session nothing may open: one a credential opens for a suspended user,
// or one naming an enrolment code that is revoked, past its hour, or has opened a session already.
var ErrSessionRefused = errors.New("db: nothing opens that session")

// OpenSession writes one. CreatedAt is the caller's, as IdleExpiresAt is. One opened by an
// enrolment code is refused where the code is revoked or past its hour at CreatedAt, used or not,
// so that the code may be spent as the session opens or when the enrolment completes, and a code
// opens one session at most.
//
// A suspended user opens no session with a credential, and one with an enrolment code all the
// same: "its enrolment links and recovery codes still work, since enrolling is how an account
// suspended for having no passkey comes back", and such a session enrols a passkey and nothing
// else.
func (w *Wide) OpenSession(ctx context.Context, s Session) error {
	tag, err := w.tx.Exec(ctx,
		`insert into sessions (hash, login, credential, enrolment_code, created_at, idle_expires_at)
		 select $1, $2, $3, $4, $5, $6
		  where ($4::bytea is not null or `+fmt.Sprintf(liveUser, "$2")+`)
		    and ($4::bytea is null or exists (
		          select from enrolment_codes c
		           where c.hash = $4 and c.login = $2 and c.revoked_at is null and c.expires_at > $5))`,
		s.Hash, s.Login, nilIfEmpty(s.Credential), nilIfNone(s.EnrolmentCode), s.CreatedAt, s.IdleExpiresAt)
	var pg *pgconn.PgError
	switch {
	case errors.As(err, &pg) && pg.ConstraintName == "sessions_enrolment_code_key":
		return fmt.Errorf("%w: that code opened a session already", ErrSessionRefused)
	case errors.As(err, &pg) && pg.ConstraintName == "sessions_login_fkey":
		return fmt.Errorf("%w: %s", ErrNoPrincipal, s.Login)
	case errors.As(err, &pg) && pg.Code == foreignKeyViolation:
		return fmt.Errorf("%w: %s holds nothing that session names", ErrNoCredential, s.Login)
	case err != nil:
		return fmt.Errorf("db: a session of %s could not be opened: %w", s.Login, err)
	case tag.RowsAffected() == 0:
		return ErrSessionRefused
	}
	return nil
}

// liveSession is the condition a session is answered and kept open on at $2: neither revoked nor
// idle past its expiry, and either opened by a credential of a user not suspended, or by an
// enrolment code neither revoked nor past its hour. A session that may only enrol lives no longer
// than the link that opened it, and a link issued again ends it; a suspension does not, since the
// link is how a suspended account comes back.
const liveSession = `revoked_at is null and idle_expires_at > $2
	and (enrolment_code is not null
	     or not exists (select from users u where u.login = sessions.login and u.suspended))
	and (enrolment_code is null or exists (
	      select from enrolment_codes c
	       where c.hash = sessions.enrolment_code and c.revoked_at is null and c.expires_at > $2))`

// SessionByHash answers the session whose identifier hashes to hash, if it is live at now.
func (w *Wide) SessionByHash(ctx context.Context, hash []byte, now time.Time) (Session, error) {
	var s Session
	var credential *string
	err := w.tx.QueryRow(ctx,
		`select hash, login, credential, enrolment_code, created_at, idle_expires_at from sessions
		  where hash = $1 and `+liveSession, hash, now,
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

// TouchSession moves the idle expiry of a session live at now to until. One that is not live
// stays as it is, so that a request arriving late opens nothing again.
func (w *Wide) TouchSession(ctx context.Context, hash []byte, now, until time.Time) error {
	tag, err := w.tx.Exec(ctx,
		`update sessions set idle_expires_at = $3
		  where hash = $1 and `+liveSession, hash, now, until)
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
	// EnrolmentFirstAdministrator is the first administrator's link, made with the bootstrap
	// token.
	EnrolmentFirstAdministrator = "first-administrator"
	// EnrolmentNewUser is the link a new user is given, and given again while they hold no
	// credential.
	EnrolmentNewUser = "enrolment"
	// EnrolmentRecovery is a recovery code, for a user who lost their passkeys.
	EnrolmentRecovery = "recovery"
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

// IssueEnrolmentCode writes a code, revoking at its IssuedAt the code it replaces, and answers
// whether there was one: a link issued again leaves the one before it unusable, and the session it
// opened with it. A new user's link or a recovery code replaces the user's open code of its kind. A
// first administrator's
// link replaces every open one, whoever it was for, and is ErrBootstrapEnded once the first
// administrator has enrolled.
//
// The user's row is locked first, and the bootstrap state's for a first administrator's link, so
// that two issues at once take turns and the second replaces the first rather than failing on it.
func (w *Wide) IssueEnrolmentCode(ctx context.Context, c EnrolmentCode) (bool, error) {
	err := w.tx.QueryRow(ctx, `select login from users where login = $1 for update`, c.Login).Scan(new(string))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("%w: %s", ErrNoPrincipal, c.Login)
	}
	if err != nil {
		return false, fmt.Errorf("db: user %s could not be read: %w", c.Login, err)
	}
	replaced := `login = $1 and kind = $2`
	if c.Kind == EnrolmentFirstAdministrator {
		var enrolled *time.Time
		if err := w.tx.QueryRow(ctx, `select enrolled_at from bootstrap for update`).Scan(&enrolled); err != nil {
			return false, fmt.Errorf("db: the bootstrap state could not be read: %w", err)
		}
		if enrolled != nil {
			return false, ErrBootstrapEnded
		}
		replaced = `$1::text is not null and kind = $2`
	}
	tag, err := w.tx.Exec(ctx,
		`update enrolment_codes set revoked_at = $3
		  where `+replaced+` and used_at is null and revoked_at is null`,
		c.Login, c.Kind, c.IssuedAt)
	if err != nil {
		return false, fmt.Errorf("db: the enrolment codes of %s could not be revoked: %w", c.Login, err)
	}
	// The sessions the codes it replaces opened end with them, those of a code spent as its
	// session opened included: a spent code is not revoked, since it opens nothing more, but its
	// session "ends when a fresh link replaces it" all the same, so that a link that leaked and was
	// opened first is shut by issuing another.
	if _, err := w.tx.Exec(ctx,
		`update sessions set revoked_at = $3
		  where revoked_at is null
		    and enrolment_code in (select hash from enrolment_codes where `+replaced+`)`,
		c.Login, c.Kind, c.IssuedAt); err != nil {
		return false, fmt.Errorf("db: the sessions the enrolment codes of %s opened could not be ended: %w", c.Login, err)
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
