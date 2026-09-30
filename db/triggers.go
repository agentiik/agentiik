package db

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/audit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Arming: what the default branch's head of a workflow declares under on, armed one row per entry.
//
// "Schedules, webhooks and events" run the default branch, so a version's triggers are armed when
// its commit lands there and disarmed when another lands there that does not declare them, in the
// transaction that moves the branch.

// Entry is one entry of a version's on block, as it is armed.
type Entry struct {
	// Kind is schedule, webhook or event, and Position the entry's place in its list under on.
	Kind     agk.TriggerKind
	Position int

	// Declared is the entry with the values in force, as JSON: what a listing answers and what
	// tells a push declaring it again from one that changed it.
	Declared json.RawMessage

	// Path and Method are a webhook's, which one trigger of the namespace answers.
	Path, Method string

	// Type and Source are an event's, where the event consumer looks for a subscription.
	Type, Source string

	// DueAt and FireAt are a schedule's next occurrence and when it fires, its jitter drawn: what
	// it is armed with where the push arms it afresh. An entry declared again keeps the row's.
	DueAt, FireAt time.Time
}

// Armed is one trigger armed, and what has become of it.
type Armed struct {
	Entry
	Workflow string
	Commit   string

	ArmedBy string
	ArmedAt time.Time

	// FiredAt is when it last fired, FiredFor the occurrence it fired for, a schedule's, and
	// FiredRun the run it started; Skipped says why it started none, where the namespace's
	// max_runs_per_hour was spent.
	FiredAt  time.Time
	FiredFor time.Time
	FiredRun agk.RunID
	Skipped  string

	// Failures counts a webhook's signature failures, the last at FailedAt.
	Failures int64
	FailedAt time.Time
}

// HookTaken is a push arming a webhook whose path and method another workflow of the namespace has
// armed: "within a namespace a path and a method answer one trigger".
type HookTaken struct {
	Namespace, Path, Method string
	// Holder is the workflow that has armed it.
	Holder string
}

func (e *HookTaken) Error() string {
	holder := "another workflow's webhook"
	if e.Holder != "" {
		holder = "the webhook of " + e.Holder
	}
	return fmt.Sprintf("%s %s is answered by %s, armed in %s, and within a namespace a path and a method answer one trigger: give this webhook another path, or disarm that one first", e.Method, e.Path, holder, e.Namespace)
}

// HookHolder is the workflow of this namespace, other than workflow, that has armed a webhook
// answering path with method, or empty where none has. It is what a push moving the default branch
// is judged against, before any ref moves.
func (n *NS) HookHolder(ctx context.Context, workflow, path, method string) (string, error) {
	var holder string
	err := n.tx.QueryRow(ctx,
		`select workflow from triggers
		 where namespace = $1 and kind = 'webhook' and path = $2 and method = $3 and workflow <> $4`,
		n.namespace, path, method, workflow).Scan(&holder)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("db: the webhooks of %s could not be read: %w", n.namespace, err)
	}
	return holder, nil
}

// ArmedCommit is the commit whose triggers a workflow has armed, empty where none has been.
func (n *NS) ArmedCommit(ctx context.Context, workflow string) (string, error) {
	var armed *string
	err := n.tx.QueryRow(ctx,
		`select armed from workflows where namespace = $1 and name = $2`, n.namespace, workflow).Scan(&armed)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: %s/%s", ErrNoWorkflow, n.namespace, workflow)
	}
	if err != nil {
		return "", fmt.Errorf("db: what %s has armed could not be read: %w", workflow, err)
	}
	return deref(armed), nil
}

