package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/config"
)

// bootstrapToken keeps the SHA-256 of the bootstrap token set in the database, where the API reads
// it, and never the token. It is the v0.2 operator token under its v0.3.0 name, set where it was.
// init calls it at every run, and so does migrate, which an installation that never runs init,
// Homebrew's or one put together by hand, runs at every start in its place; verb is which of the
// two it is, for what it says.
//
// What it says of the token names no variable, since the one a person sets is not always
// AGK_OPERATOR_TOKEN: a Compose file hands it on from a variable of its own, and the
// installation's settings are wherever that file reads them.
//
// A token set is hashed at every run, and a hash that differs from the one kept replaces it, so a
// token changed in .env is the one the API takes from its next request, until the first
// administrator has enrolled. That ends the bootstrap token for good: from then on a token set is
// ignored, and it says so at every run while one is, which is no error, since the Compose file
// still requires the line. With none set, the hash kept is kept. Where none is kept either,
// nobody can create the first administrator, and it says so; it mints none, since a token printed
// in a log is read by everybody the log reaches.
//
// imported is migrate's, and nil for init: the hash a v0.2 installation kept in the file
// AGK_OPERATOR_TOKEN_FILE names, which an upgrade that runs no init has nowhere else. It is called
// only where no token is set, no hash is kept and the bootstrap has not ended, so the file is read
// once, at the run that imports it, and never again: from then on a hash is kept, until the
// bootstrap ends, which is for good. A file that is not there imports nothing and is no error, and
// a file it refuses fails the run and writes nothing. Neither verb writes or removes the file, and
// init never reads the one a v0.2 init wrote in its directory, since a Compose installation's
// token is set in .env, which an upgrade keeps.
func bootstrapToken(ctx context.Context, pool *db.Pool, verb string, token config.Secret, imported func() ([]byte, error), out io.Writer) error {
	var hash []byte
	if token != "" {
		sum := sha256.Sum256([]byte(token))
		hash = sum[:]
	}
	var said []string
	var refused error
	nobody := "no bootstrap token is set and none is stored, so nobody can create the first administrator: set one where the installation's settings are, and run " + verb + " again"
	err := pool.Installation(ctx, db.Identity, func(ctx context.Context, w *db.Wide) error {
		if hash == nil {
			kept, err := w.Bootstrap(ctx)
			switch {
			case err != nil:
				return err
			case kept.Ended():
				return nil
			case kept.TokenHash != nil:
				said = append(said, "kept the hash of the bootstrap token stored, since none is set")
				return nil
			case imported == nil:
				said = append(said, nobody)
				return nil
			}
			v02, err := imported()
			switch {
			case err != nil:
				refused = err
				return err
			case v02 == nil:
				said = append(said, "imported no hash of the v0.2 operator token, since the file "+config.OperatorTokenFile+" names is not there", nobody)
				return nil
			}
			done, err := w.ImportBootstrapToken(ctx, v02)
			switch {
			case err != nil:
				return err
			case done:
				said = append(said, "imported the hash of the v0.2 operator token from the file "+config.OperatorTokenFile+" names, once: it is the bootstrap token's, which the API takes from its next request, and the file is not read again")
			default:
				// Another run kept a hash or ended the bootstrap between the read above and
				// this write, and what it wrote stands.
				said = append(said, "imported no hash of the v0.2 operator token, since another run wrote the bootstrap token's state a moment ago")
			}
			return nil
		}
		changed, err := w.SetBootstrapToken(ctx, hash)
		switch {
		case errors.Is(err, db.ErrBootstrapEnded):
			said = append(said, "ignored the bootstrap token set: it ended when the first administrator enrolled a passkey, and the API refuses it. That is no error, and the line may stay where the installation's settings are")
			return nil
		case err != nil:
			return err
		case changed:
			said = append(said, "wrote the hash of the bootstrap token set, which the API takes from its next request")
		default:
			said = append(said, "kept the hash of the bootstrap token set")
		}
		return nil
	})
	switch {
	case refused != nil:
		return fmt.Errorf("the hash of the v0.2 operator token was not imported: %w", refused)
	case err != nil:
		return fmt.Errorf("the bootstrap token's hash could not be kept: %w", err)
	}
	for _, line := range said {
		fmt.Fprintln(out, line)
	}
	return nil
}
