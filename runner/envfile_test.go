package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What join writes, serve reads: rendered, the file is the settings and the identity it was given,
// in the grammar the reader holds a line to.
func TestRunnerEnvIsRenderedAsServeReadsIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.env")
	text, err := renderEnv(path, []variable{
		{API, "https://agentiik.example.com"},
		{RunnerID, "runner-dmz-02"},
		{RunnerPool, "dmz"},
		{Labels, "zone=dmz,arch=amd64"},
		{Namespaces, ""},
		{Credential, credential},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, text, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := ReadConfig(environment(nil), path)
	if err != nil {
		t.Fatal(err)
	}
	if c.API != "https://agentiik.example.com" || c.Runner != "runner-dmz-02" || c.Pool != "dmz" || string(c.Credential) != credential {
		t.Errorf("serve reads %+v", c)
	}
	// A setting join was not given is left out rather than written empty.
	if strings.Contains(string(text), Namespaces) {
		t.Errorf("runner.env names %s, which join was not given:\n%s", Namespaces, text)
	}
}

func TestAValueEveryReaderWouldNotReadAlikeIsNeverRendered(t *testing.T) {
	for name, v := range map[string]variable{
		"a line break in the credential": {Credential, credential + "\nAGK_API=https://elsewhere.example.com"},
		"a $ in the address":             {API, "https://agentiik.example.com/$HOME"},
		"a runner outside the grammar":   {RunnerID, "Runner 2"},
		"a pool outside the grammar":     {RunnerPool, "DMZ"},
		"a credential of another kind":   {Credential, "agkjoin_" + strings.Repeat("x", 43)},
	} {
		t.Run(name, func(t *testing.T) {
			vars := []variable{
				{API, "https://agentiik.example.com"},
				{RunnerID, "runner-dmz-02"},
				{RunnerPool, "dmz"},
				{Labels, "zone=dmz"},
				{Credential, credential},
			}
			for i := range vars {
				if vars[i].name == v.name {
					vars[i] = v
				}
			}
			text, err := renderEnv(filepath.Join(t.TempDir(), "runner.env"), vars)
			if err == nil {
				t.Fatalf("rendered:\n%s", text)
			}
			if strings.Contains(err.Error(), credential) || strings.Contains(err.Error(), "agkjoin_") {
				t.Errorf("the refusal repeats a credential: %s", err)
			}
		})
	}
}

// A staged file is 0600 and its owner's before anything is in it, and only moved into place
// whole.
func TestAStagedFileIsTheOwnersAloneAndMovedIntoPlaceWhole(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "runner.env")
	s, err := stage(final, &Owner{UID: os.Getuid(), GID: os.Getgid()})
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the staged file is %s before anything is written to it", info.Mode())
	}
	if _, err := os.Lstat(final); err == nil {
		t.Error("the file is in place before it was committed")
	}
	if err := s.write([]byte("AGK_API=https://agentiik.example.com\n")); err != nil {
		t.Fatal(err)
	}
	if err := s.commit(false); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(final); err != nil || string(b) != "AGK_API=https://agentiik.example.com\n" {
		t.Errorf("the committed file holds %q (%v)", b, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("committing left %d files in the directory", len(entries))
	}
}

// Without replace, a file that arrived after join looked is kept, and the staged one is not put
// in its place: two joins at once end with one identity.
func TestACommitWithoutReplaceKeepsAFileThatArrivedMeanwhile(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "runner.key")
	s, err := stage(final, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(final, []byte("the other join's"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.write([]byte("this join's")); err != nil {
		t.Fatal(err)
	}
	if err := s.commit(false); err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("the commit answered %v", err)
	}
	if b, _ := os.ReadFile(final); string(b) != "the other join's" {
		t.Errorf("the file that arrived meanwhile now holds %q", b)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the refused commit left %d files in the directory", len(entries))
	}

	s, err = stage(final, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.write([]byte("this join's")); err != nil {
		t.Fatal(err)
	}
	if err := s.commit(true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(final); string(b) != "this join's" {
		t.Errorf("a replacing commit left %q", b)
	}
}

// A directory join creates is created with its mode and given to its owner, and one already there
// is left as it was: it is whoever created it's to have set up.
func TestADirectoryIsCreatedWithItsModeAndOneThereIsLeftAlone(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "var", "lib", "agentiik")
	if err := makeDir(dir, 0o700, &Owner{UID: os.Getuid(), GID: os.Getgid()}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(root, "var"), dir} {
		if info, err := os.Stat(d); err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("%s was created as %v (%v)", d, info.Mode(), err)
		}
	}

	there := filepath.Join(root, "etc")
	if err := os.Mkdir(there, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(there, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := makeDir(there, 0o700, nil); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(there); info.Mode().Perm() != 0o755 {
		t.Errorf("a directory already there was changed to %s", info.Mode())
	}

	file := filepath.Join(root, "file")
	os.WriteFile(file, nil, 0o600)
	if err := makeDir(file, 0o700, nil); err == nil {
		t.Error("a file where a directory goes was taken as one")
	}
}
