package db

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// An OAuth grant is one row of its person's tokens, and its refresh token one use: what the
// installation's own authorisation server keeps, held here against PostgreSQL.

const theAudience = "https://agentiik.example.com/mcp"

// aGrant is an OAuth grant of alice's for theAudience, keeping workflow:read, its access token
// lapsing within the hour and the grant 90 days on.
func aGrant(id string, now time.Time) APIToken {
	return APIToken{
		ID: id, Hash: valueHash("access " + id), Principal: "alice", Permissions: []string{"workflow:read"},
		DeviceLabel: "Example client", CreatedAt: now, ExpiresAt: now.AddDate(0, 0, 90),
		Audience: theAudience, ClientID: "https://client.example.com/oauth.json", AccessExpiresAt: now.Add(time.Hour),
	}
}

// A grant is answered by its access token while that lasts, with its audience and its client, and
// not once it lapses, though the grant goes on for its refresh token to renew; the table refuses a
// grant with half of what makes it one, one reaching only some namespaces, and one whose access
// token outlives it.
func TestAGrantIsAnsweredWhileItsAccessTokenLasts(t *testing.T) {
	pool := identity(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	var during APIToken
	var after error
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.CreateUser(ctx, User{Login: "alice", Profile: Profile{GivenName: "Alice"}}); err != nil {
			return err
		}
		if err := w.MintToken(ctx, aGrant("01QA", now)); err != nil {
			return err
		}
		var err error
		if during, err = w.TokenByHash(ctx, valueHash("access 01QA"), now.Add(59*time.Minute)); err != nil {
			return err
		}
		_, after = w.TokenByHash(ctx, valueHash("access 01QA"), now.Add(61*time.Minute))
		return nil
	})
	if during.Audience != theAudience || during.ClientID != "https://client.example.com/oauth.json" || !during.AccessExpiresAt.Equal(now.Add(time.Hour)) {
		t.Errorf("the grant reads back as %+v", during)
	}
	if !errors.Is(after, ErrNoToken) {
		t.Errorf("an access token past its hour answered %v", after)
	}

	for name, broken := range map[string]func(APIToken) APIToken{
		"no client":             func(g APIToken) APIToken { g.ClientID = ""; return g },
		"no access expiry":      func(g APIToken) APIToken { g.AccessExpiresAt = time.Time{}; return g },
		"no permissions":        func(g APIToken) APIToken { g.Permissions = nil; return g },
		"a within":              func(g APIToken) APIToken { g.Within = []string{"finance"}; return g },
		"an access past it":     func(g APIToken) APIToken { g.AccessExpiresAt = g.ExpiresAt.Add(time.Second); return g },
		"a client, no resource": func(g APIToken) APIToken { g.Audience = ""; return g },
	} {
		err := pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
			return w.MintToken(ctx, broken(aGrant("01QB", now)))
		})
		if err == nil {
			t.Errorf("a grant with %s was minted", name)
		}
	}
}

