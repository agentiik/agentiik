package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// A namespace's variables.
//
// "A namespace keeps variables of its own beside those a workflow file writes", read through the
// vars context by the workflows each is shown to, a name the file writes taking the file's value.
// What is kept here is each variable as the API wrote it; which of them a run reads is asked by the
// one path every run is created by, in the transaction that creates it, and frozen on the run in
// runs.namespace_vars, so that a controller taking it over and a replay read what it read.
//
// Not a secret, and nothing here treats one as one: a value is kept in clear and answered to
// whoever may read the namespace's workflows.

// The visibilities a variable has, as namespace_variables.visibility and the routes write them.
const (
	// VisibilityAll is a variable every workflow of the namespace reads.
	VisibilityAll = "all"

	// VisibilitySelected is a variable the workflows it names read, and no other.
	VisibilitySelected = "selected"
)

// The bounds a namespace's variables are held to, together.
//
// A run keeps the variables it reads, and the controller reads them at every decision it takes on
// the run, as it reads the run's inputs: so the namespace's variables are held in all to the two
// bounds a run's inputs are held to, one envelope's weight and as many values as one envelope may
// carry items, and whatever a run reads of them costs the controller no more than its inputs may.
// The count bounds a listing, which answers them whole, as the console shows them on one page.
const (
	// VariablesMax is how many variables a namespace may hold.
	VariablesMax = 1000

	// VariablesMaxBytes is what a namespace's values may weigh in all, each written as compact
	// JSON.
	VariablesMaxBytes = int(agk.DefaultEnvelopeMaxBytes)

	// VariablesMaxValues is how many values they may hold in all, each value and everything in
	// it counted, as a run's inputs are counted.
	VariablesMaxValues = agk.DefaultMaxItems
)

// ErrNoVariable is a variable the namespace does not hold.
var ErrNoVariable = errors.New("db: the namespace holds no variable of that name")

// VariablesFull is a variable refused because the namespace's variables would be past what they may
// hold together. Nothing is written before it is answered.
type VariablesFull struct{ Why string }

func (e *VariablesFull) Error() string { return e.Why }

// Variable is one variable of one namespace.
type Variable struct {
	Name string

	// Value is the value as JSON, as the API wrote it: every number as it was written but for an
	// exponent, written out with a point. Read back, it is what jsonb writes, the same value.
	Value json.RawMessage

	// Bytes and Values are what the namespace's bounds are counted from: the bytes the value takes
	// written as compact JSON, and how many values it holds, itself and everything in it.
	Bytes, Values int

	// Visibility is VisibilityAll or VisibilitySelected, and Workflows the workflows a selected
	// one names, sorted and each once, empty for none; nil for all.
	Visibility string
	Workflows  []string

	UpdatedBy string
	UpdatedAt time.Time
}

// Variables are every variable of the namespace, by name.
func (n *NS) Variables(ctx context.Context) ([]Variable, error) {
	rows, err := n.tx.Query(ctx,
		`select name, value, value_bytes, value_count, visibility, workflows, updated_by, updated_at
		 from namespace_variables where namespace = $1 order by name`, n.namespace)
	if err != nil {
		return nil, fmt.Errorf("db: the variables of %s could not be read: %w", n.namespace, err)
	}
	out, err := pgx.CollectRows(rows, scanVariable)
	if err != nil {
		return nil, fmt.Errorf("db: the variables of %s could not be read: %w", n.namespace, err)
	}
	return out, nil
}

// Variable is one variable of the namespace, or ErrNoVariable.
func (n *NS) Variable(ctx context.Context, name string) (Variable, error) {
	rows, err := n.tx.Query(ctx,
		`select name, value, value_bytes, value_count, visibility, workflows, updated_by, updated_at
		 from namespace_variables where namespace = $1 and name = $2`, n.namespace, name)
	if err != nil {
		return Variable{}, fmt.Errorf("db: variable %s could not be read: %w", name, err)
	}
	v, err := pgx.CollectExactlyOneRow(rows, scanVariable)
	if errors.Is(err, pgx.ErrNoRows) {
		return Variable{}, ErrNoVariable
	}
	if err != nil {
		return Variable{}, fmt.Errorf("db: variable %s could not be read: %w", name, err)
	}
	return v, nil
}

