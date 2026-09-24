// Command agk is the Agentiik command line. It is the name the root doc.go reserved for
// this group, and the only name this group adds to the module's top level besides the
// helper binary that goes inside a container.
//
// What it contains is the command table, one flag.FlagSet per command, the exit-code
// table of the process itself, and every byte printed. What it does not contain is a
// scheduling decision, a collection, a container, or a daemon call of its own. Six
// packages are already merged and they do the work: agk the vocabulary, artifact the
// store, brick the two edges of the container, schema the input boundary, graph the
// evaluator, driver the containers. If a rule about when a step is ready, what a port
// carries, how many shards there are, what a merge means or which exit code means what
// appeared in this package, it would be a rule the workflow file cannot show, and it
// would be in the wrong place.
//
// # The commands
//
// The table is the one #command-line states, and it is data in this package rather than
// prose, so the usage text and the dispatch cannot disagree.
//
//	agk validate      the YAML, the includes, the inheritance, the cycles, and the ports
//	                  against the manifests of the referenced images
//	agk graph         the resolved graph as DOT or Mermaid, for review inside a merge
//	                  request
//	agk run --local   the whole workflow against the local Docker daemon, with no
//	                  controller, no bus and no database, artifacts landing in a working
//	                  directory
//	agk brick test    a brick against a set of sample envelopes, compared against
//	                  expected outputs
//
// Seven more verbs are in the documented table and reach an installation that does not
// exist at v0.1.0: login, whoami, push, share, grants, logs and brick init. Each is in
// this table and each refuses naming what is missing, because a verb the documentation
// lists and the binary does not know is a binary that looks broken. agk run without
// --local gets the same treatment, since there is nothing to reach and agk login arrives
// with the API.
//
// --version is a flag rather than a command, reporting what runtime/debug.ReadBuildInfo
// says, so the documented table stays exactly the table.
//
// # The voice
//
// #voice is four rules and they settle nearly everything printed here.
//
// A control names its effect, so every success line says what happened rather than that
// something did: the workflow, the counts, the images whose manifests were read.
//
// An error names the step, the exit code and what was refused, in that order. That is
// already the field order of graph.Refusal and of driver.Fault, so a message is formatted
// out of the struct through errors.As and never out of a string. Where no container ran
// there is no code to name, and the code position reads "no exit code" rather than
// carrying one that was invented: the driver deliberately invents none, and a command
// line that invented one for it would report somebody else's failure.
//
// Identifiers are never prettified. A state prints as succeeded, failed, pending,
// skipped, timed_out and cancelled; a rule prints as edge-port-not-declared; a port
// prints as rejected; a band prints in the exit-code table's own words. The same strings
// the YAML and the API use, so that what a person reads is what they type.
//
// Sentence case throughout.
//
// # Two output rules the voice section does not state
//
// Recorded here because they are readings rather than quotations, and because a command
// line that takes them differently per command is a command line nobody can pipe.
//
// Standard output carries the command's answer and nothing else: the drawing, the
// envelopes, the reports. Standard error carries everything said on the way to it. That
// is what makes agk graph | dot and agk run --local -o json | jq work, and what keeps a
// refusal out of a pipe.
//
// A run narrates one line per step transition and not one per task. A fan-out of eight
// otherwise buries the two lines that matter. A shard appears when it is news, which is a
// failure, a retry or a timeout, and -v prints every task transition.
//
// # Exit codes of the process
//
// Five, and each is a different thing for whoever typed the command to do next. The
// distinction between the last two is the one driver already draws between a Result and
// an error, carried to the shell.
//
//	0  The command did what it says. A run reached succeeded.
//	1  Refused, and nothing ran: the workflow was refused, an input was refused, a
//	   secret a step mounts was not supplied, a brick test case did not match.
//	2  The command line was wrong. This is the standard library flag package's own code
//	   and nothing translates it.
//	3  The run reached a terminal state other than succeeded: failed, cancelled or
//	   timed_out. It ran, and it did not succeed.
//	4  No outcome could be determined: the daemon could not be reached, the userns floor
//	   refused, network: egress was refused, a pull died, a working directory could not
//	   be prepared.
//
// # Where this package is cut
//
// Only cmd/agk is new at the module's top level, which is what the root doc.go reserved
// and all of it. Three packages sit under cmd/agk/internal, each because two callers need
// it or because it is testable with nothing behind it, and internal/local because a
// laptop's facts are not the command line's.
//
//	internal/local   one local run: the loop graph deliberately does not have, the
//	                 working directory layout, the store over a directory, and the four
//	                 small things driver asks for
//	internal/draw    the resolved workflow as DOT and as Mermaid, pure and golden-tested
//	internal/diff    what "the same envelopes" means, in one place, because agk brick
//	                 test and the milestone proof both rest on it
//	internal/helper  where the static helper is carried and laid down
//
// # Layout
//
//	main.go          the command table, the dispatch, run(ctx, Env, args) int
//	validate.go      agk validate
//	graph.go         agk graph
//	run.go           agk run --local
//	bricktest.go     agk brick test
//	absent.go        the seven verbs that reach an installation, each refusing by name
//	workflow.go      the one place a workflow is read, so validate and run cannot
//	                 disagree about what is valid
//	inputs.go        --input, --input-file, --inputs, through package schema and nothing
//	                 else
//	secrets.go       --secret, --secret-file, and the check that happens before a run
//	print.go         the voice: the success lines, the refusals, the failure report, the
//	                 progress narration
//	flags.go         the repeatable name=value flag, and --version
//	milestone_test.go  issue #83
//
// The whole command line is one function, run(ctx context.Context, e Env, args []string)
// int, and main is four lines around it. Env carries the two writers, the working
// directory, the clock, the environment lookup and os.Executable, so a test drives argv
// and reads bytes, which is what issue #83 needs and what no per-command entry point
// gives.
//
// # The proof of v0.1.0
//
// "A multi-step workflow with a fan-out and a merge runs end to end on a laptop, and
// running it again on the same inputs produces the same envelopes." That is a test and not
// a claim, and it is milestone_test.go over the fixture under testdata/milestone: four
// steps, three of them bricks built there from plain Dockerfiles and one a script step
// using the helper, a fan-out of three containers, a merge of two edges into one port, and
// one committed inputs file. The command line runs it twice against the daemon of this
// machine and the envelopes are compared through internal/diff, with only the three facts
// about which run this was held aside. Item identities and artifact digests are compared,
// which is why every step of the fixture derives its identities from its payload and why
// every artifact's bytes are derived from the order they belong to.
//
// The test skips where there is no daemon, so the suite stays green in CI and a laptop
// proves the sentence.
//
// # Readings taken here
//
// Each is also recorded beside the code that applies it, because otherwise the next
// reader settles it again and differently.
//
// There is no --max-parallel. max_parallel is a rule the workflow file states, and a flag
// that changed how many containers run would make the local run a different engine from
// the server one, which is the one thing #command-line's Decision block says this command
// exists to prevent. A laptop that cannot take eight containers is a workflow that should
// say max_parallel: 3, and that is a fact the file can show. If a machine ceiling is ever
// wanted, it gates before a dispatch is recorded and never after: a task held in a queue
// whose deadline had already started would be a task stopped for running out of time it
// never had.
//
// agk validate with --manifests skip says so in its own success line rather than implying
// a check it did not run. A validate that silently skipped the port check is a validate
// that passes a workflow the pre-receive hook will reject.
//
// A secret a step mounts and the command line did not supply refuses the run before a
// container exists, naming the step and the secret. Failing at the fourth shard of a
// fan-out for a value that was never coming wastes the run and says less.
//
// /etc/agentiik/runner.toml is not read. A local run is not a runner, driver.Policy is a
// value this side builds, and two sources for one setting is one too many. The userns floor
// is lifted by default and the seccomp floor always, and what the machine gives up is
// printed once, through the driver's own Announce sentences, which is what that hook exists
// for. They arrive while the run is narrating itself, from inside driver.Run and not from
// this goroutine, so Announce and the narration share one writer with a lock on it: two
// Fprintf on standard error is a data race and, before it is a race, it is two half-lines
// spliced into one.
package main
