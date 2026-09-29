package db

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/audit"
	"github.com/jackc/pgx/v5"
)

// "A move ... is refused with 409 while a run of it is queued or running, where the target's quotas
// cannot hold what moves": each refusal, in the order a move is judged, and a move asked freezing
// the workflow in its namespace and holding its name in the target.
func TestAMoveIsJudgedAndFreezesItsWorkflow(t *testing.T) {
	pool, super := opened(t)
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	exec := func(stmt string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, stmt, args...); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
	ask := func(target string) error {
		return pool.AskMove(ctx, "finance", "monthly-invoicing", target, "alice", time.Now(), nil)
	}

	// The run the seed leaves queued has not finished.
	if err := ask("team-ops"); !errors.Is(err, ErrRunsGoing) {
		t.Fatalf("a move with a run queued answered %v", err)
	}
	exec(`update runs set state = 'succeeded', started_at = now(), finished_at = now() where id = $1`, financeRun)
	if err := ask("nowhere"); !errors.Is(err, ErrNoNamespace) {
		t.Errorf("a move to a namespace that does not exist answered %v", err)
	}

	// A run of another workflow that republished what this one's made.
	exec(`insert into workflows (namespace, name) values ('finance', 'payroll')`)
	exec(`insert into workflow_versions (namespace, workflow, commit, graph, author, created_at) values ('finance', 'payroll', 'b4a0d2f', '{}', 'alice', now())`)
	const other = "01M2T1BBBBBBBBBBBBBBBBBBBB"
	exec(`insert into runs (namespace, id, workflow, commit, trigger, state) values ('finance', $1, 'payroll', 'b4a0d2f', 'manual', 'succeeded')`, other)
	exec(`insert into steps (namespace, run_id, step) values ('finance', $1, 'archive')`, other)
	exec(`insert into tasks (namespace, id, run_id, step, attempt, state, memoised_from) values ('finance', '01M2T1CCCCCCCCCCCCCCCCCCCC', $1, 'archive', 1, 'succeeded', $2)`, other, financeRun)
	var shared *SharedOutputs
	if err := ask("team-ops"); !errors.As(err, &shared) || shared.Run != other || shared.From != financeRun {
		t.Errorf("a move whose run's outputs another workflow's run republished answered %v", err)
	}
	exec(`update tasks set memoised_from = null`)

	// The name taken in the target, and one being purged there.
	exec(`insert into workflows (namespace, name) values ('team-ops', 'monthly-invoicing')`)
	if err := ask("team-ops"); !errors.Is(err, ErrWorkflowExists) {
		t.Errorf("a move to a namespace holding the name answered %v", err)
	}
	exec(`update workflows set deleted_at = now(), deleted_by = 'bob' where namespace = 'team-ops' and name = 'monthly-invoicing'`)
	if err := ask("team-ops"); !errors.Is(err, ErrWorkflowPurging) {
		t.Errorf("a move to a namespace purging the name answered %v", err)
	}
	exec(`delete from workflows where namespace = 'team-ops' and name = 'monthly-invoicing'`)

	// A target whose max_artifact_bytes cannot hold the live artifacts that move.
	exec(`insert into artifacts (namespace, run_id, step, port, name, digest, size_bytes, media_type, expires_at)
	      values ('finance', $1, 'archive', 'out', 'invoices.zip', $2, 4096, 'application/zip', now() + interval '1 day')`, financeRun, "sha256:"+digestOf("a"))
	exec(`update namespaces set max_artifact_bytes = 1024 where name = 'team-ops'`)
	var room *NoRoomToMove
	if err := ask("team-ops"); !errors.As(err, &room) || room.Moving != 4096 || room.Limit != 1024 {
		t.Errorf("a move past the target's max_artifact_bytes answered %v", err)
	}
	// And one that would fit alone and does not beside a move asked to the target before it.
	exec(`insert into artifacts (namespace, run_id, step, port, name, digest, size_bytes, media_type, expires_at)
	      values ('finance', $1, 'archive', 'out', 'payslips.zip', $2, 2048, 'application/zip', now() + interval '1 day')`, other, "sha256:"+digestOf("e"))
	exec(`update namespaces set max_artifact_bytes = 5000 where name = 'team-ops'`)
	if err := pool.AskMove(ctx, "finance", "payroll", "team-ops", "alice", time.Now(), nil); err != nil {
		t.Fatalf("a move within the target's max_artifact_bytes answered %v", err)
	}
	if err := ask("team-ops"); !errors.As(err, &room) || room.Moving != 6144 || room.Limit != 5000 {
		t.Errorf("a move past the target's max_artifact_bytes beside another asked there answered %v", err)
	}
	exec(`delete from workflow_moves where workflow = 'payroll'`)
	exec(`update namespaces set max_artifact_bytes = 1024 where name = 'team-ops'`)

	// What the request changes besides is undone with a move refused, and a change refused is
	// told from the move's refusals.
	var records int
	if err := conn.QueryRow(ctx, `select count(*) from audit_log`).Scan(&records); err != nil {
		t.Fatal(err)
	}
	rename := func(ctx context.Context, ns *NS) (string, []audit.Record, error) {
		if err := ns.RenameWorkflow(ctx, "monthly-invoicing", "invoicing"); err != nil {
			return "", nil, err
		}
		return "invoicing", []audit.Record{{Actor: "alice", Action: audit.WorkflowUpdate, Target: "invoicing", Result: audit.Done,
			Detail: map[string]any{"name": "invoicing", "was": map[string]any{"name": "monthly-invoicing"}}}}, nil
	}
	if err := pool.AskMove(ctx, "finance", "monthly-invoicing", "team-ops", "alice", time.Now(), rename); !errors.As(err, &room) {
		t.Errorf("a move past the target's max_artifact_bytes with a rename answered %v", err)
	}
	var still, after int
	if err := conn.QueryRow(ctx, `select (select count(*) from workflows where namespace = 'finance' and name = 'monthly-invoicing'), (select count(*) from audit_log)`).Scan(&still, &after); err != nil || still != 1 || after != records {
		t.Errorf("a move refused left the workflow renamed (%d under its name) or recorded (%d entries, %d before): %v", still, after, records, err)
	}
	var besides *ChangeRefused
	if err := pool.AskMove(ctx, "finance", "monthly-invoicing", "team-ops", "alice", time.Now(), func(ctx context.Context, ns *NS) (string, []audit.Record, error) {
		return "", nil, ns.RenameWorkflow(ctx, "monthly-invoicing", "payroll")
	}); !errors.As(err, &besides) || !errors.Is(err, ErrWorkflowExists) {
		t.Errorf("a move with a rename to a name its namespace holds answered %v", err)
	}
	exec(`update namespaces set max_artifact_bytes = null where name = 'team-ops'`)

	// Asked, with a rename: under the new name, which it holds in the target, recorded as a rename
	// in the transaction that asks it. A cache entry of it goes.
	exec(`insert into step_cache (namespace, key, run_id, step, ports) values ('finance', 'finance/sha256/1', $1, 'archive', '[]')`, financeRun)
	if err := pool.AskMove(ctx, "finance", "monthly-invoicing", "team-ops", "alice", time.Now(), rename); err != nil {
		t.Fatalf("a move with a rename that may be made answered %v", err)
	}
	var target string
	if err := conn.QueryRow(ctx, `select m.target from workflow_moves m join move_targets h on h.from_namespace = m.namespace and h.name = m.workflow where m.workflow = 'invoicing' and h.namespace = 'team-ops'`).Scan(&target); err != nil || target != "team-ops" {
		t.Errorf("the move with a rename is asked as %q: %v", target, err)
	}
	if err := conn.QueryRow(ctx, `select count(*) from audit_log where namespace = 'finance' and target = 'invoicing'`).Scan(&after); err != nil || after != 1 {
		t.Errorf("the rename asked with the move is recorded %d times: %v", after, err)
	}
	exec(`delete from workflow_moves`)
	exec(`update workflows set name = 'monthly-invoicing' where namespace = 'finance' and name = 'invoicing'`)
	exec(`insert into step_cache (namespace, key, run_id, step, ports) values ('finance', 'finance/sha256/1', $1, 'archive', '[]')`, financeRun)
	var held int
	if err := ask("team-ops"); err != nil {
		t.Fatalf("a move that may be made answered %v", err)
	}
	if err := conn.QueryRow(ctx, `select count(*) from step_cache`).Scan(&held); err != nil || held != 0 {
		t.Errorf("the moving workflow's cache entries are still there: %d, %v", held, err)
	}
	if err := ask("team-ops"); !errors.Is(err, ErrWorkflowMoving) {
		t.Errorf("a second move answered %v", err)
	}

	// Frozen where it is: a push, a run, a rename, a deletion.
	for what, write := range map[string]func(context.Context, *NS) error{
		"the lock a push takes": func(ctx context.Context, ns *NS) error { return ns.HoldRepository(ctx, "monthly-invoicing") },
		"a run": func(ctx context.Context, ns *NS) error {
			return ns.CreateRun(ctx, NewRun{ID: "01M2T1DDDDDDDDDDDDDDDDDDDD", Workflow: "monthly-invoicing", Commit: "a3f9c1e", Trigger: agk.TriggerManual, TriggeredBy: "alice", Steps: []agk.Step{"archive"}})
		},
		"a rename": func(ctx context.Context, ns *NS) error {
			return ns.RenameWorkflow(ctx, "monthly-invoicing", "invoicing")
		},
		"a deletion": func(ctx context.Context, ns *NS) error {
			_, err := ns.DeleteWorkflow(ctx, "monthly-invoicing", "alice", time.Now())
			return err
		},
	} {
		if err := pool.In(ctx, "finance", func(ctx context.Context, ns *NS) error { return write(ctx, ns) }); !errors.Is(err, ErrWorkflowMoving) {
			t.Errorf("%s of a moving workflow answered %v", what, err)
		}
	}
	// And its name held in the target, against a workflow created there, and another move.
	if err := pool.In(ctx, "team-ops", func(ctx context.Context, ns *NS) error {
		_, err := ns.CreateWorkflow(ctx, "monthly-invoicing", "main", false, "bob", time.Now())
		return err
	}); !errors.Is(err, ErrWorkflowExists) {
		t.Errorf("a workflow created under a name a move holds answered %v", err)
	}
	if err := pool.In(ctx, "team-ops", func(ctx context.Context, ns *NS) error {
		return ns.SaveWorkflow(ctx, "monthly-invoicing", "main")
	}); !errors.Is(err, ErrWorkflowExists) {
		t.Errorf("a tree pushed under a name a move holds answered %v", err)
	}
	exec(`insert into workflows (namespace, name) values ('team-ops', 'invoicing')`)
	if err := pool.In(ctx, "team-ops", func(ctx context.Context, ns *NS) error {
		return ns.RenameWorkflow(ctx, "invoicing", "monthly-invoicing")
	}); !errors.Is(err, ErrWorkflowExists) {
		t.Errorf("a rename to a name a move holds answered %v", err)
	}
}

