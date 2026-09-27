package e2e

import (
	"testing"
	"time"

	"github.com/agentiik/agentiik/internal/accesstest"
)

// The fact v0.3.0 holds, on the test installation: "A principal with no permission on a namespace
// cannot establish that it exists: not through the API, and not through an error that distinguishes
// absent from forbidden."
//
// The access fixture of package accesstest is stood up on the installation's stack through its
// terminator, as anybody reaches it: the bootstrap token creates carol, whose first sign-in ends it,
// and every user signs in with a password, since the installation is addressed by an IP address,
// where no passkey signs anybody in. What each principal holds is read back from GET /api/v1/me as
// the page's rules say, and then bob, who owns hr and holds nothing in finance, asks about finance and
// about nowhere every way the API offers, with his token and from his browser, as alice does through
// her token narrowed to hr: every answer about the one is the answer about the other, and no listing
// names anything of finance's. The installation runs on the wall clock, so the fixture is held as it
// stands before its lapse; where AGENTIIK_TEST_TIMING is set, the refusals a prober would time first
// are timed as well, across the terminator.
func TestAPrincipalHoldingNothingInANamespaceCannotEstablishThatItExistsOnTheInstallation(t *testing.T) {
	in := Stand(t)
	f := accesstest.Build(t, accesstest.Installation{
		PublicURL: in.PublicURL, Client: in.client, Bootstrap: in.token,
		Now: func() time.Time { return time.Now().UTC() },
	})
	f.Resolves(t, false)
	f.Unknowable(t, f.Bob, f.BobsBrowser, f.AliceForHR)
}