// Arm replaces what a workflow has armed by entries, the triggers commit declares, and records it
// as armed, by who at at. An empty commit disarms everything: a default branch with no version to
// run, or a version that is a library.
//
// An entry declared again, the same kind with the same Declared, keeps its row's state: a schedule
// its next occurrence and its last firing, so that a push at 05:59 neither skips nor doubles the run
// due at 06:00; and a webhook answering the same path with the same method keeps the failures
// counted against it, whatever else about it changed. Each entry newly armed is recorded as
// trigger.arm and each no longer armed as trigger.disarm, with the workflow as the target; one
// declared again is neither, since a push that leaves the triggers as they were would otherwise
// record every one of them twice and bury the changes the log is read for.
//
// A webhook whose path and method another workflow of the namespace has armed is a *HookTaken, and
// nothing is written.
func (n *NS) Arm(ctx context.Context, workflow, commit string, entries []Entry, by string, at time.Time) error {
	old, err := n.armed(ctx, workflow, true)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Kind != agk.TriggerWebhook {
			continue
		}
		holder, err := n.HookHolder(ctx, workflow, e.Path, e.Method)
		if err != nil {
			return err
		}
		if holder != "" {
			return &HookTaken{Namespace: n.namespace, Path: e.Path, Method: e.Method, Holder: holder}
		}
	}
	if _, err := n.tx.Exec(ctx,
		`delete from triggers where namespace = $1 and workflow = $2`, n.namespace, workflow); err != nil {
		return fmt.Errorf("db: the triggers of %s could not be disarmed: %w", workflow, err)
	}

	kept := make([]bool, len(old))
	for _, e := range entries {
		row := Armed{Entry: e, Workflow: workflow, Commit: commit, ArmedBy: by, ArmedAt: at}
		again := -1
		for i, o := range old {
			if !kept[i] && o.Kind == e.Kind && bytes.Equal(o.Declared, e.Declared) {
				again = i
				break
			}
		}
		if again >= 0 {
			kept[again] = true
			o := old[again]
			row.ArmedBy, row.ArmedAt = o.ArmedBy, o.ArmedAt
			row.DueAt, row.FireAt = o.DueAt, o.FireAt
			row.FiredAt, row.FiredFor, row.FiredRun, row.Skipped = o.FiredAt, o.FiredFor, o.FiredRun, o.Skipped
		}
		if e.Kind == agk.TriggerWebhook {
			for _, o := range old {
				if o.Kind == agk.TriggerWebhook && o.Path == e.Path && o.Method == e.Method {
					row.Failures, row.FailedAt = o.Failures, o.FailedAt
				}
			}
		}
		if err := n.insertArmed(ctx, row); err != nil {
			return err
		}
		if again < 0 {
			if err := n.Audit(ctx, audit.Record{
				Actor: by, Action: audit.TriggerArm, Target: workflow, Result: audit.Done,
				Detail: armedDetail(e, commit),
			}); err != nil {
				return err
			}
		}
	}
	for i, o := range old {
		if kept[i] {
			continue
		}
		if err := n.Audit(ctx, audit.Record{
			Actor: by, Action: audit.TriggerDisarm, Target: workflow, Result: audit.Done,
			Detail: armedDetail(o.Entry, o.Commit),
		}); err != nil {
			return err
		}
	}

	var armed any
	if commit != "" {
		armed = commit
	}
	if _, err := n.tx.Exec(ctx,
		`update workflows set armed = $3 where namespace = $1 and name = $2`, n.namespace, workflow, armed); err != nil {
		return fmt.Errorf("db: what %s has armed could not be recorded: %w", workflow, err)
	}
	return nil
}

// armedDetail is what trigger.arm and trigger.disarm record of an entry: its kind, its place, the
// version that declares it and what it declares.
func armedDetail(e Entry, commit string) map[string]any {
	var declared any
	_ = json.Unmarshal(e.Declared, &declared)
	return map[string]any{"kind": e.Kind.String(), "position": e.Position, "commit": commit, "declared": declared}
}

func (n *NS) insertArmed(ctx context.Context, a Armed) error {
	_, err := n.tx.Exec(ctx,
		`insert into triggers (namespace, workflow, kind, position, commit, declared, path, method, type, source,
		   due_at, fire_at, fired_at, fired_for, fired_run, skipped, failures, failed_at, armed_by, armed_at)
		 values ($1, $2, $3, $4, $5, $6, nullif($7, ''), nullif($8, ''), nullif($9, ''), nullif($10, ''),
		   $11, $12, $13, $14, nullif($15, ''), nullif($16, ''), $17, $18, $19, $20)`,
		n.namespace, a.Workflow, a.Kind.String(), a.Position, a.Commit, a.Declared, a.Path, a.Method, a.Type, a.Source,
		orNil(a.DueAt), orNil(a.FireAt), orNil(a.FiredAt), orNil(a.FiredFor), string(a.FiredRun), a.Skipped,
		a.Failures, orNil(a.FailedAt), a.ArmedBy, a.ArmedAt)
	// Found here only where another push armed the pair while this one was judged, since Arm asks
	// first: the transaction is aborted by then, and the holder is not asked for.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "triggers_hook" {
		return &HookTaken{Namespace: n.namespace, Path: a.Path, Method: a.Method}
	}
	if err != nil {
		return fmt.Errorf("db: the %s %d of %s could not be armed: %w", a.Kind, a.Position, a.Workflow, err)
	}
	return nil
}

