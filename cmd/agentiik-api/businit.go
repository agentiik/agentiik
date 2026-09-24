package main

import (
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/internal/config"
)

// controlPlaneLife is how long the control plane's bus credential is minted for: ninety days.
//
// It expires, because one that never does is one a stolen disk still holds. Ninety days rather than
// the hour a runner's lasts, because nothing renews it by itself: somebody runs bus-credential and
// restarts the API and the controller, and a chore that comes round every quarter, announced two
// weeks ahead, is one a person does rather than one that is automated badly.
const controlPlaneLife = 90 * 24 * time.Hour

// busInit is agentiik-api bus-init DIR: the installation's NATS operator, application account and
// system account, created once, and the three files that hand them out, written in DIR.
func busInit(dir string, now time.Time, stdout, stderr io.Writer) int {
	dir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintf(stderr, "%s bus-init: %s\n", program, err)
		return exitFailed
	}
	in, err := bus.NewInstallation(dir, now.Add(controlPlaneLife))
	if err != nil {
		fmt.Fprintf(stderr, "%s bus-init: %s\n", program, err)
		return exitFailed
	}
	fmt.Fprintf(stdout, `The installation's bus identity is in %s.

  operator         %s
  account          %s
  system account   %s

%s is the NATS server's: include it in the server's own configuration, beside its listen address, its TLS certificates and a jetstream block.
%s is the API's alone: %s=%s
%s is the API's and the controller's: %s=%s

The control plane's credential expires at %s. %s bus-credential %s mints a new one under the same account, and the API warns from %d days before.
`,
		dir, in.Operator, in.Account, in.System,
		in.Accounts,
		in.AccountSeed, config.BusAccountSeedFile, in.AccountSeed,
		in.ControlPlane, config.BusCredentialsFile, in.ControlPlane,
		now.Add(controlPlaneLife).UTC().Truncate(time.Second).Format(time.RFC3339), program, dir, int(credentialWarning.Hours()/24))
	return exitStopped
}

// busCredential is agentiik-api bus-credential DIR: a new control plane credential under the account
// DIR holds, in place of the one it holds, with the operator, the accounts and the server's
// configuration left as they are, so the identity and every task queued under it stay.
func busCredential(dir string, now time.Time, stdout, stderr io.Writer) int {
	dir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintf(stderr, "%s bus-credential: %s\n", program, err)
		return exitFailed
	}
	path, expires, err := bus.RenewControlPlane(dir, now.Add(controlPlaneLife))
	if err != nil {
		fmt.Fprintf(stderr, "%s bus-credential: %s\n", program, err)
		return exitFailed
	}
	fmt.Fprintf(stdout, `%s holds a new control plane credential, which expires at %s.

The API and the controller read it when they start, so restart both, wherever each holds its copy of it. The credential it replaced works until its own expiry.
`, path, expires.UTC().Format(time.RFC3339))
	return exitStopped
}
