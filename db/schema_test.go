package db

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
)

// What the schema says about itself, held without a database, so that the part of this
// package that can be wrong on a laptop with nothing installed still fails there.

// A vocabulary written twice is a vocabulary that drifts, and this one is written three
// times over: in package agk, in agentiik/schemas on the wire, and in a check constraint
// here. The schema and the engine are held together here; the wire is held to the same
// list by the fixtures in its own repository.
func TestTheStateVocabulariesAreTheEngineOwn(t *testing.T) {
	sql := readMigration(t, "0001_state.sql")

	// String names a value outside the list "run state 7" rather than spelling one, so
	// that is the end of the list. The bound is there because a loop whose sentinel is a
	// message hangs rather than fails when somebody rewords the message, and a hanging
	// test says nothing at all.
	const tooMany = 64
	var runs []string
	for s := agk.Queued; len(runs) < tooMany; s++ {
		name := s.String()
		if strings.HasPrefix(name, "run state ") || name == "" {
			break
		}
		runs = append(runs, name)
	}
	var tasks []string
	for s := agk.TaskPending; len(tasks) < tooMany; s++ {
		name := s.String()
		if strings.HasPrefix(name, "task state ") || name == "" {
			break
		}
		tasks = append(tasks, name)
	}
	if len(runs) >= tooMany || len(tasks) >= tooMany {
		t.Fatalf("the end of a vocabulary could not be found: String no longer names an unknown value the way this test looks for it")
	}

	if len(runs) != 7 {
		t.Fatalf("package agk has %d run states, %v, and the documentation names seven", len(runs), runs)
	}
	// Nine since the page named the two the engine had always produced: a task stopped
	// at its deadline and a task stopped by a cancellation are neither of them failed.
	if len(tasks) != 9 {
		t.Fatalf("package agk has %d task states, %v, and the documentation names nine", len(tasks), tasks)
	}

	for _, c := range []struct {
		domain string
		want   []string
	}{
		{"run_state", runs},
		{"task_state", tasks},
	} {
		got := domainValues(t, sql, c.domain)
		if !sameSet(got, c.want) {
			t.Errorf("domain %s holds %v, and package agk holds %v", c.domain, got, c.want)
		}
	}
}

// The migrations are applied in the order their names sort in, so a name that sorted
// differently from the number a reader sees would apply in an order nobody intended.
func TestTheMigrationsAreOrderedAsTheyAreNamed(t *testing.T) {
	all, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("no migration is embedded, so an installation would come up with no schema at all")
	}
	numbered := regexp.MustCompile(`^\d{4}_[a-z0-9_]+\.sql$`)
	for i, m := range all {
		if !numbered.MatchString(m.Name) {
			t.Errorf("%s is not named <four digits>_<name>.sql, and the order is the name", m.Name)
		}
		if i > 0 && all[i-1].Name >= m.Name {
			t.Errorf("%s sorts before %s", m.Name, all[i-1].Name)
		}
		if strings.TrimSpace(m.SQL) == "" {
			t.Errorf("%s is empty", m.Name)
		}
	}
}

// Every table the schema creates is either namespaced and behind a policy, or it is on the
// installation side and says so here. A table that appeared in neither list would be a
// table nobody decided about, which is how a tenant's rows end up readable by another.
func TestEveryTableIsDecidedAbout(t *testing.T) {
	sql := readMigration(t, "0001_state.sql")

	// The eight the Storage chapter names that belong to a namespace, plus the object
	// row that carries the reference count.
	namespaced := map[string]bool{
		"workflows": true, "workflow_versions": true, "runs": true, "steps": true,
		"tasks": true, "approvals": true, "artifacts": true, "artifact_objects": true,
	}
	// A runner belongs to the installation: it serves several namespaces, its inventory
	// is administrator only, and a heartbeat covers every task on one host. A namespace
	// is the name of a namespace and cannot be scoped to itself.
	installation := map[string]bool{"namespaces": true, "runners": true}

	created := regexp.MustCompile(`(?m)^create table (\w+)`).FindAllStringSubmatch(sql, -1)
	if len(created) == 0 {
		t.Fatal("the migration creates no table")
	}
	for _, m := range created {
		name := m[1]
		if !namespaced[name] && !installation[name] {
			t.Errorf("table %s is in neither list: a table is namespaced and behind a policy, or it is the installation's and says why", name)
		}
	}

	// And what the lists claim is enforced in the file rather than hoped for. The
	// policies are built by a loop over an array of names, so being in that array is
	// what puts a table behind one.
	for _, want := range []string{
		"enable row level security",
		"force row level security",
		"agentiik_namespace()",
		"agentiik_installation()",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("the migration never says %q, so the namespace is a column and not a rule", want)
		}
	}
	for name := range namespaced {
		if !strings.Contains(sql, "'"+name+"'") {
			t.Errorf("table %s is called namespaced and is not in the list the policies are built from", name)
		}
		if !regexp.MustCompile(`(?s)create table ` + name + `\b.*?namespace\s+text not null`).MatchString(sql) {
			t.Errorf("table %s is called namespaced and has no namespace column", name)
		}
	}
}

// The escapes from the namespace rule are countable, which is the whole of what makes them
// tolerable. Every one of them names a Reason, and the set of reasons used is the set
// declared: a new escape is a line somebody adds here, not a habit that spreads.
func TestEveryEscapeIsNamed(t *testing.T) {
	declared := map[string]bool{}
	for _, r := range []Reason{ControllerSweep, Purge, Collect, RunnerInventory, Heartbeat, SchemaUpgrade} {
		declared[string(r)] = true
	}
	if len(declared) != 6 {
		t.Fatalf("two reasons share a string: %v", declared)
	}

	used := map[string]bool{}
	root := ".."
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range regexp.MustCompile(`Installation\(\s*\w+\s*,\s*(?:db\.)?(\w+)\s*,`).FindAllStringSubmatch(string(body), -1) {
			used[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{"ControllerSweep": true, "Purge": true, "Collect": true,
		"RunnerInventory": true, "Heartbeat": true, "SchemaUpgrade": true}
	for u := range used {
		if !names[u] {
			t.Errorf("Installation is called with %s, which is not a declared Reason: an escape from the namespace has to be one of the named few", u)
		}
	}
}

func readMigration(t *testing.T, name string) string {
	t.Helper()
	all, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range all {
		if m.Name == name {
			return m.SQL
		}
	}
	t.Fatalf("%s is not embedded", name)
	return ""
}

// domainValues reads the list a check constraint holds a domain to.
func domainValues(t *testing.T, sql, domain string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?s)create domain ` + domain + ` as text\s*\n\s*check \(value in \((.*?)\)\)`)
	m := re.FindStringSubmatch(sql)
	if m == nil {
		t.Fatalf("domain %s is not declared as a check over a list", domain)
	}
	var out []string
	for _, v := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(m[1], -1) {
		out = append(out, v[1])
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