// A refresh token is spent by the refresh that takes it, which answers its grant locked and gives the
// grant its next pair; the one spent presented again names the grant, so that the caller can end
// it; one never issued opens nothing, and one of a grant revoked opens nothing either. A grant holds
// one open refresh token at a time.
func TestARefreshTokenIsSpentOnceAndNamesItsGrantWhenPresentedAgain(t *testing.T) {
	pool := identity(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	later := now.Add(2 * time.Hour)
	var taken, rotated APIToken
	var again, unknown, second, afterRevoke error
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.CreateUser(ctx, User{Login: "alice", Profile: Profile{GivenName: "Alice"}}); err != nil {
			return err
		}
		if err := w.MintToken(ctx, aGrant("01QC", now)); err != nil {
			return err
		}
		return w.IssueRefreshToken(ctx, "01QC", valueHash("refresh one"), now)
	})
	second = pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
		return w.IssueRefreshToken(ctx, "01QC", valueHash("refresh beside"), now)
	})
	if second == nil {
		t.Error("a grant was issued a second open refresh token")
	}
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		var err error
		// Its access token has lapsed, which is what a refresh is for.
		if taken, err = w.TakeRefreshToken(ctx, valueHash("refresh one"), later); err != nil {
			return err
		}
		if err := w.RotateToken(ctx, taken.ID, valueHash("access two"), later.Add(time.Hour), []string{"workflow:read"}); err != nil {
			return err
		}
		if err := w.IssueRefreshToken(ctx, taken.ID, valueHash("refresh two"), later); err != nil {
			return err
		}
		if rotated, err = w.TokenByHash(ctx, valueHash("access two"), later); err != nil {
			return err
		}
		_, again = w.TakeRefreshToken(ctx, valueHash("refresh one"), later)
		_, unknown = w.TakeRefreshToken(ctx, valueHash("refresh never issued"), later)
		if _, err := w.RevokeToken(ctx, "alice", taken.ID, later); err != nil {
			return err
		}
		_, afterRevoke = w.TakeRefreshToken(ctx, valueHash("refresh two"), later)
		return nil
	})
	if taken.ID != "01QC" || rotated.ID != "01QC" || !slices.Equal(rotated.Permissions, []string{"workflow:read"}) {
		t.Errorf("the refresh took %+v and rotated it to %+v", taken, rotated)
	}
	var spent *RefreshTokenSpent
	if !errors.As(again, &spent) || spent.Token.ID != "01QC" || spent.Token.Principal != "alice" {
		t.Errorf("a refresh token spent already answered %v", again)
	}
	if !errors.Is(unknown, ErrNoRefreshToken) || !errors.Is(afterRevoke, ErrNoRefreshToken) {
		t.Errorf("a refresh token never issued answered %v, and one of a revoked grant %v", unknown, afterRevoke)
	}
	var left int
	if err := pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
		// The next issue sweeps what the revoked grant held, which nothing takes any more.
		if err := w.CreateUser(ctx, User{Login: "bob", Profile: Profile{GivenName: "Bob"}}); err != nil {
			return err
		}
		g := aGrant("01QD", later)
		g.Principal, g.Hash = "bob", valueHash("access 01QD")
		if err := w.MintToken(ctx, g); err != nil {
			return err
		}
		if err := w.IssueRefreshToken(ctx, "01QD", valueHash("refresh of bob"), later); err != nil {
			return err
		}
		return w.tx.QueryRow(ctx, `select count(*) from oauth_refresh_tokens where token = '01QC'`).Scan(&left)
	}); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d refresh tokens of a revoked grant were kept past the next issue", left)
	}
}

// An OAuth authorisation code is kept and taken as agk login's is, with what it was minted for, and
// the table refuses one holding part of it.
func TestAnAuthorisationCodeCarriesWhatItWasMintedFor(t *testing.T) {
	pool := identity(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	minted := &CodeAuthorization{
		ClientID: "https://client.example.com/oauth.json", ClientName: "Example client",
		RedirectURI: "http://127.0.0.1:4711/callback", Resource: theAudience, Scope: []string{"workflow:read", "run:read"},
	}
	var took, plain ExchangeCode
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.CreateUser(ctx, User{Login: "alice", Profile: Profile{GivenName: "Alice"}}); err != nil {
			return err
		}
		if err := w.AddCredential(ctx, Credential{ID: "alice-password", Login: "alice", Type: CredentialPassword, PasswordHash: "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA"}); err != nil {
			return err
		}
		code := ExchangeCode{
			Hash: valueHash("code"), Login: "alice", Credential: "alice-password", CodeChallenge: "Ibi4l3hyoxxry38-L3XZ59u9IdHegygM4WK38DG2YKk",
			IssuedAt: now, ExpiresAt: now.Add(ExchangeCodeLife), OAuth: minted,
		}
		if err := w.IssueExchangeCode(ctx, code); err != nil {
			return err
		}
		code.Hash, code.OAuth = valueHash("agk login's"), nil
		if err := w.IssueExchangeCode(ctx, code); err != nil {
			return err
		}
		var err error
		if took, err = w.TakeExchangeCode(ctx, valueHash("code"), now); err != nil {
			return err
		}
		plain, err = w.TakeExchangeCode(ctx, valueHash("agk login's"), now)
		return err
	})
	if took.OAuth == nil || took.OAuth.ClientID != minted.ClientID || took.OAuth.ClientName != minted.ClientName ||
		took.OAuth.RedirectURI != minted.RedirectURI || took.OAuth.Resource != minted.Resource || !slices.Equal(took.OAuth.Scope, minted.Scope) {
		t.Errorf("the code was taken as %+v", took.OAuth)
	}
	if plain.OAuth != nil {
		t.Errorf("agk login's code was taken with an authorisation: %+v", plain.OAuth)
	}
	err := pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
		return w.IssueExchangeCode(ctx, ExchangeCode{
			Hash: valueHash("half"), Login: "alice", Credential: "alice-password", CodeChallenge: "Ibi4l3hyoxxry38-L3XZ59u9IdHegygM4WK38DG2YKk",
			IssuedAt: now, ExpiresAt: now.Add(ExchangeCodeLife), OAuth: &CodeAuthorization{ClientID: minted.ClientID},
		})
	})
	if err == nil {
		t.Error("a code naming a client and no redirect URI, resource or scope was kept")
	}
}
