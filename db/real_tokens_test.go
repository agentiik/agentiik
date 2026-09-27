package db

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// A listing of tokens holds what is still accepted, and a token is read by its identifier whatever
// became of it, since whose it is decides who may revoke it.

// The tokens of a principal and of the service accounts of the namespaces named, newest first, and
// none that is revoked, expired or a suspended user's: "an expired or revoked token is gone from the
// list". A service account of a namespace not named is not listed, and neither is anybody else's,
// the user a namespace named is named after included.
func TestAListingHoldsOnlyTheTokensStillAccepted(t *testing.T) {
	pool := identity(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	minted := func(id, principal string, created, expires time.Time) APIToken {
		return APIToken{ID: id, Hash: valueHash(id), Principal: principal, CreatedAt: created, ExpiresAt: expires}
	}
	week := 7 * 24 * time.Hour
	var listed, ofTeamOps, ofSuspended, asAlice []APIToken
	var revoked APIToken
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, u := range []User{{Login: "alice", DisplayName: "Alice"}, {Login: "bob", DisplayName: "Bob"}} {
			if err := w.CreateUser(ctx, u); err != nil {
				return err
			}
		}
		for _, sa := range []ServiceAccount{
			{Namespace: "finance", Name: "nightly-sync", CreatedBy: "alice"},
			{Namespace: "team-ops", Name: "deploy-bot", CreatedBy: "alice"},
		} {
			if err := w.CreateServiceAccount(ctx, sa); err != nil {
				return err
			}
		}
		for _, tk := range []APIToken{
			minted("01A", "alice", now.Add(-3*week), now.Add(week)),
			minted("01B", "alice", now.Add(-2*week), now.Add(week)),
			minted("01C", "alice", now.Add(-week), now.Add(week)),
			minted("01D", "alice", now.Add(-week), now),
			minted("01E", "finance/nightly-sync", now.Add(-time.Hour), now.Add(week)),
			minted("01F", "team-ops/deploy-bot", now.Add(-time.Hour), now.Add(week)),
			minted("01G", "bob", now.Add(-2*time.Hour), now.Add(week)),
		} {
			if err := w.MintToken(ctx, tk); err != nil {
				return err
			}
		}
		if _, err := w.RevokeToken(ctx, "alice", "01B", now); err != nil {
			return err
		}
		var err error
		if listed, err = w.TokensOf(ctx, "alice", []string{"finance"}, now); err != nil {
			return err
		}
		if ofTeamOps, err = w.TokensOf(ctx, "bob", []string{"team-ops"}, now); err != nil {
			return err
		}
		// A namespace named after a login, as a personal namespace is, lists no token of
		// that user: a login is no service account of it.
		if asAlice, err = w.TokensOf(ctx, "bob", []string{"alice"}, now); err != nil {
			return err
		}
		if revoked, err = w.Token(ctx, "01B"); err != nil {
			return err
		}
		if err := w.UpdateUser(ctx, User{Login: "alice", DisplayName: "Alice", Suspended: true}); err != nil {
			return err
		}
		ofSuspended, err = w.TokensOf(ctx, "alice", nil, now)
		return err
	})

	ids := func(ts []APIToken) []string {
		var out []string
		for _, tk := range ts {
			out = append(out, tk.ID)
		}
		return out
	}
	// Newest first, the service account's, then alice's that are neither revoked (01B) nor at
	// their expiry (01D).
	if got, want := ids(listed), []string{"01E", "01C", "01A"}; !slices.Equal(got, want) {
		t.Errorf("alice with finance lists %q, want %q", got, want)
	}
	if got, want := ids(ofTeamOps), []string{"01F", "01G"}; !slices.Equal(got, want) {
		t.Errorf("bob with team-ops lists %q, want %q", got, want)
	}
	if got, want := ids(asAlice), []string{"01G"}; !slices.Equal(got, want) {
		t.Errorf("bob with the namespace alice lists %q, want %q", got, want)
	}
	if len(ofSuspended) != 0 {
		t.Errorf("a suspended user's tokens are listed: %q", ids(ofSuspended))
	}
	if revoked.Principal != "alice" || !revoked.RevokedAt.Equal(now) {
		t.Errorf("a revoked token reads by its identifier as %+v", revoked)
	}
}

// A token nobody minted is ErrNoToken, and so is an identifier no token could have, which is
// answered as the absence it is rather than as a value the database refuses to be asked about.
func TestATokenNobodyMintedIsNoToken(t *testing.T) {
	pool := identity(t)
	for _, id := range []string{"01M2AD1R3T5W7Y9A1C3E5G7J9M", "not-an-identifier", "%ff"} {
		err := pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
			_, err := w.Token(ctx, id)
			return err
		})
		if !errors.Is(err, ErrNoToken) {
			t.Errorf("token %q is answered %v", id, err)
		}
	}
}
