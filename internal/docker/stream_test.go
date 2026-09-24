package docker

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

// The demultiplexer is read from inside the package, because what it takes is a reader
// and there is no reason to open a socket to hand it one. Everything else about the
// stream, the hijack included, is read from outside against the fake daemon.

// frame writes one frame the way the daemon does.
func frame(s StdStream, text string) []byte {
	var b bytes.Buffer
	var header [8]byte
	header[0] = byte(s)
	binary.BigEndian.PutUint32(header[4:], uint32(len(text)))
	b.Write(header[:])
	b.WriteString(text)
	return b.Bytes()
}

// TestTheFrameHeaderKeepsTheTwoStreamsApart is the whole reason Tty stays false: a
// pseudo-terminal merges them, and the shorthand's standard output has to stay apart
// from the masked log.
func TestTheFrameHeaderKeepsTheTwoStreamsApart(t *testing.T) {
	var raw bytes.Buffer
	raw.Write(frame(Stdout, "the item\n"))
	raw.Write(frame(Stderr, "a complaint\n"))
	raw.Write(frame(Stdout, "another item\n"))

	s := newStream(&raw, nil, nil)
	var out, errs strings.Builder
	for {
		f, err := s.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading a frame: %v", err)
		}
		switch f.Stream {
		case Stdout:
			out.Write(f.Bytes)
		case Stderr:
			errs.Write(f.Bytes)
		default:
			t.Errorf("a frame arrived on %s", f.Stream)
		}
	}
	if out.String() != "the item\nanother item\n" {
		t.Errorf("standard output: got %q", out.String())
	}
	if errs.String() != "a complaint\n" {
		t.Errorf("standard error: got %q", errs.String())
	}
}

// TestEachFrameOwnsItsBytes holds the decision that a frame carries its own slice rather
// than a window onto a buffer the next call overwrites. What happens to these bytes is a
// masker, a log and a store, and a slice that changes underneath any of them is a secret
// in a log once a month.
func TestEachFrameOwnsItsBytes(t *testing.T) {
	var raw bytes.Buffer
	raw.Write(frame(Stdout, "first"))
	raw.Write(frame(Stdout, "second"))

	s := newStream(&raw, nil, nil)
	first, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next(); err != nil {
		t.Fatal(err)
	}
	if string(first.Bytes) != "first" {
		t.Errorf("the first frame reads %q after the second was read", first.Bytes)
	}
}

// TestAStreamThatEndsInsideAFrameIsNotACleanEnd holds the difference between a log that
// is short and a log that is wrong. A container whose output was truncated is not a
// container that wrote nothing.
func TestAStreamThatEndsInsideAFrameIsNotACleanEnd(t *testing.T) {
	for _, c := range []struct {
		name string
		raw  []byte
	}{
		{name: "inside the header", raw: []byte{1, 0, 0, 0, 0}},
		{name: "inside the payload", raw: append(frame(Stdout, "half")[:8], 'h', 'a')},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newStream(bytes.NewReader(c.raw), nil, nil)
			_, err := s.Next()
			if err == nil {
				t.Fatal("a frame that ended early was read as a whole one")
			}
			if errors.Is(err, io.EOF) {
				t.Errorf("%v reads as a clean end, and it is a truncation", err)
			}
		})
	}
}

// TestAFrameLongerThanAFrameCanBeIsRefused holds that a stream which lost its alignment
// is refused rather than allocated for.
func TestAFrameLongerThanAFrameCanBeIsRefused(t *testing.T) {
	var header [8]byte
	header[0] = byte(Stdout)
	binary.BigEndian.PutUint32(header[4:], maxFrame+1)

	s := newStream(bytes.NewReader(header[:]), nil, nil)
	if _, err := s.Next(); err == nil {
		t.Fatal("a frame past the maximum was accepted")
	} else if !strings.Contains(err.Error(), "alignment") {
		t.Errorf("the refusal is %q, and it does not say what went wrong", err)
	}
}

// TestAnEmptyFrameIsAFrameAndNotAnEnd holds that a zero-length write is read as one,
// because a container writing nothing on one stream is not a container that finished.
func TestAnEmptyFrameIsAFrameAndNotAnEnd(t *testing.T) {
	var raw bytes.Buffer
	raw.Write(frame(Stderr, ""))
	raw.Write(frame(Stderr, "after it"))

	s := newStream(&raw, nil, nil)
	first, err := s.Next()
	if err != nil {
		t.Fatalf("reading the empty frame: %v", err)
	}
	if first.Stream != Stderr || len(first.Bytes) != 0 {
		t.Errorf("the empty frame: %+v", first)
	}
	second, err := s.Next()
	if err != nil {
		t.Fatalf("reading the frame after it: %v", err)
	}
	if string(second.Bytes) != "after it" {
		t.Errorf("the frame after the empty one: %q", second.Bytes)
	}
}