// "Carries everything too, grants included, runs and artifacts re-keyed and re-counted under the
// target": what the workflow's rows count is counted in the target and let go in the source, its
// rows are all in the target, its logs' keys rewritten, what stays under the source's keys kept
// until the grace, its expiries held to the target's retention, and the move recorded in both.
func TestAMoveCarriesEverythingAndRecountsIt(t *testing.T) {
	pool, super := opened(t)
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	exec := func(stmt string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, stmt, args...); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
	tree, artifact := "sha256:"+digestOf("b"), "sha256:"+digestOf("c")
	exec(`update workflow_versions set tree = $1 where namespace = 'finance' and commit = 'a3f9c1e'`,
		`[{"path":"agentiik.yaml","sha256":"`+digestOf("b")+`","size":21,"mode":"0644"},{"path":"again.yaml","sha256":"`+digestOf("b")+`","size":21,"mode":"0644"}]`)
	exec(`insert into artifact_objects (namespace, digest, size_bytes, media_type, refs) values ('finance', $1, 21, 'application/octet-stream', 1), ('finance', $2, 4096, 'application/zip', 1)`, tree, artifact)
	exec(`update runs set state = 'succeeded', started_at = now(), finished_at = now() - interval '1 day', expires_at = now() + interval '80 days' where id = $1`, financeRun)
	exec(`insert into artifacts (namespace, run_id, step, port, name, digest, size_bytes, media_type, expires_at, created_at)
	      values ('finance', $1, 'archive', 'out', 'invoices.zip', $2, 4096, 'application/zip', now() + interval '80 days', now() - interval '1 day')`, financeRun, artifact)
	exec(`insert into principals (id, kind) values ('alice', 'user')`)
	exec(`insert into grants (id, namespace, workflow, principal, role, granted_by) values ('01JQ3M8T', 'finance', 'monthly-invoicing', 'alice', 'viewer', 'alice')`)
	const task = "01M2T1AAAAAAAAAAAAAAAAAAAA"
	logKey := "finance/logs/" + financeRun + "/" + task + "/1/1-" + digestOf("d")
	exec(`insert into task_logs (namespace, task_id, next_seq) values ('finance', $1, 2)`, task)
	exec(`insert into task_log_chunks (namespace, task_id, seq, first_line, shipped, shipped_digest, lines, bytes, object_key, object_digest)
	      values ('finance', $1, 1, 1, 3, $2, 3, 30, $3, $2)`, task, digestOf("d"), logKey)
	var repository string
	if err := conn.QueryRow(ctx, `select repository from workflows where namespace = 'finance' and name = 'monthly-invoicing'`).Scan(&repository); err != nil {
		t.Fatal(err)
	}
	pack := strings.Repeat("e", 40)
	exec(`insert into git_packs (namespace, repository, name, size, objects, state) values ('finance', $1, $2, 100, 3, 'live')`, repository, pack)
	exec(`update namespaces set max_retention_days = 30 where name = 'team-ops'`)
	exec(`insert into workflow_refs (namespace, workflow, ref) values ('finance', 'monthly-invoicing', 'refs/heads/main')`)

	if err := pool.AskMove(ctx, "finance", "monthly-invoicing", "team-ops", "alice", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	moves, err := pool.Moves(ctx, 10)
	if err != nil || len(moves) != 1 || moves[0].Target != "team-ops" || moves[0].Repository != repository || moves[0].AskedBy != "alice" {
		t.Fatalf("the moves asked read %+v: %v", moves, err)
	}
	m := moves[0]
	objects, err := pool.ObjectsOf(ctx, m)
	if err != nil || !objects.Ready {
		t.Fatalf("what the move copies read %+v: %v", objects, err)
	}
	if !slices.Equal(objects.Digests, []string{digestOf("b"), digestOf("c")}) || !slices.Equal(objects.Logs, []string{logKey}) || !slices.Equal(objects.Packs, []string{pack}) {
		t.Errorf("the move copies %+v", objects)
	}

	moved, err := pool.CompleteMove(ctx, m, objects, time.Hour)
	if err != nil || !moved.Done {
		t.Fatalf("the move was carried out as %+v: %v", moved, err)
	}
	if !slices.Equal(moved.MustWrite, []string{digestOf("b"), digestOf("c")}) {
		t.Errorf("the move asks for %v to be written again, where the target held a row of neither", moved.MustWrite)
	}

	// Every row where it moved.
	for table, where := range map[string]string{
		"workflows": "name = 'monthly-invoicing'", "workflow_versions": "workflow = 'monthly-invoicing'",
		"workflow_refs": "workflow = 'monthly-invoicing'", "runs": "workflow = 'monthly-invoicing'",
		"steps": "run_id = '" + financeRun + "'", "tasks": "run_id = '" + financeRun + "'",
		"artifacts": "run_id = '" + financeRun + "'", "grants": "workflow = 'monthly-invoicing'",
		"task_logs": "task_id = '" + task + "'", "task_log_chunks": "task_id = '" + task + "'",
		"git_packs": "repository = '" + repository + "'",
	} {
		var namespaces []string
		if err := conn.QueryRow(ctx, `select coalesce(array_agg(distinct namespace), '{}') from `+table+` where `+where).Scan(&namespaces); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(namespaces, []string{"team-ops"}) {
			t.Errorf("the rows of %s are in %v", table, namespaces)
		}
	}
	// Counted where they are, and let go where they were: the tree's object once for the one
	// version naming it twice, and the artifact's once.
	for _, c := range []struct {
		namespace, digest string
		refs              int
		collectable       bool
	}{
		{"team-ops", tree, 1, false}, {"team-ops", artifact, 1, false},
		{"finance", tree, 0, true}, {"finance", artifact, 0, true},
	} {
		var refs int
		var collectable bool
		if err := conn.QueryRow(ctx, `select refs, collectable_at is not null from artifact_objects where namespace = $1 and digest = $2`,
			c.namespace, c.digest).Scan(&refs, &collectable); err != nil {
			t.Fatal(err)
		}
		if refs != c.refs || collectable != c.collectable {
			t.Errorf("%s in %s counts %d, collectable %v", c.digest[:14], c.namespace, refs, collectable)
		}
	}
	var key string
	if err := conn.QueryRow(ctx, `select object_key from task_log_chunks where task_id = $1`, task).Scan(&key); err != nil || key != "team-ops"+strings.TrimPrefix(logKey, "finance") {
		t.Errorf("the log's key is %s: %v", key, err)
	}
	var left []string
	if err := conn.QueryRow(ctx, `select array_agg(key order by key) from moved_objects where namespace = 'finance' and delete_after > now()`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	prefix := "finance/git/" + repository + "/pack-" + pack
	if !slices.Equal(left, []string{prefix + ".idx", prefix + ".pack", logKey}) {
		t.Errorf("the move leaves %v", left)
	}
	var runKept, artifactKept time.Duration
	if err := conn.QueryRow(ctx, `
		select extract(epoch from r.expires_at - r.finished_at)::bigint * interval '1 second', extract(epoch from a.expires_at - a.created_at)::bigint * interval '1 second'
		from runs r join artifacts a on a.namespace = r.namespace and a.run_id = r.id where r.id = $1`, financeRun).Scan(&runKept, &artifactKept); err != nil {
		t.Fatal(err)
	}
	if runKept != 30*24*time.Hour || artifactKept != 30*24*time.Hour {
		t.Errorf("the run is kept %s and the artifact %s, past the target's 30 days", runKept, artifactKept)
	}
	var moving, targets int
	if err := conn.QueryRow(ctx, `select (select count(*) from workflow_moves), (select count(*) from move_targets)`).Scan(&moving, &targets); err != nil || moving+targets != 0 {
		t.Errorf("the move is still asked: %d, %d, %v", moving, targets, err)
	}
	var recorded []string
	if err := conn.QueryRow(ctx, `select array_agg(namespace order by namespace) from audit_log where action = 'workflow.update' and actor = 'alice' and detail::jsonb->>'namespace' = 'team-ops'`).Scan(&recorded); err != nil || !slices.Equal(recorded, []string{"finance", "team-ops"}) {
		t.Errorf("the move is recorded in %v: %v", recorded, err)
	}
	// And a push to where it is now is not frozen.
	if err := pool.In(ctx, "team-ops", func(ctx context.Context, ns *NS) error { return ns.HoldRepository(ctx, "monthly-invoicing") }); err != nil {
		t.Errorf("the moved workflow is refused a push: %v", err)
	}

	// Carried out, a move is no longer asked, and carrying it out again changes nothing.
	if again, err := pool.CompleteMove(ctx, m, objects, time.Hour); err != nil || again.Done {
		t.Errorf("a move carried out twice answered %+v: %v", again, err)
	}

	// Moved back within the grace, it names again what the first move left under the source's
	// keys, which the copy found held there: kept, and what it leaves in the target goes instead.
	if err := pool.AskMove(ctx, "team-ops", "monthly-invoicing", "finance", "alice", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	moves, err = pool.Moves(ctx, 10)
	if err != nil || len(moves) != 1 {
		t.Fatalf("the move back reads %v: %v", moves, err)
	}
	back, err := pool.ObjectsOf(ctx, moves[0])
	if err != nil || !back.Ready {
		t.Fatalf("what the move back copies read %+v: %v", back, err)
	}
	if moved, err := pool.CompleteMove(ctx, moves[0], back, time.Hour); err != nil || !moved.Done {
		t.Fatalf("the move back was carried out as %+v: %v", moved, err)
	}
	var kept, leaving []string
	if err := conn.QueryRow(ctx, `select coalesce(array_agg(key order by key) filter (where namespace = 'finance'), '{}'), coalesce(array_agg(key order by key) filter (where namespace = 'team-ops'), '{}') from moved_objects`).Scan(&kept, &leaving); err != nil {
		t.Fatal(err)
	}
	there := "team-ops/git/" + repository + "/pack-" + pack
	if len(kept) != 0 || !slices.Equal(leaving, []string{there + ".idx", there + ".pack", "team-ops" + strings.TrimPrefix(logKey, "finance")}) {
		t.Errorf("moved back, the keys it names are to be deleted %v, and those it left %v", kept, leaving)
	}
	if err := conn.QueryRow(ctx, `select object_key from task_log_chunks where task_id = $1`, task).Scan(&key); err != nil || key != logKey {
		t.Errorf("moved back, the log's key is %s: %v", key, err)
	}
	// What it left goes once the grace has passed.
	exec(`update moved_objects set delete_after = now() - interval '1 second'`)
	due, err := pool.MovedObjectsDue(ctx, 10)
	if err != nil || len(due) != 3 {
		t.Fatalf("what the move left is due as %v: %v", due, err)
	}
	if err := pool.MovedObjectsGone(ctx, due); err != nil {
		t.Fatal(err)
	}
	if due, err := pool.MovedObjectsDue(ctx, 10); err != nil || len(due) != 0 {
		t.Errorf("what the move left is still due: %v, %v", due, err)
	}
}

// A move waits for a run let in before it was asked, which a move asked while none had finished
// would never have been, and for a pack being collected.
func TestAMoveWaitsForWhatIsGoing(t *testing.T) {
	pool, super := opened(t)
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `update runs set state = 'succeeded' where id = $1`, financeRun); err != nil {
		t.Fatal(err)
	}
	if err := pool.AskMove(ctx, "finance", "monthly-invoicing", "team-ops", "alice", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	moves, err := pool.Moves(ctx, 10)
	if err != nil || len(moves) != 1 {
		t.Fatalf("%v, %v", moves, err)
	}
	if _, err := conn.Exec(ctx, `update runs set state = 'running' where id = $1`, financeRun); err != nil {
		t.Fatal(err)
	}
	if objects, err := pool.ObjectsOf(ctx, moves[0]); err != nil || objects.Ready {
		t.Errorf("a move with a run going is ready: %+v, %v", objects, err)
	}
	if moved, err := pool.CompleteMove(ctx, moves[0], MoveObjects{}, time.Hour); err != nil || moved.Done {
		t.Errorf("a move with a run going was carried out: %+v, %v", moved, err)
	}
	var namespace string
	if err := conn.QueryRow(ctx, `select namespace from runs where id = $1`, financeRun).Scan(&namespace); err != nil || namespace != "finance" {
		t.Errorf("the run is in %s: %v", namespace, err)
	}
	if _, err := conn.Exec(ctx, `update runs set state = 'succeeded' where id = $1`, financeRun); err != nil {
		t.Fatal(err)
	}

	// Outputs a cache hit found as the move was asked republished since, by a run of another
	// workflow: waited out, as the move would have been refused for them.
	for _, stmt := range []string{
		`insert into workflows (namespace, name) values ('finance', 'payroll')`,
		`insert into workflow_versions (namespace, workflow, commit, graph, author, created_at) values ('finance', 'payroll', 'b4a0d2f', '{}', 'alice', now())`,
		`insert into runs (namespace, id, workflow, commit, trigger, state) values ('finance', '01M2T1BBBBBBBBBBBBBBBBBBBB', 'payroll', 'b4a0d2f', 'manual', 'succeeded')`,
		`insert into steps (namespace, run_id, step) values ('finance', '01M2T1BBBBBBBBBBBBBBBBBBBB', 'archive')`,
		`insert into tasks (namespace, id, run_id, step, attempt, state, memoised_from) values ('finance', '01M2T1CCCCCCCCCCCCCCCCCCCC', '01M2T1BBBBBBBBBBBBBBBBBBBB', 'archive', 1, 'succeeded', '` + financeRun + `')`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
	if objects, err := pool.ObjectsOf(ctx, moves[0]); err != nil || objects.Ready {
		t.Errorf("a move whose outputs another workflow's run republished is ready: %+v, %v", objects, err)
	}
	if _, err := conn.Exec(ctx, `update tasks set memoised_from = null`); err != nil {
		t.Fatal(err)
	}

	// A chunk a task still stopping shipped after the objects were read, which the copy could not
	// have seen: carried out the pass after, once it has been read and copied.
	objects, err := pool.ObjectsOf(ctx, moves[0])
	if err != nil || !objects.Ready {
		t.Fatalf("what the move copies read %+v: %v", objects, err)
	}
	const task = "01M2T1AAAAAAAAAAAAAAAAAAAA"
	late := "finance/logs/" + financeRun + "/" + task + "/1/1-" + digestOf("d")
	for _, stmt := range []string{
		`insert into task_logs (namespace, task_id, next_seq) values ('finance', '` + task + `', 2)`,
		`insert into task_log_chunks (namespace, task_id, seq, first_line, shipped, shipped_digest, lines, bytes, object_key, object_digest)
		 values ('finance', '` + task + `', 1, 1, 3, '` + digestOf("d") + `', 3, 30, '` + late + `', '` + digestOf("d") + `')`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
	if moved, err := pool.CompleteMove(ctx, moves[0], objects, time.Hour); err != nil || moved.Done {
		t.Errorf("a move with a chunk indexed since its objects were read was carried out: %+v, %v", moved, err)
	}
	if err := conn.QueryRow(ctx, `select namespace from runs where id = $1`, financeRun).Scan(&namespace); err != nil || namespace != "finance" {
		t.Errorf("the run is in %s: %v", namespace, err)
	}
	objects, err = pool.ObjectsOf(ctx, moves[0])
	if err != nil || !slices.Equal(objects.Indexed, []string{late}) {
		t.Fatalf("what the move copies read %+v: %v", objects, err)
	}
	if moved, err := pool.CompleteMove(ctx, moves[0], objects, time.Hour); err != nil || !moved.Done {
		t.Errorf("the move with its chunk read was carried out as %+v: %v", moved, err)
	}
}
