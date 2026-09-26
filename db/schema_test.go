package db

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// What the schema says about itself, held without a database wherever the files are enough,
// so that the part of this package that can be wrong on a laptop with nothing installed still
// fails there. What only PostgreSQL can answer, which type a name in a file resolves to, is
// asked of one, and skips without it as every other real test of this package does.

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

	// And what happened to a step is a third list, not a reuse of the second. A step can be
	// skipped, which no container can be, and no step is ever dispatched or publishing.
	var verdicts []string
	for v := agk.VerdictPending; len(verdicts) < tooMany; v++ {
		name := v.String()
		if strings.HasPrefix(name, "verdict ") || name == "" {
			break
		}
		verdicts = append(verdicts, name)
	}
	if len(verdicts) >= tooMany {
		t.Fatalf("the end of the verdicts could not be found: String no longer names an unknown value the way this test looks for it")
	}
	if len(verdicts) != 6 {
		t.Fatalf("package agk has %d verdicts, %v", len(verdicts), verdicts)
	}
	if slices.Contains(tasks, "skipped") || !slices.Contains(verdicts, "skipped") {
		t.Error("skipped belongs to a step and not to a task: a step whose if condition was false publishes empty envelopes and no container ran")
	}

	for _, c := range []struct {
		domain string
		want   []string
	}{
		{"run_state", runs},
		{"task_state", tasks},
		{"step_verdict", verdicts},
	} {
		got := domainValues(t, sql, c.domain)
		if !sameSet(got, c.want) {
			t.Errorf("domain %s holds %v, and package agk holds %v", c.domain, got, c.want)
		}
	}
}

// A run says what started it in the words the language uses for the block that started it,
// and the column holds exactly what the Go vocabulary can produce. A check constraint listing
// a value nothing can write says the column holds something it never holds, and one missing a
// value something can write refuses a legitimate run at three in the morning.
func TestTheTriggerKindsAreTheEngineOwn(t *testing.T) {
	sql := readMigration(t, "0001_state.sql")

	const tooMany = 64
	var kinds []string
	for k := agk.TriggerManual; len(kinds) < tooMany; k++ {
		name := k.String()
		if strings.HasPrefix(name, "trigger kind ") || name == "" {
			break
		}
		kinds = append(kinds, name)
	}
	if len(kinds) >= tooMany {
		t.Fatalf("the end of the vocabulary could not be found: String no longer names an unknown value the way this test looks for it")
	}
	// Seven, which is the Triggers table: three blocks a workflow declares and four ways a
	// run begins that no block describes.
	if len(kinds) != 7 {
		t.Fatalf("package agk has %d trigger kinds, %v, and the Triggers table names seven", len(kinds), kinds)
	}

	// schedule and not cron. The language writes on.schedule, the workflow schema declares
	// scheduleTrigger, and cron is the five-field expression inside it.
	for _, want := range []string{"manual", "schedule", "webhook", "event", "mcp", "terraform", "workflow"} {
		if !slices.Contains(kinds, want) {
			t.Errorf("package agk does not spell a trigger kind %q, and the language does", want)
		}
	}

	re := regexp.MustCompile(`(?s)trigger\s+text not null\s*\n\s*check \(trigger in \((.*?)\)\)`)
	m := re.FindStringSubmatch(sql)
	if m == nil {
		t.Fatal("runs.trigger is not a check over a list")
	}
	var held []string
	for _, v := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(m[1], -1) {
		held = append(held, v[1])
	}
	if !sameSet(held, kinds) {
		t.Errorf("runs.trigger holds %v and package agk produces %v", held, kinds)
	}
}

