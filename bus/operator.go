package bus

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
)

// The bus an installation trusts, written once.
//
// An Issuer signs with an account seed, and a server trusts what that account signs only where an
// operator it trusts signed the account. Something has to create both, or every credential the API
// mints is one no real bus accepts, which is where an installation stood while the only operator
// was the one a test made for itself.

// The files NewInstallation writes, each for the program that holds it.
const (
	// AccountsFile is the nats-server configuration: the operator the server trusts, the two
	// accounts it signed, and which of them is the system's. It holds nothing secret, and the
	// server's own configuration includes it beside what only the server's host knows, its
	// listen address, its TLS certificates and where JetStream keeps its store.
	AccountsFile = "accounts.conf"

	// AccountSeedFile is the application account's seed, which the API alone holds, as
	// AGK_BUS_ACCOUNT_SEED_FILE, and mints every runner's bus credential with.
	AccountSeedFile = "account.seed"

	// ControlPlaneFile is the control plane's user credential, the JWT and the seed as nsc
	// writes them, which the API and the controller hold as AGK_BUS_CREDENTIALS_FILE.
	ControlPlaneFile = "control-plane.creds"
)

// Installation is what NewInstallation wrote, and the keys a person reads in the server's logs.
type Installation struct {
	// The files, each a path inside the directory NewInstallation was given.
	Accounts, AccountSeed, ControlPlane string

	// Operator, Account and System are the public keys of the operator, the application
	// account every connection of the installation authenticates under, and the system account.
	Operator, Account, System string
}

// controlPlaneName is the name the control plane's credential carries, which an operator reads in
// the server's list of connections.
const controlPlaneName = "agentiik-control-plane"

