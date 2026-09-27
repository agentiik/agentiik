// Package accesstest stands the access fixture up on an installation and holds the documentation's
// access rules against it: who holds what where, which route lets whom through and refuses the rest
// with what, and that a principal holding nothing in a namespace cannot tell it from one that does
// not exist.
//
// The fixture is built through the installation's public routes alone, as its administrator and its
// users would build it, and never by writing rows, so that what is held against the rules is what an
// installation would hold: the bootstrap token creates the administrator, whose first sign-in ends
// it, and everything after is asked of the API by whoever the documentation lets ask it.
//
//	carol                 an administrator of the installation, owning finance through its record
//	alice                 a user, in team-finance, editor of hr/onboarding by a grant of her own
//	bob                   a user, owning hr through its record, and holding only denies in finance
//	team-finance          a group, alice in it
//	finance/nightly-sync  a service account of finance, with a token its owner minted
//
//	finance                    team-finance          editor
//	finance                    finance/nightly-sync  viewer, until the lapse
//	finance/payroll            finance/nightly-sync  operator
//	finance/monthly-invoicing  alice                 deny run:read_data
//	finance                    bob                   deny run:read_data
//	finance/payroll            bob                   deny workflow:run
//	hr                         team-finance          deny workflow:run
//	hr/onboarding              alice                 editor
//	hr/onboarding              alice                 deny workflow:read, until the lapse
//
// The first and the fourth are the figure of the page's Scopes and resolution: team-finance edits
// every workflow of finance, and the deny takes run:read_data from alice on monthly-invoicing
// alone. bob's two denies give him nothing: a deny alone is no grant, and finance is a namespace he
// holds nothing in whoever asks. Each user also owns the personal namespace their first sign-in
// made, and each namespace holds its built-in identity, NS/agentiik, which holds nothing. finance
// holds monthly-invoicing and payroll, hr holds onboarding and offboarding, each namespace one
// secret, and three of the workflows a run each, started by alice, by finance/nightly-sync and by
// bob.
//
// The lapse is an hour after the installation's clock read when the fixture was built: a grant, a
// deny and one of alice's API tokens end then. A test serving the installation in its own process
// holds its clock at the instant before and at the instant itself, which is how a lapse is seen to
// happen at its instant and not after it; the test installation of package e2e runs on the wall
// clock and is held to the fixture as it stands before.
//
// Each user signs in for the first time from their enrolment link: with a passkey, from a software
// authenticator, where the installation is addressed by a name, and with a password where it is
// addressed by an IP address, where no passkey signs anybody in. Either opens a full session, which
// the fixture keeps as the user's browser, and mints the user's API tokens from.
package accesstest
