package main

import (
	"context"
	"fmt"
)

// join is the verb that trades a join token for this runner's identity.
//
// It is named and refused in this version rather than left out, so that the table of verbs is the
// documented one from the first build: it generates the host's key, sends the claim and writes
// runner.env, and it arrives with the API's side of the join in the wire's shape.
func join(_ context.Context, e env, _ []string) int {
	fmt.Fprintln(e.Err, "agk-runner join: joining is not built into this version of the agent yet, so this host cannot join an installation with it")
	return exitRefused
}
