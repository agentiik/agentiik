package api_test

import (
	"bytes"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// selectingTree is a repository a narrowed step reads part of: the entry point, SQL at two depths
// and a note beside it, a certificate, and fixtures no step here asks for.
func selectingTree() map[string]api.PushFile {
	return map[string]api.PushFile{
		"agentiik.yaml":         {Content: []byte("apiVersion: agentiik.dev/v1\nkind: Workflow\n"), Mode: "0644"},
		"sql/orders.sql":        {Content: []byte("select * from orders;\n"), Mode: "0644"},
		"sql/2026/q1.sql":       {Content: []byte("select * from q1;\n"), Mode: "0644"},
		"sql/README.md":         {Content: []byte("# The queries\n"), Mode: "0644"},
		"certs/internal-ca.pem": {Content: []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"), Mode: "0644"},
		"fixtures/big.bin":      {Content: []byte("two gibibytes, in spirit\n"), Mode: "0644"},
	}
}

// entriesOf reads a redemption's tree as one line per entry, path, mode and, where it has one, to.
func entriesOf(t *testing.T, g api.Grant) []string {
	t.Helper()
	var read []string
	for _, e := range g.Tree {
		line := e.Path + " " + e.Mode
		if e.To != "" {
			line += " " + e.To
		}
		read = append(read, line)
	}
	return read
}

// "Apply the step's files selection at fetch time, so a narrowed step downloads only what it asked
// for." The redemption of a narrowed step names the files its selectors select and no others, and
// a runner holds no URL for the rest: not the fixtures beside the SQL, not the entry point.
func TestANarrowedStepIsHandedOnlyWhatItSelected(t *testing.T) {
	g := withGrants(t, api.NoSecrets{})
	credential := g.joined(t)
	g.recorded(t, "d6c2f4b", selectingTree())

	clear := g.granted(t, db.GrantScope{
		Run: grantRun, Step: "render", Workflow: "monthly-invoicing", Commit: "d6c2f4b",
		Files: []db.GrantFile{{From: "./sql/**/*.sql"}, {From: "sql/README.md"}},
	})
	answer := g.redeemed(t, credential, asking(clear))
	if got, want := entriesOf(t, answer), []string{"sql/2026/q1.sql 0644", "sql/README.md 0644", "sql/orders.sql 0644"}; !slices.Equal(got, want) {
		t.Errorf("a step narrowed to its SQL is handed %q, want %q", got, want)
	}
	for path, f := range selectingTree() {
		if strings.HasPrefix(path, "sql/") {
			continue
		}
		for _, e := range answer.Tree {
			if strings.Contains(e.URL, digestOf(f.Content)) {
				t.Errorf("the redemption holds a URL for %s, which the step did not select", path)
			}
		}
	}
	// And each URL fetches the bytes that were committed.
	for _, e := range answer.Tree {
		res := follow(t, g.handler, "GET", e.URL, "")
		if res.Code != http.StatusOK || !bytes.Equal(res.Body.Bytes(), selectingTree()[e.Path].Content) {
			t.Errorf("fetching %s answered %d %q", e.Path, res.Code, res.Body.Bytes())
		}
	}
}

// "Give a step without files the whole tree, narrowing being an optimisation, not a permission
// boundary": a scope naming no files is answered with every file of the version, which is also
// what a grant written before a scope named files is answered with.
func TestAStepWithoutFilesIsHandedTheWholeTree(t *testing.T) {
	g := withGrants(t, api.NoSecrets{})
	credential := g.joined(t)
	g.recorded(t, "d6c2f4b", selectingTree())

	answer := g.redeemed(t, credential, asking(g.grantedFor(t, "monthly-invoicing", "d6c2f4b")))
	var want []string
	for path, f := range selectingTree() {
		want = append(want, path+" "+f.Mode)
	}
	slices.Sort(want)
	if got := entriesOf(t, answer); !slices.Equal(got, want) {
		t.Errorf("a step without files is handed %q, want %q", got, want)
	}
}

// The long form "relocates a path to wherever a tool insists on finding it": a file to its own
// path, and a glob into a directory, each file keeping its path below the glob's fixed prefix,
// with the mode asked for. A relocation "travels as the entry's own to", so the runner evaluates
// no path rule, and what the short form leaves where it is travels without one.
func TestARelocationTravelsAsEachEntrysOwnTo(t *testing.T) {
	g := withGrants(t, api.NoSecrets{})
	credential := g.joined(t)
	g.recorded(t, "d6c2f4b", selectingTree())

	clear := g.granted(t, db.GrantScope{
		Run: grantRun, Step: "render", Workflow: "monthly-invoicing", Commit: "d6c2f4b",
		Files: []db.GrantFile{
			{From: "./sql/**/*.sql", To: "/docker-entrypoint-initdb.d", Mode: "0444"},
			{From: "./certs/internal-ca.pem", To: "/etc/ssl/certs/internal-ca.pem"},
			{From: "./agentiik.yaml"},
			{From: "sql/README.md", Mode: "0600"},
		},
	})
	w, raw := call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential, asking(clear))
	if w.Code != http.StatusOK {
		t.Fatalf("redeeming answered %d: %s", w.Code, w.Body)
	}
	if err := conforms(t, "/$defs/grantRedemption/properties/response/properties/tree", raw["tree"]); err != nil {
		t.Errorf("the tree is not what the wire describes: %s", err)
	}
	answer := g.redeemed(t, credential, asking(clear))
	want := []string{
		"agentiik.yaml 0644",
		"sql/README.md 0644",
		"sql/README.md 0600 /agk/repo/sql/README.md",
		"sql/2026/q1.sql 0444 /docker-entrypoint-initdb.d/2026/q1.sql",
		"sql/orders.sql 0444 /docker-entrypoint-initdb.d/orders.sql",
		"certs/internal-ca.pem 0644 /etc/ssl/certs/internal-ca.pem",
	}
	if got := entriesOf(t, answer); !slices.Equal(got, want) {
		t.Errorf("the relocations are handed as\n%q\nwant\n%q", got, want)
	}
}

// A version recorded before a step's files selected what a runner is handed runs as it ran: its
// tasks are handed its tree whole, whatever its steps' files select, and what those relocate is
// placed from that tree on the runner, as it was then. One recorded now is narrowed.
func TestAVersionRecordedBeforeFilesSelectedIsHandedItsTreeWhole(t *testing.T) {
	g := withGrants(t, api.NoSecrets{})
	credential := g.joined(t)
	g.recorded(t, "d6c2f4b", selectingTree())
	scope := db.GrantScope{
		Run: grantRun, Step: "render", Workflow: "monthly-invoicing", Commit: "d6c2f4b",
		Files: []db.GrantFile{{From: "./sql/**/*.sql"}, {From: "./certs/internal-ca.pem", To: "/etc/ssl/certs/internal-ca.pem"}},
	}
	narrowed := entriesOf(t, g.redeemed(t, credential, asking(g.granted(t, scope))))
	if want := []string{"sql/2026/q1.sql 0644", "sql/orders.sql 0644", "certs/internal-ca.pem 0644 /etc/ssl/certs/internal-ca.pem"}; !slices.Equal(narrowed, want) {
		t.Fatalf("a version recorded now is handed %q, want %q", narrowed, want)
	}

	// As v0.3.0 recorded it: the same bundle with no word about files.
	if _, err := dbtest.Superuser(t, g.super).Exec(t.Context(),
		`update workflow_versions set graph = graph - 'selects_files' where commit = 'd6c2f4b'`); err != nil {
		t.Fatal(err)
	}
	var want []string
	for path, f := range selectingTree() {
		want = append(want, path+" "+f.Mode)
	}
	slices.Sort(want)
	if got := entriesOf(t, g.redeemed(t, credential, asking(g.granted(t, scope)))); !slices.Equal(got, want) {
		t.Errorf("a version recorded before files selected is handed %q, want the whole tree %q", got, want)
	}
}