// orNil is a time a row leaves null where it is zero.
func orNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// ArmedBy is what a workflow has armed, by kind and then by position.
func (n *NS) ArmedBy(ctx context.Context, workflow string) ([]Armed, error) {
	return n.armed(ctx, workflow, false)
}

func (n *NS) armed(ctx context.Context, workflow string, lock bool) ([]Armed, error) {
	sql := `select kind, position, commit, declared, coalesce(path, ''), coalesce(method, ''), coalesce(type, ''),
	   coalesce(source, ''), due_at, fire_at, fired_at, fired_for, coalesce(fired_run, ''), coalesce(skipped, ''),
	   failures, failed_at, armed_by, armed_at
	 from triggers where namespace = $1 and workflow = $2
	 order by case kind when 'schedule' then 0 when 'webhook' then 1 else 2 end, position`
	if lock {
		sql += ` for update`
	}
	rows, err := n.tx.Query(ctx, sql, n.namespace, workflow)
	if err != nil {
		return nil, fmt.Errorf("db: the triggers of %s could not be read: %w", workflow, err)
	}
	defer rows.Close()
	var out []Armed
	for rows.Next() {
		a := Armed{Workflow: workflow}
		var kind, run string
		var due, fire, firedAt, firedFor, failedAt *time.Time
		if err := rows.Scan(&kind, &a.Position, &a.Commit, &a.Declared, &a.Path, &a.Method, &a.Type, &a.Source,
			&due, &fire, &firedAt, &firedFor, &run, &a.Skipped, &a.Failures, &failedAt, &a.ArmedBy, &a.ArmedAt); err != nil {
			return nil, fmt.Errorf("db: the triggers of %s could not be read: %w", workflow, err)
		}
		if err := a.Kind.UnmarshalText([]byte(kind)); err != nil {
			return nil, fmt.Errorf("db: a trigger of %s: %w", workflow, err)
		}
		// jsonb keeps its keys in an order of its own, and an entry declared again is told from
		// one that changed by its bytes: read back as json.Marshal writes it, keys sorted.
		if a.Declared, err = canonical(a.Declared); err != nil {
			return nil, fmt.Errorf("db: a trigger of %s: %w", workflow, err)
		}
		a.FiredRun = agk.RunID(run)
		a.DueAt, a.FireAt, a.FiredAt, a.FiredFor, a.FailedAt = at(due), at(fire), at(firedAt), at(firedFor), at(failedAt)
		out = append(out, a)
	}
	return out, rows.Err()
}

// canonical is a JSON document written as json.Marshal writes it: compact, its keys sorted.
func canonical(doc json.RawMessage) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// at is a nullable time, zero where the row holds none.
func at(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// Unarmed is a workflow whose default branch's head is not what it has armed: its commit, and the
// head, empty while the branch is unborn.
type Unarmed struct {
	Namespace, Workflow string
	Armed, Head         string
}

// Unarmed lists the workflows whose default branch's head is not what they have armed, across
// every namespace: those pushed before v0.5.0, which arms nothing at the upgrade, and any whose
// arming failed. The leading controller arms each at the start of its term. A workflow holding no
// git ref is left out, since its head is whatever version a tree push recorded last and the tree
// push arms it itself.
func (w *Wide) Unarmed(ctx context.Context) ([]Unarmed, error) {
	rows, err := w.tx.Query(ctx,
		`select w.namespace, w.name, coalesce(w.armed, ''), coalesce(r.commit, '')
		 from workflows w
		 left join workflow_refs r on r.namespace = w.namespace and r.workflow = w.name
		   and r.ref = 'refs/heads/' || w.default_branch
		 where w.deleted_at is null
		   and exists (select 1 from workflow_refs g where g.namespace = w.namespace and g.workflow = w.name and g.commit is not null)
		   and coalesce(w.armed, '') is distinct from coalesce(r.commit, '')
		 order by w.namespace, w.name`)
	if err != nil {
		return nil, fmt.Errorf("db: the workflows to arm could not be read: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Unarmed, error) {
		var u Unarmed
		err := row.Scan(&u.Namespace, &u.Workflow, &u.Armed, &u.Head)
		return u, err
	})
}
