// {{name}} labels every item it is given with the step's label parameter.
//
// It is a starting point written by agk brick init: replace the loop with what the brick does,
// keep its cases under cases/ saying what it should publish, and run them with agk brick test.
package main

import "github.com/agentiik/brick-sdk/brick"

func main() {
	var label string
	if ok, err := brick.Param("label", &label); err != nil || !ok || label == "" {
		// 120 is invalid input: the step will be as wrong on every attempt, so it is never retried.
		brick.Refuse("no label: {{name}} writes the step's label into every item, and the step supplies it")
	}

	items, err := brick.Items("in")
	if err != nil {
		brick.Refuse("%s", err)
	}
	for i := range items {
		items[i].Data["label"] = label
	}
	if err := brick.Emit("out", items); err != nil {
		brick.Fail("%s", err)
	}
}
