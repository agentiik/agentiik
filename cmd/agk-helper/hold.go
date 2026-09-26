package main

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// holdVerb is the one thing this program does that is not one of the three verbs: the runner's
// own, and never a script's. It is kept out of the command table, so that the usage a script
// reads and the three the documentation names stay the same list.
//
// A task's secret values reach its container on a tmpfs volume, and a tmpfs volume keeps what
// is written on it only while some container has it mounted: the daemon unmounts it when the
// last one lets go, and a value written through the archive API into a container that has not
// started is gone before the container starts. So the runner starts a container of its own on
// the volume first, this program in it, hands it the values on standard input, and removes it
// once the task's container has started and holds the volume in its place. The program needs
// no shell and nothing of the image it runs in, which is why it is this one: it is the static
// binary the runner already has for the platform its containers run on.
//
// In a script step's container it is harmless: /agk/secrets there is read-only, and the one
// thing it writes is under it.
const holdVerb = "hold-secrets"

// holdReady is the line written once every value is on the volume, which is what the runner
// waits for before it starts the task's container.
const holdReady = "ready"

// secretName is one value's file name, the last segment of /agk/secrets/<name> as the driver
// holds a mount to it: one segment, starting with a letter or a digit, so that neither . nor
// .. nor a path is a name.
var secretName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// The modes a value and the directory end on. A value is read by the task's own account, which
// is not the account this program writes as, so it is readable and never writable, and the
// directory is closed to writing once it is filled: the task's mount of it is read-only
// besides, and nothing in it is anybody's but the task's.
const (
	valueMode  = 0o444
	filledMode = 0o555
	fillMode   = 0o700
)

// hold writes the values a tar stream on standard input carries under /agk/secrets, says
// ready, and then waits for its standard input to end, which is the runner letting go of it or
// going away. Until then this process is what keeps the volume mounted.
func hold(e env, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("%q is one argument too many: %s takes none, and is the runner's own", args[0], holdVerb)
	}
	dir := filepath.Join(e.Root, "secrets")
	// A volume filled once and filled again, for a container the delivery that filled it
	// never started, holds the first values in a directory closed to writing: the runner
	// opens it again rather than being refused by what it made.
	if err := os.Chmod(dir, fillMode); err != nil {
		return fmt.Errorf("%s is where the values are written, and it could not be opened for writing: %w", dir, err)
	}
	r := tar.NewReader(e.In)
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("the values did not arrive as a tar stream: %w", err)
		}
		if h.Typeflag != tar.TypeReg || !secretName.MatchString(h.Name) {
			return fmt.Errorf("%q is not a value: every entry is a file named by one segment of /agk/secrets/<name>", h.Name)
		}
		if err := writeValue(filepath.Join(dir, h.Name), r); err != nil {
			return fmt.Errorf("the value %s could not be written: %w", h.Name, err)
		}
	}
	if err := os.Chmod(dir, filledMode); err != nil {
		return fmt.Errorf("%s could not be closed to writing: %w", dir, err)
	}
	fmt.Fprintln(e.Out, holdReady)
	// The end of standard input is the only thing waited on. The runner closes it when the
	// task's container has started, and a runner that dies closes it by going away, so a
	// volume is never held by a process nobody will remove.
	io.Copy(io.Discard, e.In)
	return nil
}

// writeValue writes one value, created private and made readable afterwards so that it never
// exists with a wider mode than it needs. What was there under the name is removed first: a
// value from an earlier filling, which the new one replaces.
func writeValue(path string, value io.Reader) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, value); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, valueMode)
}
