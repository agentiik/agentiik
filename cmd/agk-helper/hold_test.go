package main

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/driver"
)

// values is a tar stream of name and value pairs, as the runner hands them to the holder.
func values(t *testing.T, pairs ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for i := 0; i < len(pairs); i += 2 {
		if err := w.WriteHeader(&tar.Header{Name: pairs[i], Mode: 0o400, Size: int64(len(pairs[i+1])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(pairs[i+1]))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// said is standard output as the holder writes it while the test reads it.
type said struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *said) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *said) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// holding runs the holder on a volume laid out as /agk/secrets, with standard input what the
// test gives it, and answers with what it says and a channel that carries its exit.
func holding(t *testing.T, h *harness, stdin io.Reader) (*said, <-chan int) {
	t.Helper()
	dir := filepath.Join(h.env.Root, "secrets")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A filled directory is closed to writing, and the test's own is removed afterwards.
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	out := &said{}
	h.env.In, h.env.Out, h.env.Err = stdin, out, out
	done := make(chan int, 1)
	go func() { done <- run(h.env, []string{holdVerb}) }()
	return out, done
}

// The holder writes each value under its name, readable and never writable, closes the
// directory to writing, says ready, and holds on until its standard input ends: it is what
// keeps the volume mounted, so it may not end before the runner lets it go.
func TestTheHolderWritesTheValuesAndHoldsOnUntilItIsLetGo(t *testing.T) {
	h := newHarness(t)
	r, w := io.Pipe()
	out, done := holding(t, h, r)
	go w.Write(values(t, "bearer", "s3cr3t-value", "client.key", "-----BEGIN KEY-----"))

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "ready\n") {
		if time.Now().After(deadline) {
			t.Fatalf("the holder never said ready: %q", out)
		}
		time.Sleep(10 * time.Millisecond)
	}

	dir := filepath.Join(h.env.Root, "secrets")
	for name, want := range map[string]string{"bearer": "s3cr3t-value", "client.key": "-----BEGIN KEY-----"} {
		path := filepath.Join(dir, name)
		b, err := os.ReadFile(path)
		if err != nil || string(b) != want {
			t.Errorf("%s holds %q: %v", name, b, err)
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o444 {
			t.Errorf("%s is %v, and a value is 0444", name, info.Mode().Perm())
		}
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o555 {
		t.Errorf("the directory is %v once filled, and it is 0555", info.Mode().Perm())
	}

	select {
	case code := <-done:
		t.Fatalf("the holder ended with %d before its standard input did", code)
	case <-time.After(100 * time.Millisecond):
	}
	w.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("the holder let go of exits %d: %s", code, out)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the holder did not end when its standard input did")
	}
}

// A volume filled once, for a container its delivery never started, is filled again by the
// next: the directory the first holder closed is opened, and what it wrote is replaced.
func TestTheHolderFillsAVolumeAgain(t *testing.T) {
	h := newHarness(t)
	for _, value := range []string{"first", "rotated"} {
		out, done := holding(t, h, bytes.NewReader(values(t, "bearer", value)))
		if code := <-done; code != 0 {
			t.Fatalf("the holder of %s exited %d: %s", value, code, out)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(h.env.Root, "secrets", "bearer")); string(b) != "rotated" {
		t.Errorf("the value is %q after the second filling", b)
	}
}

// Every entry is a file named by one segment of /agk/secrets/<name>. A path, a parent, a
// link or a directory is refused, and nothing is said ready.
func TestTheHolderRefusesWhatIsNotAValue(t *testing.T) {
	for _, header := range []tar.Header{
		{Name: "../escape", Typeflag: tar.TypeReg},
		{Name: "a/b", Typeflag: tar.TypeReg},
		{Name: ".netrc", Typeflag: tar.TypeReg},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/shadow"},
		{Name: "dir", Typeflag: tar.TypeDir},
	} {
		var b bytes.Buffer
		w := tar.NewWriter(&b)
		w.WriteHeader(&header)
		w.Close()

		h := newHarness(t)
		out, done := holding(t, h, &b)
		if code := <-done; code == 0 {
			t.Errorf("%+v was taken for a value", header)
		}
		if strings.Contains(out.String(), "ready") {
			t.Errorf("the holder said ready after refusing %s", header.Name)
		}
		if _, err := os.Lstat(filepath.Join(h.env.Root, "escape")); err == nil {
			t.Errorf("%s was written outside /agk/secrets", header.Name)
		}
	}
}

// The holder is not one of the three verbs a script is offered, and the runner runs it by the
// name the driver starts it with.
func TestTheHolderIsTheDriversAndNotAScriptsVerb(t *testing.T) {
	if !slices.Equal(driver.HolderCommand, []string{BinPath, holdVerb}) {
		t.Errorf("the driver runs %v, and the helper answers to %s %s", driver.HolderCommand, BinPath, holdVerb)
	}
	var b bytes.Buffer
	usage(&b)
	if strings.Contains(b.String(), holdVerb) {
		t.Errorf("the usage a script reads offers %s", holdVerb)
	}
}
