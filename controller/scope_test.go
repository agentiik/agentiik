package controller

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/jackc/pgx/v5"
)

// "the controller names which secret a task may have and never sees its value." What it names is
// written into the grant's scope, and the redemption answers from that scope and from nothing
// else, so a mount left out here is a value written where the brick is not looking for it.
func TestTheScopeNamesEachSecretWithItsMount(t *testing.T) {
	task := graph.Task{
		Run: decidedRun, Step: "invoice", Workflow: "monthly-invoicing", Commit: "a3f9c1e",
		Secrets: []graph.SecretMount{
			// Where the brick manifest asked for it, which is not where its name
			// would put it.
			{Name: "billing", Mount: "/agk/secrets/api-key"},
			// And a secret the manifest declares no mount for, at the path the
			// contract names the directory by.
			{Name: "stripe", Mount: "/agk/secrets/stripe"},
		},
	}
	scope := scopeOf(task, nil)

	want := []db.GrantSecret{
		{Name: "billing", Mount: "/agk/secrets/api-key"},
		{Name: "stripe", Mount: "/agk/secrets/stripe"},
	}
	if !slices.Equal(scope.Secrets, want) {
		t.Fatalf("the scope names the secrets %+v, want %+v", scope.Secrets, want)
	}

	// And nothing else: the scope is written as a document, and a secret in it is a name and a
	// path with no key a value could be written under.
	written, err := json.Marshal(scope)
	if err != nil {
		t.Fatal(err)
	}
	var read struct {
		Secrets []map[string]any `json:"secrets"`
	}
	if err := json.Unmarshal(written, &read); err != nil {
		t.Fatal(err)
	}
	for _, s := range read.Secrets {
		for key := range s {
			if key != "name" && key != "mount" {
				t.Errorf("a secret in the scope is written with %q: %s", key, written)
			}
		}
	}
}

// namingWorkflow names two secrets and mounts one of them in its first step, so that what a task
// may have is told apart from what its workflow names.
const namingWorkflow = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
secrets: [billing, stripe]
inputs:
  orders: { schema: { type: array } }
outputs:
  invoices: { from: { step: archive, port: ok } }
steps:
  normalize:
    image: ` + theImage + `
    secrets: [billing]
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok, rejected]
  archive:
    image: ` + theImage + `
    needs:
      - { step: normalize, port: ok, as: orders }
    outputs: [ok]
`

// The same, end to end: a run decided, dispatched and redeemed. The grant of each task names the
// secrets its own step mounts, each with its mount, and nothing else: not the other secret of the
// workflow, and nothing for a step that mounts none. It names what the task message names, which
// is where the runner writes each value the redemption answers, and it is written with no key a
// value could be kept under.
//
// And it is decided with the namespace's declarations and the built-in store's values out of the
// controller's reach. Where a value lives and what it is are the API's to read at redemption, and
// a controller that looked either up, to write a provider into the grant or to check a value is
// there, would be reading what the documentation says it never sees.
func TestTheControllerNamesTheSecretsATaskMayHave(t *testing.T) {
	core, q, pool, super := decidingOn(t, namingWorkflow)
	createRun(t, pool)

	// The role the pool connects as is named after the test's database.
	conn := dbtest.Superuser(t, super)
	var role string
	if err := conn.QueryRow(t.Context(), `select current_database()`).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(),
		`revoke all on secret_declarations, secret_values from `+pgx.Identifier{role}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	// And that is a refusal, rather than a namespace with nothing declared, or this test would
	// pass whatever the controller read.
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		_, err := ns.Declaration(ctx, "billing")
		return err
	}); err == nil || errors.Is(err, db.ErrNoDeclaration) {
		t.Fatalf("the declarations still answer the controller's role: %v", err)
	}

	// named is what the grant of one dispatch names, read back the way the API reads it and
	// held to what the message names.
	named := func(d Dispatch) []db.GrantSecret {
		t.Helper()
		var got db.Redeemed
		if err := core.controller.Fenced(t.Context(), core.term, func(ctx context.Context, w *db.Wide) error {
			var err error
			got, err = w.Redeem(ctx, d.Grant, d.Task.ID, theRunner, core.now())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var message []db.GrantSecret
		for _, m := range d.Task.Secrets {
			message = append(message, db.GrantSecret{Name: m.Name, Mount: m.Mount})
		}
		if !slices.Equal(got.Scope.Secrets, message) {
			t.Errorf("the grant of %s names the secrets %+v, and its message names %+v", d.Task.Step, got.Scope.Secrets, message)
		}

		// As the table holds it, from behind the policies: a secret is a name and a mount.
		var written string
		if err := conn.QueryRow(t.Context(),
			`select scope::text from task_grants where task_id = $1`, d.Row).Scan(&written); err != nil {
			t.Fatal(err)
		}
		var scope map[string]json.RawMessage
		if err := json.Unmarshal([]byte(written), &scope); err != nil {
			t.Fatal(err)
		}
		for key := range scope {
			if !slices.Contains([]string{"run", "step", "workflow", "commit", "inputs", "secrets"}, key) {
				t.Errorf("the grant of %s is written with %q: %s", d.Task.Step, key, written)
			}
		}
		var secrets []map[string]any
		if raw, ok := scope["secrets"]; ok {
			if err := json.Unmarshal(raw, &secrets); err != nil {
				t.Fatal(err)
			}
		}
		for _, s := range secrets {
			if len(s) != 2 || s["name"] == nil || s["mount"] == nil {
				t.Errorf("a secret in the grant of %s is written as %v", d.Task.Step, s)
			}
		}
		return got.Scope.Secrets
	}

	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	first := q.dispatched()
	if len(first) != 1 || first[0].Task.Step != "normalize" {
		t.Fatalf("the first pass dispatched %+v", first)
	}
	if got, want := named(first[0]), []db.GrantSecret{{Name: "billing", Mount: "/agk/secrets/billing"}}; !slices.Equal(got, want) {
		t.Errorf("normalize may have the secrets %+v, want %+v", got, want)
	}

	core.answer(t, succeeded(t, first[0].Task, core.now()))
	second := q.dispatched()
	if len(second) != 1 || second[0].Task.Step != "archive" {
		t.Fatalf("the second pass dispatched %+v", second)
	}
	if got := named(second[0]); len(got) != 0 {
		t.Errorf("archive mounts no secret and may have %+v", got)
	}
}
