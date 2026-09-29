package brick_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/brick"
)

// laughs is a block of lines each naming the line before it nine times, which stands for nine to the
// power depth values in a few dozen bytes a line, indented as a value under a key.
func laughs(depth int, indent string) string {
	var b strings.Builder
	b.WriteString(indent + "l0: &l0 [a, a, a, a, a, a, a, a, a]\n")
	for i := 1; i <= depth; i++ {
		fmt.Fprintf(&b, "%sl%d: &l%d [", indent, i, i)
		for j := range 9 {
			if j > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "*l%d", i-1)
		}
		b.WriteString("]\n")
	}
	return b.String()
}

// A manifest is read out of any image a step names, and one whose aliases stand for billions of
// values is refused before it is decoded, rather than holding the runner or the API reading it until
// it runs out of memory. Every reading of one is held to it, the stored one included.
func TestAManifestWhoseAliasesStandForBillionsIsRefusedBeforeItIsRead(t *testing.T) {
	doc := []byte("apiVersion: agentiik.dev/v1\nkind: Brick\nmetadata: {name: invoice, version: 1.0.0}\nspec:\n  definitions:\n" + laughs(12, "    "))
	for name, parse := range map[string]func([]byte) (brick.Manifest, error){
		"ParseManifest": brick.ParseManifest, "ParseStoredManifest": brick.ParseStoredManifest,
	} {
		_, err := parse(doc)
		if err == nil || !strings.Contains(err.Error(), "aliases") {
			t.Errorf("%s read a manifest standing for 9^13 values: %v", name, err)
		}
	}
}
