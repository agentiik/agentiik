package graph

import (
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
)

// A declaration compiles no more than InputSchemasMaxBytes of schema, a file counted once for
// every input that names it, and the same file named by fewer inputs compiles.
func TestADeclarationCompilesABoundedWeightOfSchema(t *testing.T) {
	file := `{"type": "array", "description": "` + strings.Repeat("x", 20<<10) + `"}`
	tree := fstest.MapFS{"schemas/order.json": &fstest.MapFile{Data: []byte(file)}}
	declaring := func(n int) *Workflow {
		wf := &Workflow{Inputs: map[string]Input{}}
		for i := range n {
			wf.Inputs[fmt.Sprintf("in%02d", i)] = Input{Schema: []byte(`{"$ref": "./schemas/order.json"}`)}
		}
		return wf
	}

	within := InputSchemasMaxBytes / (len(file) + 64)
	if _, err := declaring(within).DeclaredInputs(tree); err != nil {
		t.Errorf("%d inputs naming a file of %d bytes are refused: %v", within, len(file), err)
	}
	past := InputSchemasMaxBytes/len(file) + 1
	_, err := declaring(past).DeclaredInputs(tree)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprint(InputSchemasMaxBytes)) {
		t.Errorf("%d inputs naming a file of %d bytes answered %v", past, len(file), err)
	}

	// An input's own schema counts too, however it was written.
	inline := &Workflow{Inputs: map[string]Input{"big": {Schema: []byte(`{"description": "` + strings.Repeat("x", InputSchemasMaxBytes) + `"}`)}}}
	if _, err := inline.DeclaredInputs(nil); err == nil {
		t.Error("an inline schema past the weight compiled")
	}
}
