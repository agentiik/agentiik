package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// The local profile: the API token agk login stored for each installation, which agk presents to
// that installation wherever AGENTIIK_TOKEN is not set.
//
// It is one file, profile.json, in a directory agentiik of the user's configuration directory,
// os.UserConfigDir: ~/.config/agentiik on Linux, where XDG_CONFIG_HOME moves it, and
// ~/Library/Application Support/agentiik on macOS. It holds one entry per installation, by the
// address --server or AGENTIIK_SERVER names it with, so that AGENTIIK_SERVER picks the token as it
// picks the installation, and signing in to a second installation keeps the first one's.
//
// The file holds bearer tokens, so it is written 0600 in a directory 0700, whatever the umask, and
// read only while nobody but its owner may read it or write it: a profile the group or anybody else
// may read is a token already leaked, and one they may write is a token somebody else chose.
// It is written whole, to a file beside it that then takes its name, so that an agk interrupted
// halfway leaves the profile it found rather than half of one. Two agk login at the same moment
// each keep the profile they read and add their own entry, and the one that writes last wins: a
// person signing in to two installations at once signs in to the first again.

// profileFile and profileDir are where the profile is, under the user's configuration directory.
const (
	profileDir  = "agentiik"
	profileFile = "profile.json"
)

// profile is the file's content.
type profile struct {
	// Installations are the tokens agk login stored, by installation address.
	Installations map[string]storedToken `json:"installations"`
}

// storedToken is one installation's entry: the token, and what agk login was answered of it, for
// agk logout to revoke it by its identifier and for agk to say it has expired before sending it.
type storedToken struct {
	Token     string    `json:"token"`
	ID        string    `json:"token_id"`
	Principal string    `json:"principal"`
	ExpiresAt time.Time `json:"expires_at"`
}

// profilePath is where the profile is, and false where this agk keeps none: an Env given no
// configuration directory, as a test's is, so that no test reads the profile of whoever runs it.
func profilePath(e Env) (string, bool, error) {
	if e.ConfigDir == nil {
		return "", false, nil
	}
	dir, err := e.ConfigDir()
	if err != nil {
		return "", false, fmt.Errorf("the local profile has no directory to live in: %w", err)
	}
	return filepath.Join(dir, profileDir, profileFile), true, nil
}

// installationKey is how an installation's address is written as a key of the profile: its scheme
// and host in lower case, as a URL compares them, and its path without the slash it may end with,
// as every path agk sends is appended to it.
func installationKey(where string) string {
	u, err := url.Parse(where)
	if err != nil {
		return strings.TrimRight(where, "/")
	}
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	u.Path, u.RawPath = strings.TrimRight(u.Path, "/"), ""
	return u.String()
}

// readProfile reads the profile at path. One that is not there is an empty one; one that others
// than its owner may read or write, or that does not read, is refused, saying what to do.
func readProfile(path string) (profile, error) {
	p := profile{Installations: map[string]storedToken{}}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return profile{}, fmt.Errorf("the local profile %s could not be read: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return profile{}, fmt.Errorf("the local profile %s is not a file: remove it, and sign in again with agk login", path)
	}
	if loose := info.Mode().Perm() & 0o077; loose != 0 && runtime.GOOS != "windows" {
		return profile{}, fmt.Errorf("the local profile %s holds API tokens and others than you may read or write it (mode %04o): chmod 600 it, or remove it and sign in again with agk login", path, info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return profile{}, fmt.Errorf("the local profile %s could not be read: %w", path, err)
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return profile{}, fmt.Errorf("the local profile %s does not read as agk writes it: %v. Remove it, and sign in again with agk login", path, err)
	}
	if p.Installations == nil {
		p.Installations = map[string]storedToken{}
	}
	return p, nil
}

// writeProfile writes the profile at path whole: its directory made 0700, or brought to it, and the
// file written 0600 beside it before it takes the profile's name.
func writeProfile(path string, p profile) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("the directory of the local profile, %s, could not be made: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("the directory of the local profile, %s, could not be kept to its owner: %w", dir, err)
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	// CreateTemp opens a name nobody holds, 0600, so that nothing somebody put there first is
	// written through.
	f, err := os.CreateTemp(dir, ".profile-*.json")
	if err != nil {
		return fmt.Errorf("the local profile could not be written in %s: %w", dir, err)
	}
	written := f.Name()
	defer os.Remove(written)
	_, err = f.Write(append(raw, '\n'))
	if err == nil {
		err = f.Chmod(0o600)
	}
	if err == nil {
		err = f.Sync()
	}
	if closed := f.Close(); err == nil {
		err = closed
	}
	if err == nil {
		err = os.Rename(written, path)
	}
	if err != nil {
		return fmt.Errorf("the local profile %s could not be written: %w", path, err)
	}
	return nil
}

// profileToken is the token agk login stored for the installation where, and false where it stored
// none, or this agk keeps no profile. One past its expiry is refused, saying so, rather than sent to
// be refused with a sentence that could not say why.
func profileToken(e Env, where string) (string, bool, error) {
	path, kept, err := profilePath(e)
	if err != nil || !kept {
		return "", false, err
	}
	p, err := readProfile(path)
	if err != nil {
		return "", false, err
	}
	stored, found := p.Installations[installationKey(where)]
	switch {
	case !found || stored.Token == "":
		return "", false, nil
	case !stored.ExpiresAt.IsZero() && !e.now().Before(stored.ExpiresAt):
		return "", false, fmt.Errorf("the token agk login stored for %s expired at %s: sign in again with agk login, or set %s", where, stored.ExpiresAt.UTC().Format(time.RFC3339), tokenVariable)
	}
	return stored.Token, true, nil
}
