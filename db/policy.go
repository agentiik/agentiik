package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The authentication policy and the bootstrap state: the installation's two answers about how
// anybody gets in, read before any namespace is in question.

// AuthPolicy is how people prove who they are, as $defs/authPolicy writes it. The installation's
// sets every field. A namespace's sets only what it tightens: an empty string, a nil
// DeviceBoundOnly and a zero MinPasskeys are the installation's value. Whether a namespace's is
// tighter needs both, and is the API's to say.
type AuthPolicy struct {
	Password         string
	Passkey          string
	UserVerification string
	DeviceBoundOnly  *bool
	MinPasskeys      int
}

const policyColumns = `coalesce(password, ''), coalesce(passkey, ''), coalesce(user_verification, ''),
	device_bound_only, coalesce(min_passkeys, 0)`

func scanPolicy(row pgx.Row) (AuthPolicy, error) {
	var p AuthPolicy
	err := row.Scan(&p.Password, &p.Passkey, &p.UserVerification, &p.DeviceBoundOnly, &p.MinPasskeys)
	return p, err
}

// InstallationPolicy reads the installation's, which the migration wrote at the documented
// defaults and an administrator may have changed since.
func (w *Wide) InstallationPolicy(ctx context.Context) (AuthPolicy, error) {
	p, err := scanPolicy(w.tx.QueryRow(ctx, `select `+policyColumns+` from auth_policy where namespace is null`))
	if err != nil {
		return AuthPolicy{}, fmt.Errorf("db: the installation's authentication policy could not be read: %w", err)
	}
	return p, nil
}

// SetInstallationPolicy writes the installation's, every setting of it. One with a setting
// missing is refused: the installation's policy is the one nothing is inherited from.
func (w *Wide) SetInstallationPolicy(ctx context.Context, p AuthPolicy, at time.Time) error {
	tag, err := w.tx.Exec(ctx,
		`update auth_policy set password = $1, passkey = $2, user_verification = $3,
		        device_bound_only = $4, min_passkeys = $5, updated_at = $6
		  where namespace is null`,
		nilIfEmpty(p.Password), nilIfEmpty(p.Passkey), nilIfEmpty(p.UserVerification), p.DeviceBoundOnly,
		zeroIsNull(p.MinPasskeys), at)
	if err != nil {
		return fmt.Errorf("db: the installation's authentication policy could not be written: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errors.New("db: the installation has no authentication policy to write, which its migration wrote and nothing removes")
	}
	return nil
}

// NamespacePolicy reads what a namespace tightens, which is nothing where it has set nothing.
func (w *Wide) NamespacePolicy(ctx context.Context, namespace string) (AuthPolicy, error) {
	p, err := scanPolicy(w.tx.QueryRow(ctx, `select `+policyColumns+` from auth_policy where namespace = $1`, namespace))
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthPolicy{}, nil
	}
	if err != nil {
		return AuthPolicy{}, fmt.Errorf("db: the authentication policy of %s could not be read: %w", namespace, err)
	}
	return p, nil
}

// SetNamespacePolicy writes what a namespace tightens, replacing what it tightened before. A
// policy that sets nothing removes the namespace's, which then follows the installation's whole.
func (w *Wide) SetNamespacePolicy(ctx context.Context, namespace string, p AuthPolicy, at time.Time) error {
	if p == (AuthPolicy{}) {
		if _, err := w.tx.Exec(ctx, `delete from auth_policy where namespace = $1`, namespace); err != nil {
			return fmt.Errorf("db: the authentication policy of %s could not be removed: %w", namespace, err)
		}
		return nil
	}
	_, err := w.tx.Exec(ctx,
		`insert into auth_policy (namespace, password, passkey, user_verification, device_bound_only, min_passkeys, updated_at)
		 values ($1, $2, $3, $4, $5, $6, $7)
		 on conflict (namespace) do update
		   set password = excluded.password, passkey = excluded.passkey,
		       user_verification = excluded.user_verification,
		       device_bound_only = excluded.device_bound_only, min_passkeys = excluded.min_passkeys,
		       updated_at = excluded.updated_at`,
		namespace, nilIfEmpty(p.Password), nilIfEmpty(p.Passkey), nilIfEmpty(p.UserVerification),
		p.DeviceBoundOnly, zeroIsNull(p.MinPasskeys), at)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == foreignKeyViolation {
		return fmt.Errorf("%w: %s", ErrNoNamespace, namespace)
	}
	if err != nil {
		return fmt.Errorf("db: the authentication policy of %s could not be written: %w", namespace, err)
	}
	return nil
}

