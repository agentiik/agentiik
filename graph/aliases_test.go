package graph

import (
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
)

// A workflow file anybody holding workflow:write pushes, whose aliases stand for billions of values,
// is refused before it is decoded, rather than holding the API or the hook reading it until it runs
// out of memory; and so is a file it includes.
func TestAWorkflowWhoseAliasesStandForBillionsIsRefusedBeforeItIsRead(t *testing.T) {
	var laughs strings.Builder
	laughs.WriteString("  l0: &l0 [a, a, a, a, a, a, a, a, a]\n")
	for i := 1; i <= 12; i++ {
		fmt.Fprintf(&laughs, "  l%d: &l%d [%s]\n", i, i, strings.TrimSuffix(strings.Repeat(fmt.Sprintf("*l%d, ", i-1), 9), ", "))
	}
	entry := "apiVersion: agentiik.dev/v1\nkind: Workflow\nmetadata: { name: nightly }\nsteps:\n  a: { image: alpine, script: [\"true\"] }\n"
	for name, tree := range map[string]fstest.MapFS{
		"the entry point": {"agentiik.yaml": {Data: []byte(entry + "x:\n" + laughs.String())}},
		"a file it includes": {
			"agentiik.yaml": {Data: []byte(strings.Replace(entry, "steps:", "include: [ { path: common.yaml } ]\nsteps:", 1))},
			"common.yaml":   {Data: []byte("defaults:\n  labels:\n" + strings.ReplaceAll(laughs.String(), "  l", "    l"))},
		},
	} {
		_, err := Load(tree, "agentiik.yaml", nil)
		if err == nil || !strings.Contains(err.Error(), "aliases") {
			t.Errorf("%s standing for 9^13 values was read: %v", name, err)
		}
	}
}
