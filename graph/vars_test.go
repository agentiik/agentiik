package graph

import (
	"fmt"
	"testing"
)

// "A name the workflow's own vars writes ... takes the file's value", whatever the namespace sets
// under it, and a namespace fills in what the file leaves unwritten; neither is written into.
func TestTheFilesVarsAreLaidOverTheNamespaces(t *testing.T) {
	file := Vars{"currency": "EUR", "dunning_days": 30}
	namespace := map[string]any{"currency": "CHF", "ledger_url": "https://ledger.example.com/api"}
	got := file.Over(namespace)
	want := map[string]any{"currency": "EUR", "dunning_days": 30, "ledger_url": "https://ledger.example.com/api"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the vars read %v, want %v", got, want)
	}
	if namespace["currency"] != "CHF" || len(namespace) != 2 || len(file) != 2 {
		t.Errorf("laying one over the other wrote into them: %v, %v", file, namespace)
	}
	// With nothing from the namespace, the file's own, as agk run --local reads them.
	if got := file.Over(nil); fmt.Sprint(got) != fmt.Sprint(map[string]any(file)) {
		t.Errorf("with no namespace the vars read %v", got)
	}
	if got := Vars(nil).Over(map[string]any{"x": 1}); fmt.Sprint(got) != "map[x:1]" {
		t.Errorf("a file writing no vars reads %v", got)
	}
}
