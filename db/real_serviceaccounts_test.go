package db

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/agk"
)

// Service accounts and the built-in identity every namespace has, against a real PostgreSQL:
// "non-human, holds API tokens, cannot sign in to a client. Each namespace has a built-in one,
// NS/agentiik, to which scheduled, webhook and event runs are attributed."

// withServiceAccounts is the identity installation, finance and team-ops made as v0.2 made them,
// with no built-in identity, and alice, who created finance/nightly-sync and team-ops/deploy.
// finance/nightly-sync holds a token and a grant.
func withServiceAccounts(t *testing.T) *Pool {
	t.Helper()
	pool := identity(t)
	now := time.Now().UTC()
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.CreateUser(ctx, User{Login: "alice", DisplayName: "Alice"}); err != nil {
			return err
		}
		for _, s := range []ServiceAccount{
			{Namespace: "team-ops", Name: "deploy", CreatedBy: "alice"},
			{Namespace: "finance", Name: "nightly-sync", CreatedBy: "alice"},
		} {
			if err := w.CreateServiceAccount(ctx, s); err != nil {
				return err
			}
		}
		return w.MintToken(ctx, APIToken{ID: "01JQ3M8T", Hash: valueHash("nightly"), Principal: "finance/nightly-sync",
			CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
	})
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, n *NS) error {
		return n.GrantAccess(ctx, access.Grant{ID: "01JQ3M8G", Principal: "finance/nightly-sync",
			Scope: access.Scope{Namespace: "finance"}, Role: access.Operator, GrantedBy: "alice"})
	}); err != nil {
		t.Fatal(err)
	}
	return pool
}

// A namespace made before v0.3.0 is given its built-in identity once, holding no grant, and a run
// that finds none lacking gives none; a namespace no principal can be written with is passed over
// rather than failing the upgrade.
func TestNamespacesMadeBeforeV030AreGivenTheirBuiltInIdentityOnce(t *testing.T) {
	pool := withServiceAccounts(t)
	long := strings.Repeat("a", 256)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		_, err := w.tx.Exec(ctx, `insert into namespaces (name) values ($1)`, long)
		return err
	})

	var given, again []string
	var accounts []ServiceAccount
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		var err error
		if given, err = w.GiveBuiltInIdentities(ctx); err != nil {
			return err
		}
		if again, err = w.GiveBuiltInIdentities(ctx); err != nil {
			return err
		}
		accounts, err = w.ServiceAccountsIn(ctx, []string{"finance", "team-ops", long})
		return err
	})
	if !slices.Equal(given, []string{"finance", "team-ops"}) || len(again) != 0 {
		t.Errorf("the namespaces were given built-in identities %q, and a second run %q", given, again)
	}
	var names []string
	for _, s := range accounts {
		names = append(names, s.Principal()+" by "+s.CreatedBy)
	}
	if want := []string{"finance/agentiik by ", "finance/nightly-sync by alice", "team-ops/agentiik by ", "team-ops/deploy by alice"}; !slices.Equal(names, want) {
		t.Errorf("the service accounts are %q, want %q", names, want)
	}
	for _, ns := range []string{"finance", "team-ops"} {
		if err := pool.In(t.Context(), ns, func(ctx context.Context, n *NS) error {
			grants, err := n.AccessGrantsFor(ctx, access.Principal{Ref: ns + "/" + BuiltIn}, "", time.Now())
			if len(grants) != 0 {
				t.Errorf("%s/agentiik was given %+v", ns, grants)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// A service account is removed with its tokens and its grants, and the built-in identity is not
// removed that way at all: it goes with its namespace.
func TestAServiceAccountIsRemovedWithWhatItHoldsAndTheBuiltInIdentityIsNot(t *testing.T) {
	pool := withServiceAccounts(t)
	var gone ServiceAccount
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if _, err := w.GiveBuiltInIdentities(ctx); err != nil {
			return err
		}
		var err error
		gone, err = w.RemoveServiceAccount(ctx, "finance", "nightly-sync")
		return err
	})
	if gone.Principal() != "finance/nightly-sync" || gone.CreatedBy != "alice" || gone.CreatedAt.IsZero() {
		t.Errorf("the removal answered %+v", gone)
	}
	var tokens, grants int
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		return w.tx.QueryRow(ctx, `select (select count(*) from api_tokens), (select count(*) from grants)`).Scan(&tokens, &grants)
	})
	if tokens != 0 || grants != 0 {
		t.Errorf("%d tokens and %d grants outlived the service account that held them", tokens, grants)
	}

	for what, c := range map[string]struct {
		namespace, name string
		want            error
	}{
		"the built-in identity":           {"finance", BuiltIn, ErrBuiltIn},
		"one removed already":             {"finance", "nightly-sync", ErrNoPrincipal},
		"one of another namespace's name": {"finance", "deploy", ErrNoPrincipal},
	} {
		err := pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
			_, err := w.RemoveServiceAccount(ctx, c.namespace, c.name)
			return err
		})
		if !errors.Is(err, c.want) {
			t.Errorf("removing %s was answered %v, want %v", what, err, c.want)
		}
	}
	var left []ServiceAccount
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		var err error
		left, err = w.ServiceAccounts(ctx, "finance")
		return err
	})
	if len(left) != 1 || left[0].Name != BuiltIn {
		t.Errorf("finance holds %+v once nightly-sync is gone", left)
	}
}

