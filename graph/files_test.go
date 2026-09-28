package graph

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// theRepository is the tree of the documentation's example: an entry point, schemas, scripts, SQL
// at two depths, a certificate, and fixtures a step loading SQL has no use for.
var theRepository = []string{
	"agentiik.yaml",
	"certs/internal-ca.pem",
	"fixtures/big.bin",
	"schemas/order.json",
	"scripts/lib/rounding.py",
	"scripts/normalize.py",
	"sql/2026/q1.sql",
	"sql/README.md",
	"sql/orders.sql",
}

// "In a glob * matches within one path segment, ? one character, [abc] one of a set or a range,
// and ** written as a segment of its own zero or more whole segments, so ./sql/**/*.sql is every
// .sql file at any depth under sql/."
func TestAGlobIsMatchedSegmentBySegment(t *testing.T) {
	for _, c := range []struct {
		glob string
		want []string
	}{
		{"./sql/**/*.sql", []string{"sql/2026/q1.sql", "sql/orders.sql"}},
		{"./sql/**", []string{"sql/2026/q1.sql", "sql/README.md", "sql/orders.sql"}},
		{"sql/*", []string{"sql/README.md", "sql/orders.sql"}},
		{"sql/*.sql", []string{"sql/orders.sql"}},
		{"**/*.py", []string{"scripts/lib/rounding.py", "scripts/normalize.py"}},
		{"**/order?.*", []string{"sql/orders.sql"}},
		{"**/order.*", []string{"schemas/order.json"}},
		{"s[cq]*/*", []string{"schemas/order.json", "scripts/normalize.py", "sql/README.md", "sql/orders.sql"}},
		{"sql/[0-9][0-9][0-9][0-9]/*", []string{"sql/2026/q1.sql"}},
		{"/certs/*.pem", []string{"certs/internal-ca.pem"}},
		// ** inside a segment is two stars, each within that segment.
		{"sql/**.sql", []string{"sql/orders.sql"}},
		{"**", theRepository},
		{"*.yaml", []string{"agentiik.yaml"}},
		{"nothing/**", nil},
	} {
		got := SelectFiles([]FileSelector{{From: c.glob}}, theRepository)
		if !slices.Equal(got.Tree, c.want) || len(got.Placed) != 0 {
			t.Errorf("%s selects %q and places %v, want %q", c.glob, got.Tree, got.Placed, c.want)
		}
	}
}

// "A path names a file, or a directory with everything under it."
func TestAPathNamesAFileOrADirectoryWithEverythingUnderIt(t *testing.T) {
	for _, c := range []struct {
		from string
		want []string
	}{
		{"./sql", []string{"sql/2026/q1.sql", "sql/README.md", "sql/orders.sql"}},
		{"sql/", []string{"sql/2026/q1.sql", "sql/README.md", "sql/orders.sql"}},
		{"scripts/lib", []string{"scripts/lib/rounding.py"}},
		{"certs/internal-ca.pem", []string{"certs/internal-ca.pem"}},
		// A prefix of a name is not the directory it looks like.
		{"sq", nil},
		{"scripts/norm", nil},
		{".", theRepository},
		{"./", theRepository},
	} {
		got := SelectFiles([]FileSelector{{From: c.from}}, theRepository)
		if !slices.Equal(got.Tree, c.want) {
			t.Errorf("%s selects %q, want %q", c.from, got.Tree, c.want)
		}
	}
}

// "Narrows the repository tree, which it otherwise gets whole under /agk/repo/." A step that says
// nothing about files is handed every file of the tree, since narrowing is "an optimisation for
// large repositories, never a requirement, and never a permission boundary".
func TestAStepWithoutFilesIsGivenTheWholeTree(t *testing.T) {
	got := SelectFiles(nil, theRepository)
	if !slices.Equal(got.Tree, theRepository) || len(got.Placed) != 0 {
		t.Fatalf("a step with no files is given %q and %v", got.Tree, got.Placed)
	}
	// Several selectors are one selection, each file once.
	got = SelectFiles([]FileSelector{{From: "sql/**"}, {From: "./sql/orders.sql"}, {From: "agentiik.yaml"}}, theRepository)
	if want := []string{"agentiik.yaml", "sql/2026/q1.sql", "sql/README.md", "sql/orders.sql"}; !slices.Equal(got.Tree, want) {
		t.Errorf("three selectors select %q, want %q", got.Tree, want)
	}
}

// "to is an absolute path; a directory or a glob relocated there keeps each file's path below the
// directory, or below the glob's segments before its first wildcard, and mode applies to every
// file it places." And a relocated file is there instead of under /agk/repo, unless another
// selector leaves it there too.
func TestTheLongFormRelocatesAFileADirectoryOrAGlob(t *testing.T) {
	got := SelectFiles([]FileSelector{
		{From: "./sql/**/*.sql", To: "/docker-entrypoint-initdb.d", Mode: "0444"},
		{From: "./certs/internal-ca.pem", To: "/etc/ssl/certs/internal-ca.pem"},
		{From: "scripts/lib", To: "/opt/lib/"},
		{From: "./sql/**"},
	}, theRepository)
	want := []Placement{
		{Path: "sql/2026/q1.sql", To: "/docker-entrypoint-initdb.d/2026/q1.sql", Mode: "0444"},
		{Path: "sql/orders.sql", To: "/docker-entrypoint-initdb.d/orders.sql", Mode: "0444"},
		{Path: "certs/internal-ca.pem", To: "/etc/ssl/certs/internal-ca.pem"},
		{Path: "scripts/lib/rounding.py", To: "/opt/lib/rounding.py"},
	}
	if !slices.Equal(got.Placed, want) {
		t.Errorf("the long forms place\n%v\nwant\n%v", got.Placed, want)
	}
	// The certificate and the library are relocated and nothing leaves them under /agk/repo;
	// the SQL is left there by the short form as well.
	if want := []string{"sql/2026/q1.sql", "sql/README.md", "sql/orders.sql"}; !slices.Equal(got.Tree, want) {
		t.Errorf("under /agk/repo: %q, want %q", got.Tree, want)
	}
}