// ErrBootstrapEnded is the bootstrap token asked for after the first administrator enrolled,
// which ended it for good.
var ErrBootstrapEnded = errors.New("db: the bootstrap token ended when the first administrator enrolled")

// Bootstrap is the bootstrap token's state: the SHA-256 of the token while it works, and when the
// first administrator's enrolment ended it.
type Bootstrap struct {
	TokenHash  []byte
	EnrolledAt time.Time
}

// Ended says whether the token has ended, which it does once and for good.
func (b Bootstrap) Ended() bool { return !b.EnrolledAt.IsZero() }

// Bootstrap reads it.
func (w *Wide) Bootstrap(ctx context.Context) (Bootstrap, error) {
	var b Bootstrap
	var enrolled *time.Time
	if err := w.tx.QueryRow(ctx, `select token_hash, enrolled_at from bootstrap`).Scan(&b.TokenHash, &enrolled); err != nil {
		return Bootstrap{}, fmt.Errorf("db: the bootstrap state could not be read: %w", err)
	}
	if enrolled != nil {
		b.EnrolledAt = *enrolled
	}
	return b, nil
}

// BootstrapHeld reads it as Bootstrap does, and holds the row until the transaction ends, so that
// an act of the bootstrap token and the enrolment that ends the token take turns: the act commits
// while the token is live, or reads that it has ended. Taken after the act's own rows and before
// its entries in the audit log, which is the order EndBootstrap's caller takes them in.
func (w *Wide) BootstrapHeld(ctx context.Context) (Bootstrap, error) {
	var b Bootstrap
	var enrolled *time.Time
	if err := w.tx.QueryRow(ctx, `select token_hash, enrolled_at from bootstrap for share`).Scan(&b.TokenHash, &enrolled); err != nil {
		return Bootstrap{}, fmt.Errorf("db: the bootstrap state could not be read: %w", err)
	}
	if enrolled != nil {
		b.EnrolledAt = *enrolled
	}
	return b, nil
}

// SetBootstrapToken keeps the hash of the bootstrap token, as init does at every run from the
// token the installation's settings hold, and answers whether it changed. Once the token has
// ended it is ErrBootstrapEnded, and nothing is written.
func (w *Wide) SetBootstrapToken(ctx context.Context, hash []byte) (bool, error) {
	var was []byte
	var enrolled *time.Time
	if err := w.tx.QueryRow(ctx, `select token_hash, enrolled_at from bootstrap for update`).Scan(&was, &enrolled); err != nil {
		return false, fmt.Errorf("db: the bootstrap state could not be read: %w", err)
	}
	if enrolled != nil {
		return false, ErrBootstrapEnded
	}
	if string(was) == string(hash) {
		return false, nil
	}
	if _, err := w.tx.Exec(ctx, `update bootstrap set token_hash = $1`, hash); err != nil {
		return false, fmt.Errorf("db: the bootstrap token could not be kept: %w", err)
	}
	return true, nil
}

// EndBootstrap ends the bootstrap token at the first administrator's enrolment, forgetting its
// hash, and answers whether this was the end of it: a second enrolment ends nothing more. Every
// first administrator's link still open is revoked with it, and the session each opened, since a
// link made with the token is the token's reach and ends where it does.
func (w *Wide) EndBootstrap(ctx context.Context, at time.Time) (bool, error) {
	tag, err := w.tx.Exec(ctx,
		`update bootstrap set enrolled_at = $1, token_hash = null where enrolled_at is null`, at)
	if err != nil {
		return false, fmt.Errorf("db: the bootstrap token could not be ended: %w", err)
	}
	if _, err := w.tx.Exec(ctx,
		`update enrolment_codes set revoked_at = $1
		  where kind = 'first-administrator' and used_at is null and revoked_at is null`, at); err != nil {
		return false, fmt.Errorf("db: the first administrator's links could not be revoked: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
