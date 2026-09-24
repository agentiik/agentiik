package bus

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
// created again over it.
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
	control, err := (&Issuer{account: account}).ForControlPlane("agentiik-control-plane", until)
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
