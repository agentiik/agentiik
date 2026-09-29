package graph

import (
	"slices"
	"strings"
	"testing"
)

var aTree = []string{
	"agentiik.yaml",
	"certs/internal-ca.pem",
	"fixtures/big.bin",
	"scripts/normalize.py",
	"sql/2026/q1.sql",
	"sql/README.md",
	"sql/orders.sql",
}

func placedAs(p []Placed) []string {
	out := make([]string, 0, len(p))
	for _, f := range p {
		s := f.Path
		if f.To != "" {
			s += " -> " + f.To
		}
		if f.Mode != "" {
			s += " " + f.Mode
		}
		out = append(out, s)
	}
	return out
}

// "A path names a file, or a directory with everything under it. In a glob * matches within one
// path segment, ? one character, [abc] one of a set or a range, and ** written as a segment of its
// own zero or more whole segments, so ./sql/**/*.sql is every .sql file at any depth under sql/."
func TestAStepsFilesSelectWhatTheyName(t *testing.T) {
	for _, c := range []struct {
		files []string
		want  []string
	}{
		{nil, aTree},
		{[]string{"./sql/**"}, []string{"sql/2026/q1.sql", "sql/README.md", "sql/orders.sql"}},
		{[]string{"./sql/**/*.sql"}, []string{"sql/2026/q1.sql", "sql/orders.sql"}},
		{[]string{"sql/*.sql"}, []string{"sql/orders.sql"}},
		{[]string{"sql"}, []string{"sql/2026/q1.sql", "sql/README.md", "sql/orders.sql"}},
		{[]string{"/scripts/normalize.py", "agentiik.yaml"}, []string{"agentiik.yaml", "scripts/normalize.py"}},
		{[]string{"sql/2026/q?.sql"}, []string{"sql/2026/q1.sql"}},
		{[]string{"sql/[a-o]*.sql"}, []string{"sql/orders.sql"}},
		{[]string{"**/*.pem"}, []string{"certs/internal-ca.pem"}},
		{[]string{"sq"}, nil},
		{[]string{"nothing/**"}, nil},
	} {
		selectors := make([]FileSelector, 0, len(c.files))
		for _, f := range c.files {
			selectors = append(selectors, FileSelector{From: f})
		}
		got, err := Place(selectors, aTree)
		if err != nil {
			t.Errorf("%v: %v", c.files, err)
			continue
		}
		if paths := placedAs(got); !slices.Equal(paths, c.want) && !(len(paths) == 0 && len(c.want) == 0) {
			t.Errorf("%v select %v, not %v", c.files, paths, c.want)
		}
	}
}

// "to is an absolute path; a directory or a glob relocated there keeps each file's path below the
// directory, or below the glob's segments before its first wildcard, and mode applies to every
// file it places. A selector that relocates nothing leaves the file where it is under /agk/repo/."
func TestARelocationKeepsThePathBelowWhatItNames(t *testing.T) {
	got, err := Place([]FileSelector{
		{From: "./sql/**/*.sql", To: "/docker-entrypoint-initdb.d", Mode: "0444"},
		{From: "./certs/internal-ca.pem", To: "/etc/ssl/certs/internal-ca.pem"},
		{From: "scripts", To: "/opt/tools/"},
		{From: "agentiik.yaml", Mode: "0600"},
	}, aTree)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"agentiik.yaml 0600",
		"certs/internal-ca.pem -> /etc/ssl/certs/internal-ca.pem",
		"scripts/normalize.py -> /opt/tools/normalize.py",
		"sql/2026/q1.sql -> /docker-entrypoint-initdb.d/2026/q1.sql 0444",
		"sql/orders.sql -> /docker-entrypoint-initdb.d/orders.sql 0444",
	}
	if paths := placedAs(got); !slices.Equal(paths, want) {
		t.Errorf("the relocations place\n%s\nnot\n%s", strings.Join(paths, "\n"), strings.Join(want, "\n"))
	}
}

// A file both left where it is and relocated is in both places, and one selected twice for one
// place is there once.
func TestAFileSelectedTwiceIsPlacedWhereEachSays(t *testing.T) {
	got, err := Place([]FileSelector{
		{From: "sql/**"}, {From: "sql/orders.sql"}, {From: "sql/orders.sql", To: "/init/orders.sql"}, {From: "sql/*.sql", To: "/init", Mode: "0555"},
	}, aTree)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sql/2026/q1.sql", "sql/README.md", "sql/orders.sql", "sql/orders.sql -> /init/orders.sql 0555"}
	if paths := placedAs(got); !slices.Equal(paths, want) {
		t.Errorf("selected twice, the files are placed %v", paths)
	}
}

// Two files at one place would be one of them silently missing, and a selector leaving the tree or
// written as no glob reads is refused rather than read as matching nothing.
func TestWhatNoTreeCanHoldIsRefused(t *testing.T) {
	for name, selectors := range map[string][]FileSelector{
		"two files at one place":  {{From: "sql/orders.sql", To: "/init/x.sql"}, {From: "sql/2026/q1.sql", To: "/init/x.sql"}},
		"a path leaving the tree": {{From: "../secrets"}},
		"a glob leaving the tree": {{From: "./sql/../../**"}},
		"a set never closed":      {{From: "sql/[a-"}},
	} {
		if _, err := Place(selectors, aTree); err == nil {
			t.Errorf("%s is placed", name)
		}
	}
	if err := relocatedTo(Defaults{Files: []FileSelector{{From: "../../etc/shadow"}}}, "steps.load"); err == nil || !strings.Contains(err.Error(), "steps.load.files[0]") {
		t.Errorf("a selector leaving the tree is not refused where a version is made: %v", err)
	}
}
