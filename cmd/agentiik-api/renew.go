package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/internal/config"
	"github.com/nats-io/jwt/v2"
)

// controlPlaneName is the name the credential the API renews carries, as the one bus-init mints
// does, which an operator reads in the bus's list of connections.
const controlPlaneName = "agentiik-control-plane"

// renewer answers what renews the control plane's credential in path, under the account issuer
// signs for, valid controlPlaneLife from now: the API holds the account seed, and is the one
// program running every day that may mint it. It answers when the new one expires.
//
// path is the file AGK_BUS_CREDENTIALS_FILE names, which the controller reads too, from the same
// directory mounted read only, so the one write reaches both, and each takes the new credential
// when the bus drops the old one at its expiry. Nothing renews where path is empty, which only a
// test's settings are.
func renewer(issuer *bus.Issuer, path string, now func() time.Time) func() (time.Time, error) {
	if path == "" {
		return nil
	}
	return func() (time.Time, error) {
		control, err := issuer.ForControlPlane(controlPlaneName, now().Add(controlPlaneLife))
		if err != nil {
			return time.Time{}, err
		}
		content, err := jwt.FormatUserConfig(control.JWT, []byte(control.Seed))
		if err != nil {
			return time.Time{}, fmt.Errorf("the control plane's credential could not be written out: %w", err)
		}
		if err := replaceLike(path, content); err != nil {
			return time.Time{}, err
		}
		return control.ExpiresAt, nil
	}
}

// replaceLike puts content at path in one step, written beside it, given the mode and the owner of
// the file it replaces, and renamed over it: the controller reading it at that moment reads the
// old credential or the new one, never half of each, and reads it as it read the old one.
//
// The owner is the API's own, since the file is one the API reads and the start refuses a secret's
// file that anybody but its owner may read; it is given back all the same where the API runs as
// root. A group that cannot be given back is left, since nobody but the owner reads the file.
func replaceLike(path string, content []byte) error {
	old, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%s could not be looked at: %w", path, err)
	}
	if !old.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file, and the control plane's credential is one", path)
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("a file beside %s could not be created, and renewing the credential writes one there: %w", path, err)
	}
	temporary := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(temporary)
		return fmt.Errorf("%s could not be written: %w", path, err)
	}
	// The mode before the content, so that the secret is never readable by anybody else, even for
	// the moment between the two.
	if err := f.Chmod(old.Mode().Perm()); err != nil {
		return fail(err)
	}
	if owner, ok := old.Sys().(*syscall.Stat_t); ok {
		if err := f.Chown(int(owner.Uid), int(owner.Gid)); err != nil {
			if mine, _ := f.Stat(); mine == nil || mine.Sys().(*syscall.Stat_t).Uid != owner.Uid {
				return fail(fmt.Errorf("it is owned by uid %d, which the renewed one could not be given, and a credential its reader cannot open is none: %w", owner.Uid, err))
			}
		}
	}
	if _, err := f.Write(content); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(temporary)
		return fmt.Errorf("%s could not be written: %w", path, err)
	}
	if err := os.Rename(temporary, path); err != nil {
		os.Remove(temporary)
		return fmt.Errorf("%s could not be put in place: %w", path, err)
	}
	// The directory's entries as well, or a power cut after the renewal was said could come back
	// to the old one, which expires within the fortnight.
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("%s could not be opened to be written to the disk: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("%s could not be written to the disk: %w", dir, err)
	}
	return nil
}

// renewExpired renews, before the settings are read, a control plane credential that has expired
// already, which the settings would refuse the start on.
//
// That is an installation whose API was down past the expiry, its host off say: at the next boot
// Docker starts the API again but not init, which ran to its end once, and an API that only renewed
// once started would refuse to start on the very credential it could renew, over and over. So
// where the file AGK_BUS_CREDENTIALS_FILE names holds an expired credential and the account seed
// is in the file AGK_BUS_ACCOUNT_SEED_FILE names, readable by its owner alone as the start holds
// it, a new one is minted first; the controller, which ended at the expiry, takes it as it starts
// again. Anything else is left to the settings to refuse, saying why, so this says nothing unless
// it renewed or tried to.
func renewExpired(lookup config.Lookup, now time.Time, stderr io.Writer) {
	if lookup == nil {
		// As config reads a nil one: the process's environment.
		lookup = os.LookupEnv
	}
	path, _ := lookup(config.BusCredentialsFile)
	seedPath, _ := lookup(config.BusAccountSeedFile)
	url, _ := lookup(config.BusURL)
	if !filepath.IsAbs(path) || !filepath.IsAbs(seedPath) || url == "" {
		return
	}
	expires, err := controlPlaneExpiry(path)
	if err != nil || expires.IsZero() || expires.After(now) {
		return
	}
	issuer, err := seedIssuer(seedPath, url)
	if err == nil {
		var renewed time.Time
		if renewed, err = renewer(issuer, path, func() time.Time { return now })(); err == nil {
			fmt.Fprintf(stderr, "%s: renewed the control plane's bus credential, which expired at %s, before starting: it now expires at %s\n", program, expires.UTC().Format(time.RFC3339), renewed.UTC().Format(time.RFC3339))
			return
		}
	}
	fmt.Fprintf(stderr, "%s: the control plane's bus credential expired at %s, and could not be renewed: %s\n", program, expires.UTC().Format(time.RFC3339), err)
}

// seedIssuer is an issuer on the account seed in path, held to what the start holds the file to: a
// regular file its owner alone may read.
func seedIssuer(path, url string) (*bus.Issuer, error) {
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return nil, err
	case !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0:
		return nil, errors.New(path + " is not a regular file its owner alone may read, as the account seed is")
	}
	content, err := readRegular(path)
	if err != nil {
		return nil, err
	}
	account, err := jwt.ParseDecoratedNKey(content)
	if err != nil {
		return nil, fmt.Errorf("%s holds no seed: %w", path, err)
	}
	seed, err := account.Seed()
	if err != nil {
		return nil, fmt.Errorf("%s holds no seed: %w", path, err)
	}
	return bus.NewIssuer(string(seed), url)
}