// The identifier a task is known by is one string, and two places build it: agk.NewTaskID for
// the wire, and a generated column for the uniqueness rule. They have to agree exactly, or
// each is right about its own key while the pair let a container start twice.
func TestTheIdempotencyKeyIsTheOneOnTheWire(t *testing.T) {
	for _, c := range []struct {
		run     agk.RunID
		step    agk.Step
		attempt int
		shard   agk.Shard
		want    string
	}{
		{"01JMZ8V1P9C4XQ7K2N4D6F8H0A", "archive", 1, agk.Shard{}, "01JMZ8V1P9C4XQ7K2N4D6F8H0A/archive/1"},
		{"01JMZ8V1P9C4XQ7K2N4D6F8H0A", "invoice", 2, agk.Shard{Index: 3, Of: 8}, "01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/2/3/8"},
	} {
		if got := string(agk.NewTaskID(c.run, c.step, c.attempt, c.shard)); got != c.want {
			t.Fatalf("agk mints %q and this test expected %q", got, c.want)
		}
	}

	// The column concatenates the same four columns in the same order, cardinality
	// included. Read from the files rather than from a live database, so the disagreement is
	// caught on a laptop with nothing installed; and from every file that writes the column,
	// since 0017 wrote it again to retype the step it reads, and the last one written is the
	// one the database holds.
	all, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	generated := regexp.MustCompile(`(?s)idempotency_key\s+text generated always as \((.*?)\)\s*stored`)
	var written int
	for _, m := range all {
		for _, expr := range generated.FindAllStringSubmatch(m.SQL, -1) {
			written++
			for _, want := range []string{
				`run_id || '/' || step || '/' || attempt`,
				`'/' || shard_index || '/' || shard_of`,
			} {
				if !strings.Contains(expr[1], want) {
					t.Errorf("the generated key %s writes does not build %s, so the key in the database is not the key on the wire", m.Name, want)
				}
			}
		}
	}
	if written == 0 {
		t.Fatal("no migration writes the generated key, so the uniqueness rule is over nothing")
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
	// Every migration, not the first one alone: a table added later is a table that would
	// otherwise arrive with nobody having decided whether it belongs to a namespace.
	all, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	var whole strings.Builder
	for _, m := range all {
		whole.WriteString(m.SQL)
		whole.WriteString("\n")
	}
	sql := whole.String()

	// The eight the Storage chapter names that belong to a namespace, plus the object
	// row that carries the reference count, where a namespace's secrets live, the
	// values the built-in store keeps for it, sealed, and the index to its tasks' logs.
	namespaced := map[string]bool{
		"workflows": true, "workflow_versions": true, "runs": true, "steps": true,
		"tasks": true, "approvals": true, "artifacts": true, "artifact_objects": true,
		"notification_events": true, "task_grants": true, "secret_declarations": true,
		"secret_values": true, "task_logs": true, "task_log_chunks": true, "task_log_objects": true,
	}
	// A runner belongs to the installation: it serves several namespaces, its inventory
	// is administrator only, and a heartbeat covers every task on one host. A namespace
	// is the name of a namespace and cannot be scoped to itself. The controller's term is
	// the installation's too: there is one active controller across all of them.
	installation := map[string]bool{
		"namespaces": true, "runners": true, "controller_term": true, "join_tokens": true,
		"runner_pools": true,
		// The audit log is one chain across the installation, holding the acts of every
		// namespace and of the installation itself, and the export reads it whole.
		"audit_log": true, "audit_head": true, "audit_export": true, "audit_verified": true,
	}

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
	// And the one column where the two vocabularies were confused: a step holds a verdict.
	if !regexp.MustCompile(`(?s)create table steps\b.*?state\s+step_verdict`).MatchString(sql) {
		t.Error("steps.state is not a step_verdict, and a step that cannot be skipped is a step the language can put in a state the column refuses")
	}

	for name := range namespaced {
		// Either in the array the first migration builds its policies from, or carrying a
		// policy of its own because it arrived later.
		if !strings.Contains(sql, "'"+name+"'") && !strings.Contains(sql, "create policy "+name+"_by_namespace") {
			t.Errorf("table %s is called namespaced and is behind no policy", name)
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
	for _, r := range []Reason{ControllerSweep, Purge, Collect, RunnerInventory, Heartbeat, Redemption, LogShipment, RunRoute, RunListing, AuditLog, NamespaceAdministration, SchemaUpgrade} {
		declared[string(r)] = true
	}
	if len(declared) != 12 {
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
		"RunnerInventory": true, "Heartbeat": true, "Redemption": true, "LogShipment": true, "RunRoute": true, "RunListing": true, "AuditLog": true, "NamespaceAdministration": true, "SchemaUpgrade": true}
	for u := range used {
		if !names[u] {
			t.Errorf("Installation is called with %s, which is not a declared Reason: an escape from the namespace has to be one of the named few", u)
		}
	}
}

// The Names table's grammar, as the documentation prints it, which the identifier domain is
// held to below.
const identifierPattern = `^[A-Za-z0-9][A-Za-z0-9_-]*$`

// Every column holding a name the workflow file writes is the identifier domain, and no column
// at all is PostgreSQL's own name type. 0001 typed seven columns name meaning its domain, and
// pg_catalog, which is searched first, answered with the type PostgreSQL names its catalog
// with: one that checks nothing and cuts a value at 63 bytes without saying so. Which type a
// bare name resolves to is PostgreSQL's to say and not the file's, so this asks a database.
func TestEveryNameTheFileWritesIsAnIdentifier(t *testing.T) {
	super, _ := database(t)
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	// The cause before the symptom: a type of ours sharing its name with one of PostgreSQL's
	// is a type no column written with it bare is ever given.
	rows, err := conn.Query(ctx,
		`select t.typname from pg_type t
		 join pg_namespace n on n.oid = t.typnamespace
		 where n.nspname = current_schema() and t.typrelid = 0 and t.typcategory <> 'A'
		   and exists (select 1 from pg_type p
		               where p.typnamespace = 'pg_catalog'::regnamespace and p.typname = t.typname)
		 order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	shadowed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range shadowed {
		t.Errorf("the schema defines a type called %s, and so does pg_catalog, which is searched first: a column typed %s is given PostgreSQL's and never this one", name, name)
	}

	// And the domain is the grammar the Names table prints and the length a directory holds a
	// name to, not a neighbour of either.
	rows, err = conn.Query(ctx,
		`select pg_get_constraintdef(c.oid) from pg_constraint c
		 join pg_type t on t.oid = c.contypid
		 join pg_namespace n on n.oid = t.typnamespace
		 where n.nspname = current_schema() and t.typname = 'identifier'`)
	if err != nil {
		t.Fatal(err)
	}
	checks, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	var grammar, bound bool
	for _, check := range checks {
		grammar = grammar || strings.Contains(check, "'"+identifierPattern+"'")
		bound = bound || strings.Contains(check, "length(") && strings.Contains(check, fmt.Sprintf("<= %d)", agk.IdentifierMaxBytes))
	}
	if len(checks) != 2 || !grammar || !bound {
		t.Errorf("the identifier domain checks %q, and what it should check is the Names table's grammar, %s, and a length of at most %d, and those alone", checks, identifierPattern, agk.IdentifierMaxBytes)
	}

	// A column called after one of the things the Names table lists holds that thing's name,
	// whichever table it is on: "A workflow name and namespace, each half of
	// <namespace>/<name>, a step, an input, an output, a port, a variable, a secret, a
	// published tool name." Namespace is on the list and not here, because every column
	// called namespace names a row of namespaces through a foreign key, and that row is held
	// to a narrower grammar of its own, lowercase and hyphenated, which lies inside this one.
	named := map[string]bool{
		"workflow": true, "step": true, "input": true, "output": true, "port": true,
		"variable": true, "secret": true, "tool": true,
	}
	// A column called name is the name of what its row is, and whether the workflow file
	// writes it depends on the row, so every table with one is decided about here.
	fileNames := map[string]bool{"workflows": true, "secret_declarations": true, "secret_values": true}
	otherNames := map[string]string{
		"namespaces":        "a namespace is held to a narrower grammar of its own",
		"runner_pools":      "a pool is named by an administrator, not by the workflow file",
		"artifacts":         "an artifact is named after the file it is, dot and all, and held to one segment of its URI",
		"schema_migrations": "a migration is named after its file, dot and all",
	}

	identifiers := map[string]bool{}
	for _, c := range schemaColumns(t, conn) {
		where := c.table + "." + c.column
		postgres := c.typeSchema == "pg_catalog" && c.typeName == "name"
		if postgres {
			t.Errorf("%s is PostgreSQL's own name type, which checks nothing and cuts a value at 63 bytes without saying so", where)
		}
		holds := named[c.column]
		if c.column == "name" {
			if _, decided := otherNames[c.table]; !fileNames[c.table] && !decided {
				t.Errorf("%s is a name nobody decided about: it is one the workflow file writes and an identifier, or it is something else and says why here", where)
				continue
			}
			holds = fileNames[c.table]
		}
		if !holds {
			continue
		}
		identifiers[where] = true
		if !postgres && c.typeName != "identifier" {
			t.Errorf("%s holds a name the workflow file writes and is %s.%s rather than an identifier", where, c.typeSchema, c.typeName)
		}
	}

	// The seven 0001 typed name and the two 0015 and 0016 wrote out by hand, so that a query
	// which stopped seeing columns cannot pass everything above by seeing nothing.
	for _, want := range []string{
		"workflows.name", "workflow_versions.workflow", "runs.workflow", "steps.step",
		"tasks.step", "artifacts.step", "artifacts.port", "secret_declarations.name",
		"secret_values.name",
	} {
		if !identifiers[want] {
			t.Errorf("%s was not found among the columns holding a name the workflow file writes", want)
		}
	}
}

// A name longer than PostgreSQL's own is kept whole by every column holding one, and a name off
// the grammar is refused by every one of them. While those columns were PostgreSQL's name, two
// workflow names sharing their first 63 bytes were one workflow, and a step called "two words"
// was stored as written. A name longer than a directory holds is refused by every one of them
// too, as it is everywhere a name is written: left to the index a name is also a key of, one of
// a few kilobytes was refused there, as a failure of the database and at every run.
func TestANameLongerThanPostgreSQLsOwnIsKeptWhole(t *testing.T) {
	super, _ := database(t)
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	// Every one past 63 bytes, and the two workflows alike for the whole of the first 63. The
	// step and the port are as long as a name may be.
	stem := strings.Repeat("a", 63)
	workflow, twin := stem+"-monthly", stem+"-weekly"
	step := "normalize-" + strings.Repeat("s", agk.IdentifierMaxBytes-len("normalize-"))
	port := "rejected-" + strings.Repeat("p", agk.IdentifierMaxBytes-len("rejected-"))
	secret := "billing-" + strings.Repeat("k", 60)
	const run = "01JMZ8V1P9C4XQ7K2N4D6F8H0A"

	for _, s := range []struct {
		sql  string
		args []any
	}{
		{`insert into namespaces (name) values ('finance')`, nil},
		{`insert into workflows (namespace, name) values ('finance', $1), ('finance', $2)`, []any{workflow, twin}},
		{`insert into workflow_versions (namespace, workflow, commit, graph, author, created_at)
		  values ('finance', $1, 'a3f9c1e', '{}', 'alice', now())`, []any{workflow}},
		{`insert into runs (namespace, id, workflow, commit, trigger)
		  values ('finance', $1, $2, 'a3f9c1e', 'manual')`, []any{run, workflow}},
		{`insert into steps (namespace, run_id, step) values ('finance', $1, $2)`, []any{run, step}},
		{`insert into tasks (namespace, id, run_id, step, attempt)
		  values ('finance', '01JMZ8V1PC7K3M0', $1, $2, 1)`, []any{run, step}},
		{`insert into artifacts (namespace, run_id, step, port, name, digest, size_bytes, media_type, expires_at)
		  values ('finance', $1, $2, $3, 'orders.csv', $4, 1, 'text/csv', now() + interval '1 day')`,
			[]any{run, step, port, "sha256:" + strings.Repeat("0", 64)}},
		{`insert into secret_declarations (namespace, name, provider, declared_by)
		  values ('finance', $1, 'builtin', 'alice')`, []any{secret}},
		{`insert into secret_values (namespace, name, version) values ('finance', $1, 0)`, []any{secret}},
	} {
		if _, err := conn.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("%s: %s", s.sql, err)
		}
	}

	written := map[string][]string{
		"workflows.name":             {workflow, twin},
		"workflow_versions.workflow": {workflow},
		"runs.workflow":              {workflow},
		"steps.step":                 {step},
		"tasks.step":                 {step},
		"artifacts.step":             {step},
		"artifacts.port":             {port},
		"secret_declarations.name":   {secret},
		"secret_values.name":         {secret},
	}
	for _, c := range schemaColumns(t, conn) {
		if c.typeName == "identifier" && written[c.table+"."+c.column] == nil {
			t.Errorf("%s.%s is an identifier and this test writes nothing into it, so it holds nothing about it: give it a row above", c.table, c.column)
		}
	}

	for where, want := range written {
		table, column, _ := strings.Cut(where, ".")
		rows, err := conn.Query(ctx, fmt.Sprintf(`select %s::text from %s order by 1`,
			pgx.Identifier{column}.Sanitize(), pgx.Identifier{table}.Sanitize()))
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s holds %q, and what was written is %q", where, got, want)
		}

		// Refused by the domain itself, and not by whatever else the row happens to check.
		_, err = conn.Exec(ctx, fmt.Sprintf(`update %s set %s = 'two words'`,
			pgx.Identifier{table}.Sanitize(), pgx.Identifier{column}.Sanitize()))
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != checkViolation || pg.DataTypeName != "identifier" {
			t.Errorf("%s took a name with a space in it and answered %v, where the identifier domain refuses it", where, err)
		}
		_, err = conn.Exec(ctx, fmt.Sprintf(`update %s set %s = $1`,
			pgx.Identifier{table}.Sanitize(), pgx.Identifier{column}.Sanitize()),
			strings.Repeat("n", agk.IdentifierMaxBytes+1))
		if !errors.As(err, &pg) || pg.Code != checkViolation || pg.DataTypeName != "identifier" {
			t.Errorf("%s took a name of %d characters and answered %v, where the identifier domain refuses it", where, agk.IdentifierMaxBytes+1, err)
		}
	}

	// The key a task is known by carries the step whole too, since it is built from the column.
	var key string
	if err := conn.QueryRow(ctx, `select idempotency_key from tasks`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if want := run + "/" + step + "/1"; key != want {
		t.Errorf("the idempotency key is %q, and the task's is %q", key, want)
	}
}

// checkViolation is PostgreSQL's code for a value a check refused, which is what a domain's
// check raises.
const checkViolation = "23514"

// column is one column of a table of the schema, with the type it was given as PostgreSQL
// resolved it.
type column struct {
	table, column        string
	typeSchema, typeName string
}

// schemaColumns reads every column of every table in the schema the migrations ran in.
func schemaColumns(t *testing.T, conn *pgx.Conn) []column {
	t.Helper()
	rows, err := conn.Query(t.Context(),
		`select c.relname::text, a.attname::text, tn.nspname::text, ty.typname::text
		 from pg_attribute a
		 join pg_class c on c.oid = a.attrelid
		 join pg_namespace cn on cn.oid = c.relnamespace
		 join pg_type ty on ty.oid = a.atttypid
		 join pg_namespace tn on tn.oid = ty.typnamespace
		 where cn.nspname = current_schema() and c.relkind in ('r', 'p')
		   and a.attnum > 0 and not a.attisdropped
		 order by 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	var out []column
	for rows.Next() {
		var c column
		if err := rows.Scan(&c.table, &c.column, &c.typeSchema, &c.typeName); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("the schema has no column at all, so nothing here holds anything")
	}
	return out
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
