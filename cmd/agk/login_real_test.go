package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/password"
)

// agk login from end to end against the real API over PostgreSQL: the browser agk opens signs alice
// in with her password on the page's origin, handing on what agk opened the page with, follows the
// redirect_to the API wrote back to agk's loopback address, and agk trades the code the API minted
// for alice's token and keeps it; agk then reaches the installation as alice with nothing in
// AGENTIIK_TOKEN, and agk logout revokes the token and forgets it.
func TestAgkLoginSignsInThroughTheRealSignInAndExchange(t *testing.T) {
	pool, _ := dbtest.Open(t)
	const publicURL = "https://agentiik.example.com"
	now := time.Now().UTC()
	err := pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		if err := w.CreateUser(ctx, db.User{Login: "alice", Profile: db.Profile{GivenName: "Alice"}}); err != nil {
			return err
		}
		hash, err := password.Hash("alice's own passphrase")
		if err != nil {
			return err
		}
		if err := w.AddCredential(ctx, db.Credential{ID: "alice-password", Login: "alice", Type: db.CredentialPassword, PasswordHash: hash}); err != nil {
			return err
		}
		bound := false
		return w.SetInstallationPolicy(ctx, db.AuthPolicy{Password: "allowed", Passkey: "optional", UserVerification: "required", DeviceBoundOnly: &bound, MinPasskeys: 2}, now)
	})
	if err != nil {
		t.Fatal(err)
	}
	principals, err := api.NewPrincipals(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := principals.AcceptSessions(publicURL); err != nil {
		t.Fatal(err)
	}
	rt, err := api.NewRouter(principals, principals.Identify)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewPasswords(rt, api.PasswordOptions{Pool: pool, PublicURL: publicURL, Identify: principals.Identify}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewTokens(rt, api.TokenOptions{Pool: pool}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewExchange(rt, api.ExchangeOptions{Pool: pool, PublicURL: publicURL}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)

	p := somebody(t)
	p.env[serverVariable] = srv.URL
	p.browse = func(page string) error {
		opened, err := url.Parse(page)
		if err != nil {
			return err
		}
		q := opened.Query()
		body, _ := json.Marshal(map[string]any{"login": "alice", "password": "alice's own passphrase",
			"terminal": map[string]string{"redirect_uri": q.Get("redirect_uri"), "code_challenge": q.Get("code_challenge")}})
		r, _ := http.NewRequest("POST", srv.URL+"/api/v1/auth/login", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", publicURL)
		answer, err := http.DefaultClient.Do(r)
		if err != nil {
			return err
		}
		defer answer.Body.Close()
		var signedIn api.SignedIn
		if err := json.NewDecoder(answer.Body).Decode(&signedIn); err != nil || answer.StatusCode != http.StatusOK {
			t.Errorf("the page's sign-in answered %d, %v", answer.StatusCode, err)
		}
		if !strings.HasPrefix(signedIn.RedirectTo, q.Get("redirect_uri")+"?code=") {
			t.Errorf("the page was sent to %q", signedIn.RedirectTo)
		}
		get(t, signedIn.RedirectTo)
		return nil
	}

	code, out, errs := p.agk(t, "login", "--label", "alice-laptop")
	if code != exitSucceeded || !strings.HasPrefix(out, "signed in to "+srv.URL+" as alice: token ") {
		t.Fatalf("agk login left with %d, printing %q: %s", code, out, errs)
	}
	id := p.kept(t).Installations[srv.URL].ID
	code, out, errs = p.agk(t, "token", "list")
	if code != exitSucceeded || !strings.HasPrefix(out, id+"  alice  expires ") || !strings.HasSuffix(out, "  alice-laptop\n") {
		t.Errorf("agk token list with the token kept left with %d, printing %q: %s", code, out, errs)
	}

	if code, out, errs := p.agk(t, "logout"); code != exitSucceeded || !strings.HasPrefix(out, "signed out of "+srv.URL+": token "+id+" is revoked") {
		t.Errorf("agk logout left with %d, printing %q: %s", code, out, errs)
	}
	var revoked bool
	if err := pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		tk, err := w.Token(ctx, id)
		revoked = !tk.RevokedAt.IsZero()
		return err
	}); err != nil || !revoked {
		t.Errorf("the token agk logout revoked is not revoked: %v", err)
	}
	if code, _, errs := p.agk(t, "token", "list"); code != exitUsage || !strings.Contains(errs, "no credential: sign in with agk login") {
		t.Errorf("agk token list once signed out left with %d: %s", code, errs)
	}
}
