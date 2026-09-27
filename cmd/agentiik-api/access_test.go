package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/internal/accesstest"
)

// The access fixture of package accesstest, stood up through the routes serve builds on a real
// PostgreSQL, and held to every rule the page's Identity and access control sets: what each
// principal holds, through each credential; which route lets whom through and refuses the rest with
// what; and that a principal holding nothing in a namespace cannot tell it from one that does not
// exist.

// accessInstallation is an installation serve built, on a database and a bus of the test's own,
// telling time by clock, at publicURL where it is not empty.
func accessInstallation(t *testing.T, clock *accesstest.Clock, publicURL string) (*installation, settings) {
	t.Helper()
	database := freshDatabase(t)
	if err := migrate(t.Context(), database, io.Discard); err != nil {
		t.Fatal(err)
	}
	bootstrapped(t, database.Application)
	dir := filepath.Join(t.TempDir(), "bus")
	if code := run(t.Context(), []string{"bus-init", dir}, empty, io.Discard, io.Discard); code != exitStopped {
		t.Fatal("bus-init failed")
	}
	s := servingSettings(t, database.Application, dir, natsFrom(t, dir))
	s.now = clock.Now
	if publicURL != "" {
		s.PublicURL = publicURL
	}
	in, err := open(t.Context(), s, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(in.close)
	return in, s
}

// accessFixture is the fixture, stood up on an installation serve built at publicURL, or at the
// name servingSettings gives it where publicURL is empty, and the clock it tells time by.
func accessFixture(t *testing.T, publicURL string) (*accesstest.Fixture, *installation, *accesstest.Clock) {
	t.Helper()
	clock := &accesstest.Clock{}
	in, s := accessInstallation(t, clock, publicURL)
	f := accesstest.Build(t, accesstest.Installation{
		PublicURL: s.PublicURL, Client: accesstest.Serve(in.router), Bootstrap: theToken, Now: clock.Now,
	})
	return f, in, clock
}

// Every resolution rule and every refusal, on the fixture: what GET /api/v1/me answers each asker,
// each principal through a token narrowing nothing, through its browser's session and through the
// narrowed tokens alice and carol hold; every route, as every asker, about everything of the fixture
// it can name, let through where the asker holds what the page says the route needs and refused
// otherwise with the answer its scope refuses with; each listing holding what its asker may list;
// and the refusals no one route is about. Asked with the installation's clock held at the instant
// before the lapse, where finance/nightly-sync still views finance, alice's deny still takes
// workflow:read from her on hr/onboarding and her lapsing token still opens, and again at the lapse
// itself, where none of the three holds any more.
func TestTheAccessFixtureHoldsEveryResolutionRuleAndRefusal(t *testing.T) {
	f, in, clock := accessFixture(t, "")

	// The page's figure, as GET /api/v1/me answers alice about monthly-invoicing.
	var me api.Me
	a, err := f.Ask(t.Context(), "GET", "/api/v1/me", f.Alice, nil)
	if err != nil || a.Status != http.StatusOK {
		t.Fatalf("GET /api/v1/me as alice answered %d: %s %v", a.Status, a.Body, err)
	}
	decodeInto(t, a.Body, &me)
	figure := []api.Permission{api.WorkflowRead, api.WorkflowRun, api.WorkflowWrite, api.RunRead, api.SecretUse, api.SecretWrite}
	if got := me.Permissions["finance/monthly-invoicing"]; !slices.Equal(got, figure) {
		t.Errorf("alice holds %v on finance/monthly-invoicing, and the page's figure %v", got, figure)
	}

	f.Refuses(t)

	clock.Hold(f.Lapse.Add(-time.Microsecond))
	t.Run("at the instant before the lapse", func(t *testing.T) {
		f.Resolves(t, false)
		f.Probe(t, in.router.Routes(), false)
	})
	clock.Hold(f.Lapse)
	t.Run("at the lapse", func(t *testing.T) {
		f.Resolves(t, true)
		f.Probe(t, in.router.Routes(), true)
	})
}

// The roadmap's v0.3.0 fact: "A principal with no permission on a namespace cannot establish that
// it exists: not through the API, and not through an error that distinguishes absent from
// forbidden." bob, who owns hr and holds nothing in finance, asks about finance and about nowhere
// every way the API offers, with his token and from his browser, and so does alice through her token
// narrowed to hr, which reaches nothing in finance whatever she holds there: every answer about the
// one is the answer about the other, byte for byte, and no listing names anything of finance's. Where
// AGENTIIK_TEST_TIMING is set, the refusals a prober would time first take as long for the one as
// for the other, as the isolation test holds them.
func TestAPrincipalHoldingNothingInANamespaceCannotEstablishThatItExists(t *testing.T) {
	f, _, _ := accessFixture(t, "")
	f.Unknowable(t, f.Bob, f.BobsBrowser, f.AliceForHR)
}

// The fixture as package e2e stands it up, on an installation addressed by an IP address, where no
// passkey signs anybody in and every user signs in for the first time by setting a password from
// their enrolment link, which opens a full session there: carol's ends the bootstrap token, each
// principal holds what the page says through each credential, and bob cannot tell finance from
// nowhere. The test installation of package e2e runs this on its own stack, which only Linux runs.
func TestTheAccessFixtureStandsWherePasswordsAloneSignIn(t *testing.T) {
	f, _, _ := accessFixture(t, "https://127.0.0.1:8443")
	f.Resolves(t, false)
	f.Unknowable(t, f.Bob, f.BobsBrowser, f.AliceForHR)
}

// decodeInto reads JSON into into, failing the test where it does not decode.
func decodeInto(t *testing.T, body []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(body, into); err != nil {
		t.Fatalf("%s does not decode: %s", body, err)
	}
}
