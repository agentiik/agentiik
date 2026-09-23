package controller

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
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
