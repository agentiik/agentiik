package api_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/version"
)

// "secret:use is checked when a version is pushed, against whoever pushes it, and never when the
// workflow runs."

// namingASecret is the ordinary workflow with a secret named in its secrets block and mounted by a
// step, as the documentation's worked example names billing.
var namingASecret = strings.Replace(
	strings.Replace(workflowDocument, "steps:\n", "secrets: [billing]\nsteps:\n", 1),
	"    outputs: [ok, rejected]\n", "    outputs: [ok, rejected]\n    secrets: [billing]\n", 1)

// asked records every question an authorizer is asked, and answers as another does.
type asked struct {
	api.Authorizer
	mu        sync.Mutex
	questions []question
}

type question struct {
	what api.Permission
	over api.Target
}

func (a *asked) Allow(ctx context.Context, who api.Principal, what api.Permission, over api.Target) (bool, error) {
	a.mu.Lock()
	a.questions = append(a.questions, question{what, over})
	a.mu.Unlock()
	return a.Authorizer.Allow(ctx, who, what, over)
}

// servingTo is the ordinary server with an authorizer of the test's own and an object store the
// test can look into.
func servingTo(t *testing.T, auth api.Authorizer) (http.Handler, *db.Pool, artifact.Objects) {
	t.Helper()
	pool, super := dbtest.Open(t)
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(), `insert into namespaces (name) values ('finance')`); err != nil {
		t.Fatal(err)
	}
	store, err := version.New(pool, version.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := api.NewRouter(auth, bearer)
	if err != nil {
		t.Fatal(err)
	}
	objects := artifact.Dir(t.TempDir())
	if _, err := api.NewServer(rt, api.ServerOptions{Pool: pool, Versions: store, Objects: objects}); err != nil {
		t.Fatal(err)
	}
	return rt, pool, objects
}

func TestAVersionNamingASecretIsPushedOnlyBySomeoneHoldingSecretUse(t *testing.T) {
	finance := api.Target{Namespace: "finance"}
	invoicing := api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}
	auth := &asked{Authorizer: denying{
		allowed: granted{
			// alice writes the workflow and holds nothing of the namespace's secrets.
			"alice": {{api.WorkflowWrite, finance}},
			// bob writes it and holds secret:use in the namespace.
			"bob": {{api.WorkflowWrite, finance}, {api.SecretUse, finance}},
			// carol holds both on the namespace and is denied secret:use on this workflow.
			"carol": {{api.WorkflowWrite, finance}, {api.SecretUse, finance}},
		},
		denied: granted{"carol": {{api.SecretUse, invoicing}}},
	}}
	h, pool, objects := servingTo(t, auth)

	stored := func() bool {
		t.Helper()
		held, err := objects.Has(t.Context(), keyOf("finance", []byte(namingASecret)))
		if err != nil {
			t.Fatal(err)
		}
		var versions int
		if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
			_, err := ns.Version(ctx, "monthly-invoicing", aCommit)
			if err == nil {
				versions++
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return held || versions > 0
	}

	for _, who := range []string{"alice", "carol"} {
		auth.questions = nil
		w, answer := call(t, h, "PUT", pushTo, who, aPushOf(t, namingASecret))
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s, without secret:use on monthly-invoicing, pushed a version naming a secret and was answered %d: %s", who, w.Code, w.Body)
		}
		said, _ := answer["error"].(string)
		for _, want := range []string{"billing", "secret:use", "finance", "workflow:run alone"} {
			if !strings.Contains(said, want) {
				t.Errorf("the refusal does not say %q: %s", want, said)
			}
		}
		if stored() {
			t.Errorf("a push refused to %s stored its tree or its version", who)
		}
		// Asked over the workflow, so that a deny on it counts, and about nothing more.
		want := []question{{api.WorkflowWrite, invoicing}, {api.SecretUse, invoicing}}
		if len(auth.questions) != 2 || auth.questions[0] != want[0] || auth.questions[1] != want[1] {
			t.Errorf("pushing as %s asked %v", who, auth.questions)
		}
	}

	// A version naming no secret asks nothing about secrets, and alice pushes it.
	auth.questions = nil
	if w, _ := call(t, h, "PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/"+anotherCommit, "alice", aPush(t)); w.Code != http.StatusOK {
		t.Fatalf("a version naming no secret, pushed without secret:use, answered %d: %s", w.Code, w.Body)
	}
	for _, q := range auth.questions {
		if q.what == api.SecretUse {
			t.Errorf("a version naming no secret asked about secret:use over %v", q.over)
		}
	}

	// bob holds secret:use in the namespace, and his push of the same version is accepted.
	if w, _ := call(t, h, "PUT", pushTo, "bob", aPushOf(t, namingASecret)); w.Code != http.StatusOK {
		t.Fatalf("bob, holding secret:use in finance, pushed a version naming a secret and was answered %d: %s", w.Code, w.Body)
	}
	if !stored() {
		t.Error("bob's push stored nothing")
	}
}

// An authorizer that cannot answer about secret:use leaves the push unanswered rather than refused
// or accepted, and nothing is stored.
func TestAPushWhoseSecretUseCannotBeAskedIsNotAnswered(t *testing.T) {
	h, _, objects := servingTo(t, failingOn{Authorizer: everything{who: "alice"}, what: api.SecretUse})
	w, answer := call(t, h, "PUT", pushTo, "alice", aPushOf(t, namingASecret))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("a push whose secret:use could not be asked answered %d: %s", w.Code, w.Body)
	}
	if said, _ := answer["error"].(string); !strings.Contains(said, "could not be authorised") {
		t.Errorf("the refusal reads %q", said)
	}
	if held, err := objects.Has(t.Context(), keyOf("finance", []byte(namingASecret))); held || err != nil {
		t.Errorf("the tree was stored: %v %v", held, err)
	}
}