// A long form with a mode and no to relocates nothing: the file stays where it is, and is placed
// over itself with the mode asked for.
func TestAModeWithoutToLeavesTheFileWhereItIs(t *testing.T) {
	got := SelectFiles([]FileSelector{{From: "scripts/*.py", Mode: "0755"}}, theRepository)
	if want := []string{"scripts/normalize.py"}; !slices.Equal(got.Tree, want) {
		t.Errorf("under /agk/repo: %q, want %q", got.Tree, want)
	}
	if want := []Placement{{Path: "scripts/normalize.py", To: "/agk/repo/scripts/normalize.py", Mode: "0755"}}; !slices.Equal(got.Placed, want) {
		t.Errorf("placed %v, want %v", got.Placed, want)
	}
}

// What no tree could answer is refused before any tree is read, and selects nothing where it is
// read anyway.
func TestASelectorNoTreeCouldAnswerIsRefused(t *testing.T) {
	for _, c := range []struct {
		file FileSelector
		says string
	}{
		{FileSelector{From: "../../etc/shadow", To: "/etc/shadow"}, "leaves the repository tree"},
		{FileSelector{From: "./../secrets", To: "/run/secrets"}, "leaves the repository tree"},
		{FileSelector{From: "", To: "/etc/app.yaml"}, "names no path"},
		{FileSelector{From: "certs/ca.pem", To: "etc/ssl/ca.pem"}, "not absolute"},
		{FileSelector{From: "certs/**", To: "/"}, "root of the container"},
		{FileSelector{From: "certs/ca.pem", Mode: "644 "}, "three octal digits"},
		{FileSelector{From: "certs/ca.pem", Mode: "0999"}, "three octal digits"},
	} {
		err := CheckFiles([]FileSelector{{From: "sql/**"}, c.file})
		if err == nil || !strings.Contains(err.Error(), c.says) || !strings.HasPrefix(err.Error(), "files[1] ") {
			t.Errorf("%+v was refused with %v", c.file, err)
		}
		if got := SelectFiles([]FileSelector{c.file}, theRepository); len(got.Tree)+len(got.Placed) != 0 {
			t.Errorf("%+v selected %q and placed %v", c.file, got.Tree, got.Placed)
		}
	}
	if err := CheckFiles([]FileSelector{{From: "./sql/**/*.sql", To: "/docker-entrypoint-initdb.d", Mode: "0444"}, {From: "/certs"}}); err != nil {
		t.Errorf("the documentation's selectors are refused: %v", err)
	}
	// A selector that relocates nothing and leaves the tree selects nothing, and is not refused:
	// a version recorded before v0.4.0 carrying one ran, since nothing read it then.
	for _, f := range []FileSelector{{From: "../shared/**"}, {From: ""}, {From: "../bin/*.sh", Mode: "0755"}} {
		if err := CheckFiles([]FileSelector{f}); err != nil {
			t.Errorf("%+v is refused: %v", f, err)
		}
		if got := SelectFiles([]FileSelector{f}, theRepository); len(got.Tree)+len(got.Placed) != 0 {
			t.Errorf("%+v selected %q and placed %v", f, got.Tree, got.Placed)
		}
	}
}

// A [ that nothing closes does not read as a glob, so the segment is the name it spells: a version
// recorded before files narrowed anything may name such a path.
func TestASegmentThatIsNoGlobIsAName(t *testing.T) {
	tree := []string{"data[1/x.csv", "data1/x.csv"}
	got := SelectFiles([]FileSelector{{From: "data[1/*.csv"}}, tree)
	if want := []string{"data[1/x.csv"}; !slices.Equal(got.Tree, want) {
		t.Errorf("selects %q, want %q", got.Tree, want)
	}
	if base, err := (FileSelector{From: "./data[1/*.csv"}).Base(); err != nil || base != "data[1" {
		t.Errorf("the base is %q: %v", base, err)
	}
}

// Where a walk of a large tree starts: the path itself, or a glob's segments before its first
// wildcard.
func TestASelectorsBaseIsItsFixedPrefix(t *testing.T) {
	for from, want := range map[string]string{
		"./sql/**/*.sql":        "sql",
		"**/*.py":               ".",
		"certs/internal-ca.pem": "certs/internal-ca.pem",
		"/scripts/lib/":         "scripts/lib",
		"a/b/c*/d":              "a/b",
	} {
		if got, err := (FileSelector{From: from}).Base(); err != nil || got != want {
			t.Errorf("%s has the base %q (%v), want %q", from, got, err, want)
		}
	}
}

// A redemption reads every selector against every file of the tree, so a glob written with many
// ** against a deep tree has to cost the product of the two lengths and not a power of them.
func TestManyDoubleStarsCostNoMoreThanTheTreeIsDeep(t *testing.T) {
	glob := strings.Split(strings.Repeat("**/a/", 30)+"b", "/")
	deep := strings.Repeat("a/", 60) + "c"
	done := make(chan bool)
	go func() { done <- globMatch(glob, deep) }()
	select {
	case matched := <-done:
		if matched {
			t.Error("the glob matched a path whose last segment it does not name")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("matching one glob against one path took more than five seconds")
	}
}
