package runner

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Owner is the account join gives its files to: the agent's, which serve holds runner.env's owner
// to. Nil is whoever runs join, which is that account already.
type Owner struct {
	UID, GID int
}

// variable is one line of runner.env.
type variable struct{ name, value string }

// renderEnv is runner.env as join writes it, read back through the reader serve uses so that a
// file join writes is one serve takes.
//
// It is read back rather than trusted because two of its values come from the API's answer, and
// an answer this runner could not read back would be a runner that joined, spent its token, and
// could never start: that is refused here, before anything is written, with the reason serve
// would have given.
func renderEnv(path string, vars []variable) ([]byte, error) {
	var b strings.Builder
	b.WriteString("# Written by agk-runner join, and read by agk-runner serve: this runner's identity and the\n")
	b.WriteString("# credential every call to the API carries. Owned by the agent's account, mode 0600.\n")
	for _, v := range vars {
		if v.value == "" {
			continue
		}
		if fault := valueFault(v.value); fault != "" {
			// The value is not repeated: it may be the credential.
			return nil, &Error{Variable: v.name, Reason: fault + ", and it cannot be written to " + path}
		}
		b.WriteString(v.name + "=" + v.value + "\n")
	}
	text := []byte(b.String())

	r := &reader{lookup: func(string) (string, bool) { return "", false }, path: path, file: map[string]string{}, written: map[string]bool{}}
	r.parse(text)
	r.api()
	r.labels()
	r.namespaces()
	r.concurrency()
	r.workDir()
	r.name(RunnerID, "the identifier the API minted for this runner")
	r.name(RunnerPool, "the pool this runner joined")
	r.credential()
	if err := r.err(); err != nil {
		return nil, err
	}
	return text, nil
}

// staged is a file written beside where it goes and moved into place whole, so that a reader
// finds the old file or the new one and never part of either.
//
// It is created with mode 0600 and given to its owner before a byte is written to it, so that
// there is no moment at which what it holds is anybody else's to read.
type staged struct {
	f     *os.File
	final string
}

// stage creates the temporary file for final, in final's directory, since a rename moves a file
// within one filesystem alone.
func stage(final string, owner *Owner) (*staged, error) {
	f, err := os.CreateTemp(filepath.Dir(final), "."+filepath.Base(final)+".new-*")
	if err != nil {
		return nil, fmt.Errorf("runner: %s cannot be written: %s", final, reasonOf(err))
	}
	s := &staged{f: f, final: final}
	// CreateTemp gives 0600 already. It is said again so that the mode does not rest on a
	// default somebody could change.
	if err := f.Chmod(0o600); err != nil {
		s.abandon()
		return nil, fmt.Errorf("runner: %s cannot be written: %s", final, reasonOf(err))
	}
	if owner != nil {
		if err := f.Chown(owner.UID, owner.GID); err != nil {
			s.abandon()
			return nil, fmt.Errorf("runner: %s cannot be given to account %d: %s", final, owner.UID, reasonOf(err))
		}
	}
	return s, nil
}

// write writes the whole of the file and makes it durable, so that what is renamed into place is
// on the disk and not only in the page cache.
func (s *staged) write(b []byte) error {
	if _, err := s.f.Write(b); err != nil {
		return fmt.Errorf("runner: %s cannot be written: %s", s.final, reasonOf(err))
	}
	if err := s.f.Sync(); err != nil {
		return fmt.Errorf("runner: %s cannot be written: %s", s.final, reasonOf(err))
	}
	return nil
}

// commit moves the file into place and syncs its directory, so that the rename survives a crash.
//
// Without replace, the file is linked into place rather than renamed, since a link refuses a name
// that exists where a rename would take it: two joins run at once on one host, or a file that
// arrived after join looked, end with one identity and a refusal rather than with whichever wrote
// last.
func (s *staged) commit(replace bool) error {
	if err := s.f.Close(); err != nil {
		os.Remove(s.f.Name())
		return fmt.Errorf("runner: %s cannot be written: %s", s.final, reasonOf(err))
	}
	if replace {
		if err := os.Rename(s.f.Name(), s.final); err != nil {
			os.Remove(s.f.Name())
			return fmt.Errorf("runner: %s cannot be written: %s", s.final, reasonOf(err))
		}
	} else {
		err := os.Link(s.f.Name(), s.final)
		os.Remove(s.f.Name())
		switch {
		case errors.Is(err, fs.ErrExist):
			return fmt.Errorf("runner: %s appeared while this host was joining, and it is not replaced without --replace", s.final)
		case err != nil:
			return fmt.Errorf("runner: %s cannot be written: %s", s.final, reasonOf(err))
		}
	}
	return syncDir(filepath.Dir(s.final))
}

// abandon removes the file, which is how a join that stops before it commits leaves nothing.
func (s *staged) abandon() {
	s.f.Close()
	os.Remove(s.f.Name())
}

// syncDir makes a rename in dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("runner: %s cannot be synced: %s", dir, reasonOf(err))
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("runner: %s cannot be synced, so what was written there may not survive a crash: %s", dir, reasonOf(err))
	}
	return nil
}

// makeDir creates dir and whatever of its parents is missing, each with mode and given to owner,
// and leaves alone whatever is there: a directory an operator created is theirs to have set up.
func makeDir(dir string, mode fs.FileMode, owner *Owner) error {
	info, err := os.Stat(dir)
	switch {
	case err == nil && info.IsDir():
		return nil
	case err == nil:
		return fmt.Errorf("runner: %s is not a directory", dir)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("runner: %s cannot be read: %s", dir, reasonOf(err))
	}
	if parent := filepath.Dir(dir); parent != dir {
		if err := makeDir(parent, mode, owner); err != nil {
			return err
		}
	}
	err = os.Mkdir(dir, mode)
	switch {
	case errors.Is(err, fs.ErrExist):
		// Somebody else created it in the meantime, and it is theirs as a directory found
		// there is.
		return makeDir(dir, mode, owner)
	case err != nil:
		return fmt.Errorf("runner: %s cannot be created: %s", dir, reasonOf(err))
	}
	// The umask may have taken bits off, and the mode is the point.
	if err := os.Chmod(dir, mode); err != nil {
		return fmt.Errorf("runner: %s cannot be created: %s", dir, reasonOf(err))
	}
	if owner != nil {
		if err := os.Chown(dir, owner.UID, owner.GID); err != nil {
			return fmt.Errorf("runner: %s cannot be given to account %d: %s", dir, owner.UID, reasonOf(err))
		}
	}
	return nil
}