// NewInstallation creates an installation's bus identity in dir: an operator, the application
// account and the system account, and the three files that hand them out. The control plane's
// credential expires at until.
//
// It writes each file once and never over another, so a second call on the same directory, or on
// one holding any of the three, is refused: an operator created twice is two sets of keys, the
// server would trust the one and the API mint with the other, and every runner would be refused at
// the bus with nothing on the API's side to say why. A directory holding some of the three and not
// the others is a creation cut off part way, which no program can start on, and the refusal says
// so rather than calling it an identity. A call refused or failing part way removes the files it
// wrote and nothing else, so it can be run again; the directory it made, if it made one, is left
// empty. An until already passed is refused before anything is written, since the API and the
// controller would refuse the credential on their first start and the identity could not then be
// created again over it. So is a directory its group or anybody else may write to, for the reason
// private gives.
//
// The operator's seed and the system account's are not written anywhere. Nothing in an installation
// signs with either after this. An account the server would trust, new or changed, is one the
// operator signed, and the server takes one pushed to it at run time only from a connection under
// the system account, so a key kept for the day an account changes is a key that could sign one and
// hand it to the server, on a disk, for every other day. The day an installation needs new
// accounts, it creates a new identity in a new directory, restarts the server on it and hands the
// API and the controller their new files, and every runner is given a credential under the new
// account the next time it asks.
func NewInstallation(dir string, until time.Time) (Installation, error) {
	if dir == "" {
		return Installation{}, errors.New("bus: no directory to write the installation's bus identity in")
	}
	if until.IsZero() {
		return Installation{}, errors.New("bus: a control plane credential that never expires, and one that never expires is one a stolen disk still holds")
	}
	if !until.After(time.Now()) {
		return Installation{}, fmt.Errorf("bus: a control plane credential that expired at %s, which the API and the controller refuse to start on", until.UTC().Format(time.RFC3339))
	}
	if err := private(dir, false); err != nil {
		return Installation{}, err
	}
	in := Installation{
		Accounts:     filepath.Join(dir, AccountsFile),
		AccountSeed:  filepath.Join(dir, AccountSeedFile),
		ControlPlane: filepath.Join(dir, ControlPlaneFile),
	}
	if err := unwritten(dir, in); err != nil {
		return Installation{}, err
	}
	operator, err := nkeys.CreateOperator()
	if err != nil {
		return Installation{}, fmt.Errorf("bus: the operator key could not be made: %w", err)
	}
	defer operator.Wipe()
	account, err := nkeys.CreateAccount()
	if err != nil {
		return Installation{}, fmt.Errorf("bus: the account key could not be made: %w", err)
	}
	defer account.Wipe()
	system, err := nkeys.CreateAccount()
	if err != nil {
		return Installation{}, fmt.Errorf("bus: the system account key could not be made: %w", err)
	}
	defer system.Wipe()
	if in.Operator, err = operator.PublicKey(); err != nil {
		return Installation{}, fmt.Errorf("bus: the operator key could not be read: %w", err)
	}
	if in.Account, err = account.PublicKey(); err != nil {
		return Installation{}, fmt.Errorf("bus: the account key could not be read: %w", err)
	}
	if in.System, err = system.PublicKey(); err != nil {
		return Installation{}, fmt.Errorf("bus: the system account key could not be read: %w", err)
	}

	// The application account, with JetStream, since the task and result streams are its.
	// Unlimited, because the bus is the installation's own: a limit here would be one more
	// place for a full pool to be refused a task, and an operator bounds the store where it
	// sizes the disk, in the server's own configuration.
	app := jwt.NewAccountClaims(in.Account)
	app.Name = "agentiik"
	app.Limits.JetStreamLimits.DiskStorage = jwt.NoLimit
	app.Limits.JetStreamLimits.MemoryStorage = jwt.NoLimit
	appJWT, err := app.Encode(operator)
	if err != nil {
		return Installation{}, fmt.Errorf("bus: the account could not be signed: %w", err)
	}

	// The system account, which JetStream refuses to start without and nobody connects to.
	sys := jwt.NewAccountClaims(in.System)
	sys.Name = "SYS"
	sysJWT, err := sys.Encode(operator)
	if err != nil {
		return Installation{}, fmt.Errorf("bus: the system account could not be signed: %w", err)
	}

	op := jwt.NewOperatorClaims(in.Operator)
	op.Name = "agentiik"
	op.SystemAccount = in.System
	opJWT, err := op.Encode(operator)
	if err != nil {
		return Installation{}, fmt.Errorf("bus: the operator could not be signed: %w", err)
	}

	accountSeed, err := account.Seed()
	if err != nil {
		return Installation{}, fmt.Errorf("bus: the account seed could not be read: %w", err)
	}
	defer wipe(accountSeed)

	// The control plane's credential, minted the way every other is. An Issuer asks for an
	// address because a runner is handed one with its credential; a credential file carries
	// none, since the API and the controller are given the bus's address as a setting.
	control, err := (&Issuer{account: account}).ForControlPlane(controlPlaneName, until)
	if err != nil {
		return Installation{}, err
	}
	creds, err := jwt.FormatUserConfig(control.JWT, []byte(control.Seed))
	if err != nil {
		return Installation{}, fmt.Errorf("bus: the control plane's credential could not be written out: %w", err)
	}
	defer wipe(creds)

	// The configuration a server includes. Every value is quoted, and the JWTs are written
	// inline: a path to a file would be read from the server's working directory rather than
	// from beside this one, which is a configuration that works until somebody starts the
	// server from somewhere else.
	var conf strings.Builder
	conf.WriteString("# The operator this installation's bus trusts, its application account and its system account,\n")
	conf.WriteString("# written once by agentiik and included by the server's own configuration, which enables JetStream.\n")
	fmt.Fprintf(&conf, "operator: %q\n", opJWT)
	fmt.Fprintf(&conf, "system_account: %q\n", in.System)
	conf.WriteString("resolver: MEMORY\n")
	fmt.Fprintf(&conf, "resolver_preload: {\n  %q: %q\n  %q: %q\n}\n", in.Account, appJWT, in.System, sysJWT)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Installation{}, fmt.Errorf("bus: the directory for the bus identity could not be made: %w", err)
	}
	err = writeAll(dir, []file{
		{in.Accounts, []byte(conf.String())},
		{in.AccountSeed, append(append([]byte{}, accountSeed...), '\n')},
		{in.ControlPlane, creds},
	})
	if err != nil {
		return Installation{}, err
	}
	return in, nil
}

