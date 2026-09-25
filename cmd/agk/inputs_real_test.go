package main

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
)

// The inputs of a run, bound on a laptop by agk run --local and on an installation by the API, from
// the same flags: one set of inputs has to be one set of values wherever it runs.

const bindingWorkflow = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
inputs:
  orders:
    schema: { $ref: "./schemas/order.json" }
    required: true
  count: { schema: { type: integer } }
  ratio: { schema: { type: number } }
  big: {}
  flag: { schema: { type: boolean } }
  note: { schema: { type: string } }
  cycle:
    schema: { type: string }
    default: "2026-01"
  customers:
    default: [{ id: C-1, tier: 2 }]
  limit:
    schema: { type: integer }
    default: 3
  threshold:
    default: 2.5
outputs:
  invoices: { from: { step: normalize, port: ok } }
steps:
  normalize:
    image: docker.io/library/alpine@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc
    script: ["echo hello > /agk/out/ports/ok"]
    outputs: [ok]
`

// The values a local run binds and the values the controller starts a server run with, from the
// same command line, are the same JSON, and each input the command line supplied is the same Go
// value in both: a whole number a 64-bit float, as encoding/json reads it everywhere.
//
// A declared default is compared as JSON only. The workflow file hands it over as a json.Number,
// which a local run keeps and an expression reads as an integer, while a server run reads it back
// from the database as a float, as it read it when agk bound the inputs itself. Which of the two
// a whole number is has not been decided, and this binding changes neither.
func TestTheSameInputsAreTheSameValuesLocallyAndOnAnInstallation(t *testing.T) {
	in := anInstallation(t)
	dir := repository(t)
	write(t, dir, "agentiik.yaml", bindingWorkflow)
	write(t, dir, "schemas/order.json", `{"type": "array", "items": {"type": "object", "required": ["id"]}}`)
	commitAll(t, dir, "inputs")
	if code, said := in.pushFrom(t, dir); code != exitSucceeded {
		t.Fatalf("push answered %d: %s", code, said)
	}
	quickly(t)

	given := []string{
		"--input", `orders=[{"id":"A-1","amount":12.50,"lines":[1,2.0,3e2]}]`,
		"--input", "count=7",
		"--input", "ratio=0.1",
		"--input", "big=12345678901234567890",
		"--input", "flag=true",
		"--input", "note=2026-09",
	}
	supplied := []string{"orders", "count", "ratio", "big", "flag", "note"}

	// On a laptop: the flags read, and bound, as agk run --local reads and binds them.
	var flags pairs
	for i := 1; i < len(given); i += 2 {
		flags = append(flags, given[i])
	}
	wf, tree, _, err := load(Env{Dir: dir}, "")
	if err != nil {
		t.Fatal(err)
	}
	values, err := suppliedInputs(flags, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	local, err := bindInputs(wf, tree, values)
	if err != nil {
		t.Fatal(err)
	}

	// On the installation: agk run --namespace with the same flags, followed until it has said
	// which run it started, and the run read as the controller reads it to start its evaluator.
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	out, errs := &written{}, &written{}
	done := make(chan int, 1)
	go func() {
		done <- as(ctx, dir, in.url, out, errs, append([]string{"run", "--namespace", "finance"}, given...)...)
	}()
	var run agk.RunID
	for deadline := time.Now().Add(10 * time.Second); run == ""; {
		select {
		case code := <-done:
			t.Fatalf("agk run answered %d before starting a run: %s", code, errs)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("agk run never said which run it started: %s", errs)
		}
		if said, found := strings.CutPrefix(errs.String(), "run "); found {
			run = agk.RunID(strings.Fields(said)[0])
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	<-done
	var e db.Evaluation
	if err := in.pool.Installation(t.Context(), db.ControllerSweep, func(ctx context.Context, w *db.Wide) error {
		var err error
		e, err = w.Run(ctx, run)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	server := e.Inputs

	if a, b := slices.Sorted(maps.Keys(local)), slices.Sorted(maps.Keys(server)); !slices.Equal(a, b) {
		t.Fatalf("a local run binds %v and a server run %v", a, b)
	}
	for name, v := range local {
		a, _ := json.Marshal(v)
		b, _ := json.Marshal(server[name])
		if string(a) != string(b) {
			t.Errorf("input %s is %s locally and %s on the installation", name, a, b)
		}
	}
	for _, name := range supplied {
		if !reflect.DeepEqual(local[name], server[name]) {
			t.Errorf("input %s is %#v locally and %#v on the installation", name, local[name], server[name])
		}
	}
}
