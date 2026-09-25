package db

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// A reader held on the first shard of a wide fan-out reads a batch of its dispatches from where it
// stands, in order, out of the index and nothing else: were the index in the database's collation,
// every row of the step would be read and sorted each time the reader is woken.
func TestAStepsDispatchesAreReadFromTheIndexInOrder(t *testing.T) {
	super, _ := database(t)
	seed(t, super)
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// Told to prefer an index where one serves, since a table of a few rows is read whole
	// whatever indexes it has.
	if _, err := tx.Exec(ctx, `set local enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, `explain
		select t.id from tasks t
		where t.namespace = 'finance' and t.run_id = '`+financeRun+`' and t.step = 'render'
		  and t.id collate "C" >= '01M2' collate "C"
		order by t.id collate "C"
		limit 64`)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	whole := strings.Join(plan, "\n")
	if !strings.Contains(whole, "tasks_by_step") || strings.Contains(whole, "Sort") || strings.Contains(whole, "Filter") {
		t.Errorf("a step's dispatches are not read from the index in order:\n%s", whole)
	}
}
