package main

import (
	"context"
	"fmt"
)

// The verbs of the documented table that wait for something an installation does not serve yet, a
// sign-in, or that this binary does not carry, brick templates.
//
// Each one is in the table and each refuses naming what is missing. A verb the documentation
// lists and the binary does not know is a binary that looks broken, and "unknown command" for
// agk login is the wrong answer twice over: it says the documentation is wrong, and it says
// nothing about what would make the command work.
//
// The refusal is exit 1 and not exit 2. The command line was right: it named a verb the
// documentation lists, spelled as the documentation spells it, and what is missing is on the
// other side of it.

// absent is one verb of the table that cannot be served yet, with what it waits for and where
// that arrives from.
//
// missing is the whole clause and not a noun phrase dropped into one here. A template that
// wrote "there is no %s" round a phrase carrying its own article printed "there is no a server
// to register the workflow with", and the determiner is not the same word for every verb:
// brick init waits on templates and wants a plural with no article at all. A sentence nobody
// can read aloud is a sentence that reads as a broken binary, at the moment somebody is
// already stuck, so each verb writes its own.
//
// arrives is separate for the same reason: login waits on the API's sign-in and brick init waits on
// a release of agentiik/bricks, and one template saying "it arrives with principals" for both was
// telling the second to wait for the wrong thing.
func absent(name, missing, arrives string) func(context.Context, Env, []string) int {
	return func(_ context.Context, e Env, _ []string) int {
		fmt.Fprintf(e.Err, "%s: refused: %s. %s, and until then this binary validates, draws, runs and tests a workflow on this machine, and pushes, runs and follows one on an installation as its operator, with the token in %s\n", name, missing, arrives, tokenVariable)
		return exitRefused
	}
}

// withSignIn is what the verb that waits on an installation's sign-in waits for.
const withSignIn = "It arrives with the sign-in, in v0.3.0"