// A service account signs in to no client: every record a person proves who they are with, or
// comes in through, is a user's, and each is refused a service account. A sign-in path added later,
// the passkey ceremonies and the password sign-in among them, joins this list with what it writes,
// so that a service account holding a way in other than its tokens fails here first.
func TestAServiceAccountSignsInWithNothing(t *testing.T) {
	pool := withServiceAccounts(t)
	const account = "finance/nightly-sync"
	now := time.Now().UTC().Truncate(time.Microsecond)
	for what, write := range map[string]func(context.Context, *Wide) error{
		"a passkey": func(ctx context.Context, w *Wide) error {
			return w.AddCredential(ctx, Credential{ID: "c2VydmljZQ", Login: account, Type: CredentialPasskey,
				PublicKey: []byte{0xa5}, AAGUID: make([]byte, 16)})
		},
		"a password": func(ctx context.Context, w *Wide) error {
			return w.AddCredential(ctx, Credential{ID: "password-sync", Login: account, Type: CredentialPassword, PasswordHash: "h"})
		},
		"a TOTP": func(ctx context.Context, w *Wide) error {
			return w.AddCredential(ctx, Credential{ID: "totp-sync", Login: account, Type: CredentialTOTP, TOTPSealed: []byte{1}})
		},
		"a session": func(ctx context.Context, w *Wide) error {
			return w.OpenSession(ctx, Session{Hash: valueHash("session"), Login: account, Credential: "password-sync",
				CreatedAt: now, IdleExpiresAt: now.Add(time.Hour)})
		},
		"an enrolment link": func(ctx context.Context, w *Wide) error {
			_, err := w.IssueEnrolmentCode(ctx, EnrolmentCode{Hash: valueHash("link"), Login: account, Kind: EnrolmentNewUser,
				IssuedBy: "alice", IssuedAt: now, ExpiresAt: now.Add(time.Hour)})
			return err
		},
		"a recovery code": func(ctx context.Context, w *Wide) error {
			_, err := w.IssueEnrolmentCode(ctx, EnrolmentCode{Hash: valueHash("recovery"), Login: account, Kind: EnrolmentRecovery,
				IssuedBy: "alice", IssuedAt: now, ExpiresAt: now.Add(time.Hour)})
			return err
		},
		"a sign-in": func(ctx context.Context, w *Wide) error {
			return w.SignedIn(ctx, account, now)
		},
	} {
		err := pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error { return write(ctx, w) })
		if !errors.Is(err, ErrNoPrincipal) {
			t.Errorf("%s of %s was answered %v", what, account, err)
		}
	}
	var held int
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		return w.tx.QueryRow(ctx,
			`select (select count(*) from credentials) + (select count(*) from sessions) + (select count(*) from enrolment_codes)`).Scan(&held)
	})
	if held != 0 {
		t.Errorf("%d credentials, sessions and enrolment codes were written for a service account", held)
	}
}

