package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/config"
)

// recoverVerb is agentiik-api recover LOGIN: the break-glass line of recovery, "for the day every
// administrator loses their authenticator", which issues the administrator LOGIN a recovery code
// and prints the link that carries it, once.
//
// It takes no credential, and that is the point of it: every other way to a recovery code goes
// through an administrator who can sign in, and on that day there is none. What it takes instead is
// the installation's host, where it runs with the API's own settings, as namespace create does, so
// that the network brings nobody to it; whoever holds the host could write the code's row by hand,
// and this does it for them, audited, as enrolment.issue by installation. The link is handed over
// by whoever ran it, as an administrator hands over the one the API answers, and never by mail.
func recoverVerb(ctx context.Context, lookup config.Lookup, login string, now time.Time, stdout, stderr io.Writer) int {
	// The login is checked before the settings are read, so that one no user can have is refused
	// as that wherever the verb is run.
	if err := api.LoginName(login); err != nil {
		fmt.Fprintf(stderr, "%s recover: %s\n", program, err)
		return exitFailed
	}
	c, err := config.ReadRecovery(lookup)
	if err != nil {
		fmt.Fprintf(stderr, "%s recover: the configuration refuses the start:\n%s\n", program, err)
		return exitFailed
	}
	if err := recoverAdministrator(ctx, c, login, now, stdout); err != nil {
		fmt.Fprintf(stderr, "%s recover: %s\n", program, err)
		return exitFailed
	}
	return exitStopped
}

// recoverAdministrator issues login a recovery code as the installation, and prints the link that
// carries it, with the minute it lapses at.
func recoverAdministrator(ctx context.Context, c config.Recovery, login string, now time.Time, stdout io.Writer) error {
	pool, err := db.Open(ctx, c.Database.ConnString())
	if err != nil {
		return fmt.Errorf("the database %s names could not be reached as %s: %w", config.DatabaseURL, c.Database.Role, err)
	}
	defer pool.Close()

	code, err := api.BreakGlass(ctx, pool, c.PublicURL, login, now)
	switch {
	case errors.Is(err, db.ErrNoPrincipal):
		return fmt.Errorf("there is no user %s, so no recovery code was issued", login)
	case errors.Is(err, api.ErrNotAdministrator):
		return fmt.Errorf("%s is not an administrator, so no recovery code was issued: this recovers an administrator, who then issues a user theirs with agk user recover", login)
	case err != nil:
		return fmt.Errorf("no recovery code was issued: %w", err)
	}
	fmt.Fprintf(stdout, "%s, an administrator, may open this link once, before %s, to enrol a new passkey, or a password where the installation allows one; any recovery code issued them before no longer works. Hand it over yourself:\n",
		login, code.ExpiresAt.UTC().Format("15:04 UTC"))
	fmt.Fprintln(stdout, code.Link)
	return nil
}