// RenewControlPlane mints the control plane a new credential, expiring at until, under the account
// whose seed dir holds, and puts it in place of the one dir holds. It answers the credential's path
// and the instant it expires, which is until to the second.
//
// Nothing else changes. The operator, the accounts and the server's configuration stay as they
// were, so every stream, every consumer and every task queued on them stays too, and the API goes on
// minting runner credentials the server trusts. That is the difference between renewing a
// credential and creating an identity again, which would be a server restarted on new accounts with
// nothing it held before.
//
// The credential it replaces keeps working until its own expiry, since a NATS credential is not
// revoked but runs out, so a program still holding it is not cut off by the renewal. It is
// replaced in one step, by a rename over it once the new one is on the disk, so a program starting
// at that moment reads the one or the other and never half of each.
//
// dir has to hold the identity NewInstallation wrote: the account seed, and a server
// configuration that trusts the account it is the seed of. A seed from some other installation
// would mint a credential the server refuses, with nothing on the API's side to say why, so it is
// refused here instead. The directory is held to what private says, and the seed to what every
// secret's file is held to: a regular file its owner alone may read.
func RenewControlPlane(dir string, until time.Time) (string, time.Time, error) {
	if dir == "" {
		return "", time.Time{}, errors.New("bus: no directory holding the installation's bus identity")
	}
	if until.IsZero() {
		return "", time.Time{}, errors.New("bus: a control plane credential that never expires, and one that never expires is one a stolen disk still holds")
	}
	if !until.After(time.Now()) {
		return "", time.Time{}, fmt.Errorf("bus: a control plane credential that expired at %s, which the API and the controller refuse to start on", until.UTC().Format(time.RFC3339))
	}
	if err := private(dir, true); err != nil {
		return "", time.Time{}, err
	}

	seedPath := filepath.Join(dir, AccountSeedFile)
	info, err := os.Lstat(seedPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", time.Time{}, fmt.Errorf("bus: %s holds no %s, so there is no identity to renew a credential under: an installation's bus identity is created first", dir, AccountSeedFile)
	case err != nil:
		return "", time.Time{}, fmt.Errorf("bus: %s could not be looked for: %w", seedPath, err)
	case !info.Mode().IsRegular():
		return "", time.Time{}, fmt.Errorf("bus: %s is not a regular file, and the account seed is one", seedPath)
	case info.Mode().Perm()&0o077 != 0:
		return "", time.Time{}, fmt.Errorf("bus: %s is mode %#o, and the account seed is readable by its owner alone: chmod 600 it, because a seed anybody on the host can read is an account anybody on the host signs for", seedPath, info.Mode().Perm())
	}
	content, err := os.ReadFile(seedPath)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("bus: %s could not be read: %w", seedPath, err)
	}
	defer wipe(content)
	account, err := jwt.ParseDecoratedNKey(content)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("bus: %s holds no seed: %w", seedPath, err)
	}
	defer account.Wipe()
	public, err := account.PublicKey()
	if err != nil || !strings.HasPrefix(public, "A") {
		return "", time.Time{}, fmt.Errorf("bus: %s holds a seed that is not an account's, and a credential signed by anything else is one no bus trusts", seedPath)
	}
	conf, err := os.ReadFile(filepath.Join(dir, AccountsFile))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("bus: %s could not be read, and it is what says which account the server trusts: %w", filepath.Join(dir, AccountsFile), err)
	}
	if !strings.Contains(string(conf), strconv.Quote(public)) {
		return "", time.Time{}, fmt.Errorf("bus: %s holds the seed of %s, and %s does not trust that account, so a credential minted under it is one the server refuses", seedPath, public, AccountsFile)
	}

	control, err := (&Issuer{account: account}).ForControlPlane(controlPlaneName, until)
	if err != nil {
		return "", time.Time{}, err
	}
	creds, err := jwt.FormatUserConfig(control.JWT, []byte(control.Seed))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("bus: the control plane's credential could not be written out: %w", err)
	}
	path := filepath.Join(dir, ControlPlaneFile)
	if err := replace(dir, path, creds); err != nil {
		return "", time.Time{}, err
	}
	return path, control.ExpiresAt, nil
}

// private refuses a directory its group or anybody else may write to, and one that is not there
// where it has to be.
//
// Writing to a directory is replacing what it holds. Whoever may do that can put an operator of
// their own in the configuration the server includes, or a credential of their own where the
// control plane reads its credential, and the files' own modes, which say who may read them, say
// nothing about that. A directory that is not there yet is not refused where the caller makes it,
// which it does readable and writable by its owner alone.
func private(dir string, mustExist bool) error {
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist) && !mustExist:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("bus: %s does not exist, and it is the directory holding the installation's bus identity", dir)
	case err != nil:
		return fmt.Errorf("bus: %s could not be looked at: %w", dir, err)
	case !info.IsDir():
		return fmt.Errorf("bus: %s is not a directory", dir)
	case info.Mode().Perm()&0o022 != 0:
		return fmt.Errorf("bus: %s is mode %#o, writable by its group or by others, and whoever may write to it may replace the operator the server trusts or the credential the control plane signs in with: chmod 700 it", dir, info.Mode().Perm())
	}
	return nil
}