// TestTheVersionsAreParsedAndCompared holds the negotiation's arithmetic, which is the
// half of it that does not need a daemon.
func TestTheVersionsAreParsedAndCompared(t *testing.T) {
	for _, c := range []struct {
		offered string
		speaks  string
		refused bool
	}{
		{offered: Ceiling, speaks: Ceiling},
		{offered: Floor, speaks: Floor},
		{offered: "1.55", speaks: "1.55"},
		{offered: "2.0", speaks: Ceiling},
		{offered: "1.24", refused: true},
		{offered: "0.99", refused: true},
		{offered: "", refused: true},
		{offered: "1.x", refused: true},
	} {
		t.Run(c.offered, func(t *testing.T) {
			got, err := negotiate(c.offered)
			if c.refused {
				if err == nil {
					t.Fatalf("%q was accepted and answered %q", c.offered, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q was refused: %v", c.offered, err)
			}
			if got != c.speaks {
				t.Errorf("a daemon offering %q is spoken to at %q, and this said %q", c.offered, c.speaks, got)
			}
		})
	}
}

// TestTheCeilingIsWhatTheDocumentationNames holds the one number the site fixes.
func TestTheCeilingIsWhatTheDocumentationNames(t *testing.T) {
	if Ceiling != "1.56" {
		t.Errorf("Ceiling is %q, and the documentation names v1.56", Ceiling)
	}
	floor, err := parseAPIVersion(Floor)
	if err != nil {
		t.Fatalf("Floor does not parse: %v", err)
	}
	// The newest endpoint this package speaks is the wait with
	// condition=next-exit, which arrived at v1.30. A floor below it would be a floor
	// this package could not keep.
	if floor.before(apiVersion{major: 1, minor: 30}) {
		t.Errorf("Floor is %s, and the wait with condition=next-exit is v1.30", Floor)
	}
}

// TestTheUsernsSignalIsReadOffTheSecurityOptions holds where the floor is read, which is
// a fact about the wire rather than a rule of the product.
func TestTheUsernsSignalIsReadOffTheSecurityOptions(t *testing.T) {
	on := Info{SecurityOptions: []string{"name=seccomp,profile=builtin", "name=userns"}}
	if !on.UsernsRemapped() {
		t.Error("a daemon carrying name=userns was read as one without it")
	}
	off := Info{SecurityOptions: []string{"name=seccomp,profile=builtin", "name=cgroupns"}}
	if off.UsernsRemapped() {
		t.Error("a daemon carrying no name=userns was read as one with it")
	}
	if (Info{}).UsernsRemapped() {
		t.Error("a daemon carrying no security options at all was read as remapped")
	}
}

// TestTheRemappedRangeIsReadOffTheRootDirectory holds the other half of the floor: the
// ownership a working directory has to be given, which is the one place the range is
// readable without parsing /etc/subuid.
func TestTheRemappedRangeIsReadOffTheRootDirectory(t *testing.T) {
	uid, gid, ok := Info{DockerRootDir: "/var/lib/docker/165536.165536"}.RemappedRange()
	if !ok || uid != "165536" || gid != "165536" {
		t.Errorf("got %q %q %v, want the pair the directory name ends with", uid, gid, ok)
	}
	if _, _, ok := (Info{DockerRootDir: "/var/lib/docker"}).RemappedRange(); ok {
		t.Error("a root directory with no range was read as carrying one")
	}
	if _, _, ok := (Info{DockerRootDir: "/var/lib/docker/dockremap.dockremap"}).RemappedRange(); ok {
		t.Error("a root directory ending in names was read as carrying a numbered range")
	}
}

// The three profiles of the SecurityOpt row are read off the same list, one option each,
// and a seccomp listed as unconfined is told apart from one listed with a profile, since
// it is the one answer that lists the mechanism and applies none of it.
func TestTheProfilesAreReadOffTheSecurityOptions(t *testing.T) {
	ubuntu := Info{SecurityOptions: []string{"name=apparmor", "name=seccomp,profile=builtin", "name=cgroupns"}}
	if profile, ok := ubuntu.SeccompProfile(); !ok || profile != "builtin" {
		t.Errorf("the seccomp profile of %v reads as %q, %v", ubuntu.SecurityOptions, profile, ok)
	}
	if !ubuntu.AppArmor() || ubuntu.SELinux() {
		t.Errorf("%v reads as AppArmor %v and SELinux %v", ubuntu.SecurityOptions, ubuntu.AppArmor(), ubuntu.SELinux())
	}

	fedora := Info{SecurityOptions: []string{"name=seccomp,profile=/etc/docker/seccomp.json", "name=selinux"}}
	if profile, _ := fedora.SeccompProfile(); profile != "/etc/docker/seccomp.json" {
		t.Errorf("a daemon started with a profile of its own reads as %q", profile)
	}
	if fedora.AppArmor() || !fedora.SELinux() {
		t.Errorf("%v reads as AppArmor %v and SELinux %v", fedora.SecurityOptions, fedora.AppArmor(), fedora.SELinux())
	}

	unconfined := Info{SecurityOptions: []string{"name=seccomp,profile=unconfined"}}
	if profile, ok := unconfined.SeccompProfile(); !ok || profile != "unconfined" {
		t.Errorf("a daemon started with --seccomp-profile=unconfined reads as %q, %v", profile, ok)
	}

	if _, ok := (Info{SecurityOptions: []string{"name=cgroupns"}}).SeccompProfile(); ok {
		t.Error("a daemon listing no seccomp was read as one that filters system calls")
	}
	// A field is matched whole, so a profile that happens to be called apparmor is not
	// the AppArmor option.
	if (Info{SecurityOptions: []string{"name=seccomp,profile=apparmor"}}).AppArmor() {
		t.Error("a seccomp profile called apparmor was read as AppArmor")
	}
}