func scanVariable(row pgx.CollectableRow) (Variable, error) {
	var v Variable
	var value []byte
	err := row.Scan(&v.Name, &value, &v.Bytes, &v.Values, &v.Visibility, &v.Workflows, &v.UpdatedBy, &v.UpdatedAt)
	v.Value = json.RawMessage(value)
	v.UpdatedAt = v.UpdatedAt.UTC()
	if v.Visibility == VisibilitySelected && v.Workflows == nil {
		v.Workflows = []string{}
	}
	return v, err
}

// SetVariable writes one variable whole, replacing the one of that name if there is one, and
// answers it as it was stored and whether it is new. A variable the namespace's bounds leave no room
// for is a *VariablesFull, and a namespace nobody created ErrNoNamespace.
//
// One variable at a time and never the set, so that two writers each setting their own cannot undo
// each other. The namespace's row is locked before its variables are counted, so that two writers in
// one namespace count one after the other, the second counting the first's variable once it has
// committed: each would otherwise count what was there before both, and the two together could pass
// a bound neither passed alone. FOR NO KEY UPDATE, which leaves alone the key share a row naming the
// namespace takes, as withinRunsPerHour locks it.
//
// The time answered is the one the row holds, in UTC, as Declare answers it and for its reason.
func (n *NS) SetVariable(ctx context.Context, v Variable) (Variable, bool, error) {
	switch {
	case v.Name == "":
		return Variable{}, false, errors.New("db: a variable names itself")
	case len(v.Value) == 0 || v.Bytes < 1 || v.Values < 1:
		return Variable{}, false, fmt.Errorf("db: variable %s has no value", v.Name)
	case v.UpdatedBy == "":
		return Variable{}, false, fmt.Errorf("db: variable %s is written by nobody", v.Name)
	case v.Visibility == VisibilityAll && v.Workflows != nil:
		return Variable{}, false, fmt.Errorf("db: variable %s is read by every workflow and names some", v.Name)
	case v.Visibility == VisibilitySelected:
		if v.Workflows == nil {
			v.Workflows = []string{}
		}
		// Sorted into a list of its own, never nil: an empty one is selected for none, which the
		// row tells from all by holding a list.
		v.Workflows = append([]string{}, slices.Compact(slices.Sorted(slices.Values(v.Workflows)))...)
	case v.Visibility != VisibilityAll:
		return Variable{}, false, fmt.Errorf("db: variable %s is shown to %q", v.Name, v.Visibility)
	}

	var found string
	err := n.tx.QueryRow(ctx, `select name from namespaces where name = $1 for no key update`, n.namespace).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return Variable{}, false, ErrNoNamespace
	}
	if err != nil {
		return Variable{}, false, fmt.Errorf("db: namespace %s could not be read: %w", n.namespace, err)
	}
	var others, bytes, values int
	var held bool
	if err := n.tx.QueryRow(ctx,
		`select count(*) filter (where name <> $2), coalesce(sum(value_bytes) filter (where name <> $2), 0),
		        coalesce(sum(value_count) filter (where name <> $2), 0), count(*) filter (where name = $2) > 0
		 from namespace_variables where namespace = $1`, n.namespace, v.Name).Scan(&others, &bytes, &values, &held); err != nil {
		return Variable{}, false, fmt.Errorf("db: the variables of %s could not be counted: %w", n.namespace, err)
	}
	switch {
	case !held && others >= VariablesMax:
		return Variable{}, false, &VariablesFull{Why: fmt.Sprintf("namespace %s holds %d variables, the most a namespace may: a listing answers them whole, and every run reads those shown to it, so remove one before setting another", n.namespace, others)}
	case bytes+v.Bytes > VariablesMaxBytes:
		return Variable{}, false, &VariablesFull{Why: fmt.Sprintf("the variables of %s would weigh %d bytes with this one, and a namespace's variables weigh at most %d in all, what a run's inputs may: a run keeps those it reads, and the controller reads them at every decision", n.namespace, bytes+v.Bytes, VariablesMaxBytes)}
	case values+v.Values > VariablesMaxValues:
		return Variable{}, false, &VariablesFull{Why: fmt.Sprintf("the variables of %s would hold %d values with this one, and a namespace's variables hold at most %d in all, as many as a run's inputs may: a run keeps those it reads, and the controller reads them at every decision", n.namespace, values+v.Values, VariablesMaxValues)}
	}

	at := v.UpdatedAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	var created bool
	err = n.tx.QueryRow(ctx,
		`insert into namespace_variables (namespace, name, value, value_bytes, value_count, visibility, workflows, updated_by, updated_at)
		 values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 on conflict (namespace, name) do update
		   set value = excluded.value, value_bytes = excluded.value_bytes, value_count = excluded.value_count,
		       visibility = excluded.visibility, workflows = excluded.workflows,
		       updated_by = excluded.updated_by, updated_at = excluded.updated_at
		 returning xmax = 0, value, updated_at`,
		n.namespace, v.Name, string(v.Value), v.Bytes, v.Values, v.Visibility, v.Workflows, v.UpdatedBy, at).
		Scan(&created, (*[]byte)(&v.Value), &v.UpdatedAt)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == foreignKeyViolation {
		return Variable{}, false, ErrNoNamespace
	}
	if err != nil {
		return Variable{}, false, fmt.Errorf("db: variable %s could not be written: %w", v.Name, err)
	}
	v.UpdatedAt = v.UpdatedAt.UTC()
	return v, created, nil
}