// replace puts content at path in one step: written beside it under another name, on the disk,
// then renamed over it, then the directory's entries on the disk as well.
func replace(dir, path string, content []byte) error {
	defer wipe(content)
	// CreateTemp makes the file readable and writable by its owner alone, and exclusively, so
	// that two renewals at once each write a file of their own and the later rename wins whole.
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("bus: a file beside %s could not be created: %w", path, err)
	}
	temporary := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(temporary)
		return fmt.Errorf("bus: %s could not be written: %w", path, err)
	}
	if _, err := f.Write(content); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(temporary)
		return fmt.Errorf("bus: %s could not be written: %w", path, err)
	}
	if err := os.Rename(temporary, path); err != nil {
		os.Remove(temporary)
		return fmt.Errorf("bus: %s could not be put in place: %w", path, err)
	}
	return syncDir(dir)
}

// file is one file writeAll writes.
type file struct {
	path    string
	content []byte
}

// writeAll writes each file once, then the directory's entries, and on failing part way removes
// the files it wrote and nothing else, so that what it leaves is either every file or none of them
// and the call can be run again.
func writeAll(dir string, files []file) error {
	var written []string
	undo := func() {
		for _, path := range written {
			os.Remove(path)
		}
	}
	for _, f := range files {
		if err := writeOnce(f.path, f.content); err != nil {
			undo()
			return err
		}
		written = append(written, f.path)
	}
	// The directory's entries as well as the files' contents, or a power cut after this
	// answered could come back to a directory missing a file the call said it wrote.
	if err := syncDir(dir); err != nil {
		undo()
		return err
	}
	return nil
}

// unwritten refuses a directory already holding any of the three files, and tells an identity from
// the remains of a creation cut off part way, which only the second can be repaired by removing.
// Every file is written exclusively after this all the same, so a call running at the same moment
// is refused there.
func unwritten(dir string, in Installation) error {
	var present, missing []string
	for _, path := range []string{in.Accounts, in.AccountSeed, in.ControlPlane} {
		_, err := os.Lstat(path)
		switch {
		case err == nil:
			present = append(present, filepath.Base(path))
		case errors.Is(err, fs.ErrNotExist):
			missing = append(missing, filepath.Base(path))
		default:
			return fmt.Errorf("bus: %s could not be looked for: %w", path, err)
		}
	}
	switch {
	case len(present) == 0:
		return nil
	case len(missing) == 0:
		return fmt.Errorf("bus: %s already holds an installation's bus identity, which is created once: an operator created twice is one the server trusts and another the API mints with", dir)
	default:
		return fmt.Errorf("bus: %s holds %s and not %s, which is what a creation cut off part way leaves and no program can start on: remove what it holds and create the identity again", dir, strings.Join(present, " and "), strings.Join(missing, " and "))
	}
}

// syncDir puts a directory's entries on the disk.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("bus: %s could not be opened to be written to the disk: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("bus: %s could not be written to the disk: %w", dir, err)
	}
	return nil
}

// writeOnce writes a file that did not exist, readable by its owner alone, and on the disk before
// it answers. Two of the three are secrets, and the third is written the same way so that the
// directory never holds a file anybody else on the host can read; the server's host is where it is
// handed on from.
func writeOnce(path string, content []byte) error {
	defer wipe(content)
	// Exclusively, which is what refuses a second call, and one running at the same moment.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("bus: %s already exists, and an installation's bus identity is created once: an operator created twice is one the server trusts and another the API mints with", path)
	}
	if err != nil {
		return fmt.Errorf("bus: %s could not be created: %w", path, err)
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("bus: %s could not be written: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("bus: %s could not be written: %w", path, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("bus: %s could not be written: %w", path, err)
	}
	return nil
}

// wipe overwrites a copy of a seed once it has been written, as nkeys does with its own.
func wipe(b []byte) {
	for i := range b {
		b[i] = 'x'
	}
}
