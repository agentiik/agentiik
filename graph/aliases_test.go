package graph

import (
	"fmt"
	"runtime"
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

// The shape the review of the bound found through a merge key, an anchor defined again inside the
// anchor a merge reads, is refused at the entry point as it is by package yamlbound.
func TestAWorkflowRedefiningAnAnchorThroughAMergeIsRefused(t *testing.T) {
	var b strings.Builder
	b.WriteString("apiVersion: agentiik.dev/v1\nkind: Workflow\nmetadata: { name: nightly }\nsteps:\n  a: { image: alpine, script: [\"true\"] }\nx:\n  l0: &L0 s\n")
	for i := 1; i <= 7; i++ {
		fmt.Fprintf(&b, "  m%d: &M%d {k: &L%d [%s]}\n  s%d: &L%d s\n  u%d: {<<: *M%d}\n",
			i, i, i, strings.TrimSuffix(strings.Repeat(fmt.Sprintf("*L%d, ", i-1), 9), ", "), i, i, i, i)
	}
	_, err := Load(fstest.MapFS{"agentiik.yaml": {Data: []byte(b.String())}}, "agentiik.yaml", nil)
	if err == nil || !strings.Contains(err.Error(), "defined twice") {
		t.Errorf("a workflow redefining an anchor through a merge was read: %v", err)
	}
}

// A long key over many values is refused before it is parsed: the parser copies the path to a value
// once for every value beneath it, and a key of 1 MiB over 50,000 values is 50 GiB of paths.
func TestALongKeyOverManyValuesIsRefusedBeforeItIsParsed(t *testing.T) {
	var b strings.Builder
	b.WriteString("apiVersion: agentiik.dev/v1\nkind: Workflow\nmetadata: { name: nightly }\nsteps:\n  a: { image: alpine, script: [\"true\"] }\n")
	b.WriteString("x:\n  " + strings.Repeat("k", 1<<20) + ":\n")
	for i := range 50_000 {
		fmt.Fprintf(&b, "    v%d: 1\n", i)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, _, err := document([]byte(b.String()))
	runtime.ReadMemStats(&after)
	if err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("a key of 1 MiB over 50,000 values was read: %v", err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 256<<20 {
		t.Errorf("refusing it allocated %d MiB", allocated>>20)
	}
}