// RemoveVariable removes one variable, or answers ErrNoVariable where there was none.
func (n *NS) RemoveVariable(ctx context.Context, name string) error {
	tag, err := n.tx.Exec(ctx,
		`delete from namespace_variables where namespace = $1 and name = $2`, n.namespace, name)
	if err != nil {
		return fmt.Errorf("db: variable %s could not be removed: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoVariable
	}
	return nil
}

// VariablesFor are the variables the namespace shows a workflow, by name, each value as it was
// written, every number a json.Number: those every workflow reads, and those that name it. Never
// nil, so that a caller handing them on is told apart from one that read none.
//
// By the workflow's name, as a selected variable names it, whether or not the namespace holds a
// workflow of that name: a name no workflow holds yet is read by the workflow created under it.
func (n *NS) VariablesFor(ctx context.Context, workflow string) (map[string]any, error) {
	rows, err := n.tx.Query(ctx,
		`select name, value from namespace_variables
		 where namespace = $1 and (visibility = 'all' or $2 = any (workflows))`, n.namespace, workflow)
	if err != nil {
		return nil, fmt.Errorf("db: the variables %s shows %s could not be read: %w", n.namespace, workflow, err)
	}
	defer rows.Close()
	out := map[string]any{}
	for rows.Next() {
		var name string
		var raw []byte
		if err := rows.Scan(&name, &raw); err != nil {
			return nil, fmt.Errorf("db: the variables %s shows %s could not be read: %w", n.namespace, workflow, err)
		}
		var value any
		if err := asWritten(raw, &value); err != nil {
			return nil, fmt.Errorf("db: variable %s of %s could not be read: %w", name, n.namespace, err)
		}
		out[name] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: the variables %s shows %s could not be read: %w", n.namespace, workflow, err)
	}
	return out, nil
}

// NamespaceVarsOf are the namespace's variables a run read when it was created, which a replay of
// it is written with, so that it reads what the run read rather than what is true now. Never nil:
// a run that read none, or one from before v0.6.0, answers an empty map, which a replay reads as
// none, as the run did, rather than as variables to read now.
func (n *NS) NamespaceVarsOf(ctx context.Context, run agk.RunID) (map[string]any, error) {
	var raw []byte
	err := n.tx.QueryRow(ctx,
		`select namespace_vars from runs where namespace = $1 and id = $2`, n.namespace, string(run)).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNoRun, run)
	}
	if err != nil {
		return nil, fmt.Errorf("db: the variables run %s read could not be read: %w", run, err)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if err := asWritten(raw, &out); err != nil {
			return nil, fmt.Errorf("db: the variables run %s read could not be read: %w", run, err)
		}
	}
	return out, nil
}
