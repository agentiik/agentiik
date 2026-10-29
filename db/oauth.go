package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The refresh tokens of the OAuth grants the installation's own authorisation server issues. A grant
// is an API token's row (APIToken, Audience set), whose hash is its current access token's; what is
// here is the refresh token beside it, single use, which a refresh spends to issue the next pair.
// Minting the values and deciding what a refresh may ask are the API's.

// ErrNoRefreshToken is a refresh token that opens nothing: never issued, or of a grant revoked,
// expired or removed, or of a suspended user.
var ErrNoRefreshToken = errors.New("db: no live refresh token of that value")

// RefreshTokenSpent is a refresh token presented after a refresh spent it, and the grant it belongs
// to: "if a refresh token is compromised and subsequently used by both the attacker and the
// legitimate client, one of them will present an invalidated refresh token", and the caller ends the
// grant, since which of the two presented it cannot be told.
type RefreshTokenSpent struct {
	Token APIToken
}

func (e *RefreshTokenSpent) Error() string {
	return "db: that refresh token was spent already, by a refresh of grant " + e.Token.ID
}

// refreshSwept is how many refresh tokens of grants that ended IssueRefreshToken removes at once, so
// that each issue does a bounded share of the sweeping and none waits on the whole of it.
const refreshSwept = 100

// IssueRefreshToken keeps a refresh token of grant token, issued at at, and removes some of those of
// grants revoked or past their end, which nothing takes any more, skipping a row another transaction
// holds. A grant that is not there is ErrNoToken; one already holding an open refresh token is
// refused by the table, since a refresh spends the one it presents before it issues the next.
func (w *Wide) IssueRefreshToken(ctx context.Context, token string, hash []byte, at time.Time) error {
	if _, err := w.tx.Exec(ctx,
		`delete from oauth_refresh_tokens
		  where hash in (select r.hash from oauth_refresh_tokens r join api_tokens t on t.id = r.token
		                  where t.revoked_at is not null or t.expires_at <= $1
		                  limit $2 for update of r skip locked)`, at, refreshSwept); err != nil {
		return fmt.Errorf("db: the refresh tokens of grants that ended could not be removed: %w", err)
	}
	_, err := w.tx.Exec(ctx,
		`insert into oauth_refresh_tokens (hash, token, issued_at) values ($1, $2, $3)`, hash, token, at)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == foreignKeyViolation {
		return fmt.Errorf("%w: %s", ErrNoToken, token)
	}
	if err != nil {
		return fmt.Errorf("db: a refresh token of grant %s could not be issued: %w", token, err)
	}
	return nil
}

// TakeRefreshToken spends the refresh token whose value hashes to hash at now and answers the grant it
// belongs to, its row locked until the transaction ends, so that two refreshes of one grant take
// turns. The grant has to be live: neither revoked nor past its end, and its principal no suspended
// user, as TokenByHash holds a token to, though its current access token may have lapsed, which is
// what a refresh is for. A refresh token spent already is a *RefreshTokenSpent naming the grant,
// whether it is live or not; one never issued, or whose grant opens nothing, is ErrNoRefreshToken,
// and is spent all the same.
func (w *Wide) TakeRefreshToken(ctx context.Context, hash []byte, now time.Time) (APIToken, error) {
	var id string
	var used *time.Time
	err := w.tx.QueryRow(ctx,
		`select token, used_at from oauth_refresh_tokens where hash = $1 for update`, hash).Scan(&id, &used)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIToken{}, ErrNoRefreshToken
	}
	if err != nil {
		return APIToken{}, fmt.Errorf("db: a refresh token could not be read: %w", err)
	}
	if used != nil {
		t, err := w.Token(ctx, id)
		if err != nil {
			return APIToken{}, err
		}
		return APIToken{}, &RefreshTokenSpent{Token: t}
	}
	if _, err := w.tx.Exec(ctx, `update oauth_refresh_tokens set used_at = $2 where hash = $1`, hash, now); err != nil {
		return APIToken{}, fmt.Errorf("db: a refresh token could not be spent: %w", err)
	}
	t, err := scanToken(w.tx.QueryRow(ctx,
		`select `+tokenColumns+` from api_tokens
		  where id = $1 and audience is not null and revoked_at is null and expires_at > $2
		    and `+fmt.Sprintf(liveUser, "api_tokens.principal")+`
		  for update`, id, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return APIToken{}, ErrNoRefreshToken
	}
	if err != nil {
		return APIToken{}, fmt.Errorf("db: grant %s could not be read: %w", id, err)
	}
	return t, nil
}

// RotateToken gives a grant its next access token, the SHA-256 hash of a value the caller minted,
// lapsing at accessExpiresAt, and keeps permissions as what the grant keeps from then on, which a
// refresh asking for less narrows. Its refresh token is the caller's to issue beside it, in the same
// transaction.
func (w *Wide) RotateToken(ctx context.Context, id string, hash []byte, accessExpiresAt time.Time, permissions []string) error {
	tag, err := w.tx.Exec(ctx,
		`update api_tokens set hash = $2, access_expires_at = $3, scope_permissions = $4
		  where id = $1 and audience is not null`, id, hash, accessExpiresAt, permissions)
	if err != nil {
		return fmt.Errorf("db: grant %s could not be given its next access token: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrNoToken, id)
	}
	return nil
}
