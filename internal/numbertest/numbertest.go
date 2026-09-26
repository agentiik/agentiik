// Package numbertest is one workflow whose numbers come from everywhere a number can come
// from, and the params every task of it has to be handed, for the tests that run it locally and
// on a server to hold the two to one answer.
//
// "A number written without a fraction or an exponent is an int in an expression, and any other
// a double, wherever it comes from: vars, inputs, defaults, matrix values." Each of those is
// read here by an expression that has no overload for the other kind, n + 1 for an int and
// x * 1.5 for a double, so a number read as the wrong kind fails its step with 120 rather than
// passing with a value nobody looks at. And there are three steps, so that the last is decided
// on a pass that read everything back from wherever the run keeps it.
//
// Test support: nothing at runtime reaches for it.
package numbertest

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/agentiik/agentiik/agk"
)

// Workflow is the workflow. Every step is a script step, so it runs with no manifest and no
// image pulled.
const Workflow = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
vars:
  n: 3
  x: 2.0
inputs:
  count: { schema: { type: integer, maximum: 9007199254740992 }, required: true }
  ratio: { schema: { type: number }, required: true }
  tenfold: { schema: { type: number }, required: true }
  size: { default: 4 }
  scale: { default: 2.0 }
steps:
  first:
    image: alpine:3.21
    script: [ "true" ]
    params:
` + numbers + `    outputs: [out]
  second:
    image: alpine:3.21
    needs:
      - { step: first, port: out, as: in }
    strategy:
      matrix:
        whole: [1, 2]
        half: [0.5, 2.0]
    script: [ "true" ]
    params:
` + numbers + `      whole_next: ${{ matrix.whole + 1 }}
      half_scaled: ${{ matrix.half * 1.5 }}
    outputs: [out]
  third:
    image: alpine:3.21
    needs:
      - { step: second, port: out, as: in }
    script: [ "true" ]
    params:
` + numbers + `    outputs: [out]
`

// numbers are the params every step is given, each reading one number by an expression that
// has no overload for the other kind.
const numbers = `      vars_n: ${{ vars.n + 1 }}
      vars_x: ${{ vars.x * 1.5 }}
      count: ${{ workflow.inputs.count + 1 }}
      ratio: ${{ workflow.inputs.ratio * 1.5 }}
      tenfold: ${{ workflow.inputs.tenfold * 1.5 }}
      size: ${{ workflow.inputs.size + 1 }}
      scale: ${{ workflow.inputs.scale * 1.5 }}
`

// Steps are the steps of Workflow, in the order they run.
var Steps = []agk.Step{"first", "second", "third"}

// Flags are the inputs as agk run --local is given them, one --input each, and Body is the same
// inputs as a request to start a run on a server writes them. 2.0 and 1e1 are doubles, written
// with a point and with an exponent; 3 is an int.
var (
	Flags = []string{"count=3", "ratio=2.0", "tenfold=1e1"}
	Body  = `{"count": 3, "ratio": 2.0, "tenfold": 1e1}`
)

// Stored is Body as the run holds it once bound: the exponent written out, so that PostgreSQL,
// which would write 1e1 back as 10, writes back a double, and the defaults applied as written.
const Stored = `{"count":3,"ratio":2.0,"scale":2.0,"size":4,"tenfold":10.0}`

// Params answers the params the task of one step and one shard is handed. shard is the index of
// a shard of second, counted from 1, and is ignored for the other steps.
func Params(step agk.Step, shard int) map[string]any {
	out := map[string]any{
		"vars_n": int64(4), "vars_x": 3.0,
		"count": int64(4), "ratio": 3.0, "tenfold": 15.0,
		"size": int64(5), "scale": 3.0,
	}
	if step != "second" {
		return out
	}
	// The combinations in variable name order, the last moving fastest: half, then whole.
	combinations := []struct {
		half  json.Number
		whole json.Number
	}{{"0.5", "1"}, {"0.5", "2"}, {"2.0", "1"}, {"2.0", "2"}}
	c := combinations[shard-1]
	whole, _ := c.whole.Int64()
	half, _ := c.half.Float64()
	// The matrix variables are injected into params as they were written, beside what the
	// expressions made of them.
	out["whole"], out["half"] = c.whole, c.half
	out["whole_next"], out["half_scaled"] = whole+1, half*1.5
	return out
}

// Differ says how params differ from what Params answers for the same task, and is empty where
// they do not. A value is compared with its Go type, which is the kind the expression answered
// with: an int64 is an int and a float64 a double.
func Differ(step agk.Step, shard int, params map[string]any) string {
	want := Params(step, shard)
	var out string
	for _, key := range slices.Sorted(maps.Keys(want)) {
		if got, ok := params[key]; !ok || got != want[key] {
			out += fmt.Sprintf("%s is %#v, want %#v; ", key, params[key], want[key])
		}
	}
	for _, key := range slices.Sorted(maps.Keys(params)) {
		if _, ok := want[key]; !ok {
			out += fmt.Sprintf("%s is %#v, and nothing is wanted of it; ", key, params[key])
		}
	}
	return out
}
