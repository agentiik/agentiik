package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// agk login's one-time codes: what a sign-in agk login started mints, for the sign-in page to hand
// agk at its loopback address, and what agk trades, with the verifier whose SHA-256 opened the
// page, for an API token. Verifying the verifier and minting the token are the API's; what is here
// is the code kept between the sign-in and the exchange.

// ExchangeCodeLife is how long a code is taken: a minute, the time a browser takes to follow a
// redirect to a port of its own machine, and short enough that one read off a browser's history
// afterwards opens nothing. The table holds it too.
const ExchangeCodeLife = time.Minute

// ErrNoExchangeCode is a code that opens nothing: never minted, taken already, or past its minute.
var ErrNoExchangeCode = errors.New("db: no open exchange code of that value")

// ExchangeCode is a code a sign-in minted, and what it was minted for.
type ExchangeCode struct {
	// Hash is the SHA-256 of the code's value, which the caller computes.
	Hash []byte

	// Login signed in with Credential, a credential of theirs, which says at the exchange what the
	// sign-in may mint, as it says what the session it opened may do.
	Login      string
	Credential string

	// CodeChallenge is the SHA-256 of agk login's verifier, base64url without padding, as the
	// sign-in page was opened with it.
	CodeChallenge string

	IssuedAt  time.Time
	ExpiresAt time.Time
}

// IssueExchangeCode keeps a code until it is taken or its minute passes, and removes some of those
// whose minute passed before c was issued, skipping a row another transaction holds, as
// IssueChallenge does. A login no user holds is ErrNoPrincipal, and a credential that is not
// theirs, removed since the sign-in included, ErrNoCredential.
func (w *Wide) IssueExchangeCode(ctx context.Context, c ExchangeCode) error {
	if _, err := w.tx.Exec(ctx,
		`delete from exchange_codes
		  where hash in (select hash from exchange_codes where expires_at <= $1
		                  limit $2 for update skip locked)`, c.IssuedAt, lapsedSwept); err != nil {
		return fmt.Errorf("db: the exchange codes past their minute could not be removed: %w", err)
	}
	_, err := w.tx.Exec(ctx,
		`insert into exchange_codes (hash, login, credential, code_challenge, issued_at, expires_at)
		 values ($1, $2, $3, $4, $5, $6)`,
		c.Hash, c.Login, c.Credential, c.CodeChallenge, c.IssuedAt, c.ExpiresAt)
	var pg *pgconn.PgError
	switch {
	case errors.As(err, &pg) && pg.ConstraintName == "exchange_codes_login_fkey":
		return fmt.Errorf("%w: %s", ErrNoPrincipal, c.Login)
	case errors.As(err, &pg) && pg.Code == foreignKeyViolation:
		return fmt.Errorf("%w: %s holds no credential %s", ErrNoCredential, c.Login, c.Credential)
	case err != nil:
		return fmt.Errorf("db: an exchange code for %s could not be issued: %w", c.Login, err)
	}
	return nil
}

// TakeExchangeCode answers the code whose value hashes to hash, if it is open at now, and removes it
// in the same statement, whatever the caller then makes of it: of two exchanges presenting it at
// once, one takes it and the other is answered ErrNoExchangeCode, and a code presented with a
// verifier that is not its own is spent all the same.
func (w *Wide) TakeExchangeCode(ctx context.Context, hash []byte, now time.Time) (ExchangeCode, error) {
	var c ExchangeCode
	err := w.tx.QueryRow(ctx,
		`delete from exchange_codes where hash = $1 and expires_at > $2
		 returning hash, login, credential, code_challenge, issued_at, expires_at`, hash, now,
	).Scan(&c.Hash, &c.Login, &c.Credential, &c.CodeChallenge, &c.IssuedAt, &c.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ExchangeCode{}, ErrNoExchangeCode
	}
	if err != nil {
		return ExchangeCode{}, fmt.Errorf("db: an exchange code could not be taken: %w", err)
	}
	return c, nil
}