// A run nobody asked for, a schedule, a webhook or an event, is attributed to its namespace's
// built-in identity, and one naming anybody else is refused and not written; a run somebody asked
// for is theirs.
func TestARunNobodyAskedForIsTheBuiltInIdentitys(t *testing.T) {
	pool, _ := created(t)
	triggeredBy := func(id agk.RunID) string {
		t.Helper()
		var by *string
		if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
			return ns.tx.QueryRow(ctx, `select triggered_by from runs where id = $1`, string(id)).Scan(&by)
		}); err != nil {
			t.Fatal(err)
		}
		if by == nil {
			return ""
		}
		return *by
	}
	for kind, want := range map[agk.TriggerKind]string{
		agk.TriggerSchedule:  "finance/agentiik",
		agk.TriggerWebhook:   "finance/agentiik",
		agk.TriggerEvent:     "finance/agentiik",
		agk.TriggerManual:    "alice",
		agk.TriggerMCP:       "alice",
		agk.TriggerTerraform: "alice",
	} {
		r := runOf(kind)
		if err := createIn(t, pool, "finance", r); err != nil {
			t.Fatalf("a %s run: %s", kind, err)
		}
		if got := triggeredBy(r.ID); got != want {
			t.Errorf("a %s run is attributed to %q, want %q", kind, got, want)
		}
	}
	builtIn := runOf(agk.TriggerSchedule)
	builtIn.TriggeredBy = "finance/agentiik"
	if err := createIn(t, pool, "finance", builtIn); err != nil {
		t.Errorf("a scheduled run naming the built-in identity was answered %s", err)
	}

	editor := runOf(agk.TriggerSchedule)
	editor.TriggeredBy = "alice"
	err := createIn(t, pool, "finance", editor)
	if err == nil || !strings.Contains(err.Error(), "attributed to finance/agentiik") {
		t.Errorf("a scheduled run attributed to the last editor was answered %v", err)
	}
	var runs int
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.tx.QueryRow(ctx, `select count(*) from runs where id = $1`, string(editor.ID)).Scan(&runs)
	}); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Error("a run refused its attribution was written")
	}
}

// The tokens a principal holds are counted while they open something, revoked and expired ones
// left out, and the count is taken under a lock on the principal: a second count waits for the
// transaction holding the first, and then sees what it minted.
func TestLiveTokensAreCountedOneMintAtATime(t *testing.T) {
	pool := withServiceAccounts(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	const account = "finance/nightly-sync"
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		// One expired a minute ago, and one revoked, beside the live one it was seeded with.
		for id, until := range map[string]time.Time{"01JQ3M8X": now.Add(-time.Minute), "01JQ3M8Y": now.Add(time.Hour)} {
			if err := w.MintToken(ctx, APIToken{ID: id, Hash: valueHash(id), Principal: account,
				CreatedAt: now.Add(-time.Hour), ExpiresAt: until}); err != nil {
				return err
			}
		}
		_, err := w.RevokeToken(ctx, account, "01JQ3M8Y", now)
		return err
	})

	counted := make(chan int, 1)
	release := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
			live, err := w.LiveTokens(ctx, account, now)
			if err != nil {
				return err
			}
			counted <- live
			<-release
			return w.MintToken(ctx, APIToken{ID: "01JQ3M8Z", Hash: valueHash("w"), Principal: account, CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
		})
	}()
	if live := <-counted; live != 1 {
		t.Errorf("the first count is %d, and one token of the three opens anything", live)
	}
	second := make(chan int, 1)
	go func() {
		pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
			live, err := w.LiveTokens(ctx, account, now)
			second <- live
			return err
		})
	}()
	// The first transaction is let go whatever the second did, so that a count taken without
	// the lock fails the test rather than leaving the first holding its connection for good.
	select {
	case live := <-second:
		t.Errorf("a second count answered %d while the first transaction held the principal", live)
		close(release)
	case <-time.After(300 * time.Millisecond):
		close(release)
		if live := <-second; live != 2 {
			t.Errorf("the second count is %d once the first minted one more", live)
		}
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	err := pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
		_, err := w.LiveTokens(ctx, "finance/nobody", now)
		return err
	})
	if !errors.Is(err, ErrNoPrincipal) {
		t.Errorf("counting the tokens of nobody was answered %v", err)
	}
}
