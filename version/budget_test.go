package version_test

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/version"
)

// endless is a tree of one file, agentiik.yaml, that never ends and says it weighs size bytes, and
// counts what was read of it.
type endless struct {
	size int64
	read *int64
}

func (e endless) Open(name string) (fs.File, error) {
	if name != version.EntryPoint {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return &endlessFile{endless: e}, nil
}

type endlessFile struct{ endless }

func (f *endlessFile) Stat() (fs.FileInfo, error) { return endlessInfo{f.size}, nil }
func (f *endlessFile) Close() error               { return nil }
func (f *endlessFile) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = '#'
	}
	*f.read += int64(len(p))
	return len(p), nil
}

type endlessInfo struct{ size int64 }

func (i endlessInfo) Name() string       { return version.EntryPoint }
func (i endlessInfo) Size() int64        { return i.size }
func (i endlessInfo) Mode() fs.FileMode  { return 0o644 }
func (i endlessInfo) ModTime() time.Time { return time.Time{} }
func (i endlessInfo) IsDir() bool        { return false }
func (i endlessInfo) Sys() any           { return nil }

// An entry point of hundreds of mebibytes is a few kilobytes of a git push once compressed, and the
// validation reads no more of a tree than ReadMaxBytes, every file together: one that says it is
// larger is refused before a byte is read, and one that says it is small and goes on is refused one
// byte past the budget.
func TestTheValidationReadsATreeWithinItsBudget(t *testing.T) {
	for name, size := range map[string]int64{"saying what it weighs": 600 << 20, "lying about it": 10} {
		var read int64
		_, err := version.Check(t.Context(), endless{size: size, read: &read}, version.Checking{Entry: version.EntryPoint})
		if err == nil || !strings.Contains(err.Error(), "past 16777216 bytes") {
			t.Errorf("an entry point that never ends, %s, is answered %v", name, err)
		}
		if read > version.ReadMaxBytes+1 {
			t.Errorf("an entry point that never ends, %s, was read for %d bytes, past the %d the validation reads", name, read, version.ReadMaxBytes)
		}
	}
}

// shared is a tree whose every directory names the same directory of a thousand entries, as git
// lets one tree object be named any number of times: eleven trees, and 10^33 paths.
type shared struct{}

func (shared) Open(name string) (fs.File, error) { return &sharedDir{name: name}, nil }

type sharedDir struct {
	name string
	read bool
}

func (d *sharedDir) Stat() (fs.FileInfo, error) { return sharedInfo(path.Base(d.name)), nil }
func (d *sharedDir) Read([]byte) (int, error)   { return 0, errors.New("a directory") }
func (d *sharedDir) Close() error               { return nil }
func (d *sharedDir) ReadDir(int) ([]fs.DirEntry, error) {
	if d.read || d.name != "." && strings.Count(d.name, "/") >= 10 {
		return nil, nil
	}
	d.read = true
	entries := make([]fs.DirEntry, 1000)
	for i := range entries {
		entries[i] = fs.FileInfoToDirEntry(sharedInfo(fmt.Sprintf("e%03d", i)))
	}
	return entries, nil
}

type sharedInfo string

func (i sharedInfo) Name() string       { return string(i) }
func (i sharedInfo) Size() int64        { return 0 }
func (i sharedInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o755 }
func (i sharedInfo) ModTime() time.Time { return time.Time{} }
func (i sharedInfo) IsDir() bool        { return true }
func (i sharedInfo) Sys() any           { return nil }

// A walk of a commit's tree counts the paths it visits, and a tree naming one directory many times
// over is refused at TreeMaxEntries rather than walked for ever.
func TestATreeNamingOneDirectoryOverAndOverIsRefusedByItsCount(t *testing.T) {
	_, err := version.Check(t.Context(), shared{}, version.Checking{Committed: true})
	if !errors.Is(err, version.ErrTooManyEntries) {
		t.Errorf("a tree of 10^33 paths is answered %v", err)
	}
}
