package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/cmd/agk/internal/local"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/schema"
)

// The voice, in one file: what a success line says, what a refusal reads as, and which exit
// code an error leaves with. The failure report of a run and its narration belong here too,
// beside these, for the same reason: a command line that said the same thing two ways is a
// command line nobody can read twice.
//
// #voice is four rules and they settle nearly everything printed here. A control names its
// effect, so a success line says what happened rather than that something did. An error
// names the step, the exit code and what was refused, in that order, which is already the
// field order of graph.Refusal and of driver.Fault, so a message is formatted out of the
// struct and never out of a string. Identifiers are never prettified: a state prints as
// succeeded, a rule as edge-port-not-declared, a port as rejected. Sentence case throughout.

// refusal writes one refusal as the line a person reading it needs.
//
// The leading graph:, driver: or brick: is trimmed. Those prefixes are there so that a
// message read in a log says which package refused; the person holding a YAML file at a
// terminal knows which command they typed and has never heard of the packages, and the step,
// the port and the rule are the three things they can act on.
func refusal(w io.Writer, err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(w, sentence(err))
}

// sentence is the refusal as one line, out of the struct where there is one.
//
// Each of the four types already writes the step, the port and the rule in the order the
// documentation asks for, so what is left to do here is to stop saying which package the
// refusal came from.
func sentence(err error) string {
	var refused *graph.Refusal
	if errors.As(err, &refused) {
		return trim(refused.Error())
	}
	var fault *driver.Fault
	if errors.As(err, &fault) {
		return trim(fault.Error())
	}
	var input *schema.InputRefusal
	if errors.As(err, &input) {
		return trim(input.Error())
	}
	var envelope *agk.Refusal
	if errors.As(err, &envelope) {
		return trim(envelope.Error())
	}
	return trim(err.Error())
}

// trim takes the package off the front of a message, wherever it is on the front.
//
// local: is in the list and is the one that was missing. It is the package behind agk run
// --local, so a working directory that could not be prepared said "local: the object store
// /dev/null/nope/objects could not be prepared" at a terminal: a name nothing outside this
// module can import, in front of the one sentence the person needed. A package of this binary's
// own is the easiest one to forget here, because it is the only one whose name is not also a
// directory the reader has seen.
func trim(s string) string {
	for _, prefix := range []string{"graph: ", "driver: ", "brick: ", "artifact: ", "schema: ", "local: "} {
		if rest, ok := strings.CutPrefix(s, prefix); ok {
			return rest
		}
	}
	return s
}

// leaving says which exit code an error leaves the process with.
//
// The line it draws is the one the driver already draws between a Result and an error: a
// refusal of the language is refused and nothing ran, which is exit 1, and trouble on this
// side is an outcome nobody could determine, which is exit 4. Charging the second to the
// first would tell whoever typed the command to go and fix their file.
func leaving(err error) int {
	switch {
	case err == nil:
		return exitSucceeded
	case errors.Is(err, driver.ErrDaemonUnreachable),
		errors.Is(err, driver.ErrUsernsRemapRequired),
		errors.Is(err, driver.ErrEgressProxyMissing),
		errors.Is(err, driver.ErrImagePullFailed):
		return exitNoOutcome
	}
	if charge, ok := driver.Charged(err); ok && charge == driver.ChargePlatform {
		return exitNoOutcome
	}
	return exitRefused
}

// counted writes a number with the word for it, singular where there is one of them, because
// a success line saying 1 steps is a success line written by a machine.
func counted(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// The narration of a run, and the report at the end of it.
//
// One line per step transition and not one per task. A fan-out of eight otherwise buries the
// two lines that matter, and the graph is known before the first container, so the step
// column is as wide as the longest step name and the lines read as a column. A shard appears
// when it is news, which is a failure, a retry or a timeout, and -v prints every task
// transition.
//
// All of it goes to standard error. The answer of a run is its output envelopes, and a
// narration on standard output would land in the pipe they were meant for.

// serial is one writer two goroutines may write to, which standard error is during a run:
// the loop narrates every transition, and the driver says its own sentences from inside Run,
// what the machine gives up and a log sink that failed. Two Fprintf
// on one writer is a data race, and before it is a race it is two half-lines spliced into
// one, which is worse than either sentence arriving late.
//
// Only the run needs it. Everything else this command writes to standard error it writes
// from the one goroutine that read the flags.
type serial struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *serial) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// narration prints what a run says as it says it.
type narration struct {
	w       io.Writer
	width   int
	verbose bool
}

// newNarration sizes the step column off the graph, which is known before anything runs.
func newNarration(w io.Writer, g *graph.Graph, verbose bool) *narration {
	n := &narration{w: w, verbose: verbose}
	if g != nil {
		for _, name := range g.Steps() {
			n.width = max(n.width, len(name))
		}
	}
	return n
}

// say writes one transition, where it is worth a line.
//
// An attempt of zero is the step itself rather than one of its shards, which is how the two
// kinds of line are told apart: an attempt is numbered from one everywhere in this module, so
// zero cannot name a task. On screen they are told apart by a mark, because a line about one
// container of eight and a line about the step those eight belong to are different news, and a
// person reading both needs to see which is which at a glance.
func (n *narration) say(e local.Event) {
	if e.Attempt == 0 {
		fmt.Fprintf(n.w, "%8s  %-*s  %s%s\n", elapsed(e.At), n.width, e.Step, e.Verdict, published(e.Ports))
		return
	}
	if !n.verbose && !news(e) {
		return
	}
	shard := ""
	if !e.Shard.IsZero() {
		shard = e.Shard.String() + " "
	}
	line := fmt.Sprintf("%8s  %-*s  %s %s%s", elapsed(e.At), n.width, e.Step, taskMark, shard, e.State)
	if e.State == agk.TaskFailed {
		line += fmt.Sprintf(", exit code %d, %s", e.ExitCode, agk.Band(e.ExitCode))
	}
	if !e.NextAttemptAt.IsZero() {
		line += fmt.Sprintf(", attempt %d at %s", e.Attempt, e.NextAttemptAt.Format("15:04:05"))
	}
	fmt.Fprintln(n.w, line+published(e.Ports))
}

// taskMark is what a line about one container of a step carries, so that it is not read as a
// line about the step. One character, because the step name beside it is what the eye is looking
// for, and a bar rather than a dot because the column to its left is a number with a dot in it.
const taskMark = "|"

// news says whether a task transition is worth a line of its own when the step lines are
// already being printed: a failure, a retry and a timeout are, and a shard going about its
// business is not.
func news(e local.Event) bool {
	switch e.State {
	case agk.TaskFailed, agk.TaskTimedOut, agk.TaskLost:
		return true
	}
	return !e.NextAttemptAt.IsZero()
}

// published writes what a port carries, in the ports' own order, or nothing at all.
func published(ports map[agk.Port]int) string {
	if len(ports) == 0 {
		return ""
	}
	out := make([]string, 0, len(ports))
	for _, port := range slices.Sorted(maps.Keys(ports)) {
		out = append(out, fmt.Sprintf("%s %d", port, ports[port]))
	}
	return ", " + strings.Join(out, ", ")
}

// elapsed is how long into the run a line is, to a tenth of a second, because a narration is
// read as a sequence and the moments are in the state beside it.
func elapsed(d time.Duration) string {
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// under is a path as the person reading it would type it next: relative to the directory the
// command was run in where it is inside it, and absolute where it is not.
//
// A local run writes under .agk beside the entry point, so the absolute form is the same
// eighty characters of prefix on every line of the report, five times over, and the part that
// differs is at the end where it is hardest to read. A --dir somewhere else prints in full,
// because then the prefix is the news.
func under(dir, path string) string {
	if dir == "" {
		return path
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}

// reportSuccess is the success line of a run and where everything it produced is.
//
// A control names its effect, so it says what the run did rather than that it finished: the
// run, the state, how long it took, what it ran, and one line per declared output naming the
// file and how many items it carries.
func reportSuccess(w io.Writer, dir string, l local.Layout, out local.Outcome) {
	fmt.Fprintf(w, "%s %s in %s: %s, %s\n", out.Run.Workflow, out.Run.State,
		elapsed(out.Run.FinishedAt.Sub(out.Run.StartedAt)),
		counted(len(out.State.Steps), "step", "steps"),
		counted(tasks(out.State), "container", "containers"))
	for _, name := range slices.Sorted(maps.Keys(out.Outputs)) {
		fmt.Fprintf(w, "%s: %s in %s\n", name,
			counted(len(out.Outputs[name].Items), "item", "items"),
			under(dir, filepath.Join(l.Outputs(out.Run.ID), name+".json")))
	}
	fmt.Fprintf(w, "run %s: %s\n", out.Run.ID, under(dir, l.Dir(out.Run.ID)))

	// A failure the run tolerated is still a failure and still worth its lines. What
	// continue_on_error did is not fail the run, and a success report that said nothing about
	// it would make a step that failed invisible.
	if len(out.Failures) > 0 {
		fmt.Fprintf(w, "%s under continue_on_error:\n", counted(len(out.Failures), "failure tolerated", "failures tolerated"))
		for _, f := range out.Failures {
			failure(w, f)
		}
	}
}

// reportFailure says what became of a run that did not succeed.
//
// The run state first, because it is the answer to what happened, and then one report per
// failure in the order the graph put them in.
//
// A terminal run with no failure at all is a run that was called off: cancelled by an interrupt
// or by a concurrency group, or timed out at the root deadline, with the containers it had
// stopped rather than failed. There is no exit code to name then and nothing was refused, so
// what the report has to say is which steps were stopped.
func reportFailure(w io.Writer, out local.Outcome) {
	if len(out.Failures) == 0 {
		line := fmt.Sprintf("%s %s: no step failed", out.Run.Workflow, out.Run.State)
		if stopped := cancelled(out.State); len(stopped) > 0 {
			line += ", and " + strings.Join(stopped, ", ") + " was stopped"
			if len(stopped) > 1 {
				line = strings.TrimSuffix(line, " was stopped") + " were stopped"
			}
		}
		fmt.Fprintln(w, line)
		return
	}
	fmt.Fprintf(w, "%s %s: %s\n", out.Run.Workflow, out.Run.State,
		counted(len(out.Failures), "step failed", "steps failed"))
	for _, f := range out.Failures {
		failure(w, f)
	}
}

// cancelled names the steps a stop reached, in name order.
//
// It is read off the shards and not off the step verdicts, because a run that ends on a cancel
// ends there: the evaluator's next plan is the stops, and a step whose container was called off
// is never settled to a verdict. The shard is where the cancelling shows, and it is what the
// driver reported.
func cancelled(state *graph.State) []string {
	if state == nil {
		return nil
	}
	var out []string
	for _, name := range slices.Sorted(maps.Keys(state.Steps)) {
		for _, shard := range state.Steps[name].Shards {
			if shard.Task == agk.TaskCancelled || shard.Task == agk.TaskTimedOut {
				out = append(out, string(name))
				break
			}
		}
	}
	return out
}

// failure writes one failure: the step, the exit code, and what was refused, in that order.
//
// That order is #voice and it is not negotiable here. Where no container ran there is no code
// to name and the position reads "no exit code": the driver deliberately invents none, and a
// command line that invented one for it would report somebody else's failure. What was
// refused is then the driver's own sentence, and the last lines of the log are what the
// container itself wrote.
func failure(w io.Writer, f local.Failure) {
	where := "step " + string(f.Step)
	if !f.Shard.IsZero() {
		where += " " + f.Shard.String()
	}
	if f.Attempt > 1 {
		where += fmt.Sprintf(", attempt %d", f.Attempt)
	}

	code := "no exit code"
	if f.HasExit {
		code = fmt.Sprintf("exit code %d, %s", f.ExitCode, f.Band)
	}

	line := fmt.Sprintf("%s: %s: %s", where, code, f.State)
	if f.Refused != "" {
		line += ": " + trim(f.Refused) + fmt.Sprintf(" (charged to %s)", f.Charge)
	}
	fmt.Fprintln(w, line)

	if f.Log == "" {
		return
	}
	for _, line := range tail(f.Log, logLines) {
		fmt.Fprintln(w, "  "+line)
	}
	fmt.Fprintln(w, "  the log: "+f.Log)
}

// logLines is how much of a log a failure report carries. Enough to see what the container
// was doing when it stopped, and little enough that a fan-out of eight failures is still a
// screen somebody reads rather than scrolls.
const logLines = 10

// tail reads the last lines of a task's log, as the container wrote them.
//
// As the container wrote them means the text and not the record around it. The driver writes
// one JSON object per line, carrying the moment, the index, the stream and the text, because
// that is what a log indexed by run, step, attempt and shard has to carry; what a person
// reading a failure wants is the words. The shape is driver.Line and it is decoded rather than
// guessed at, so a change to it is a compile error here and not a screen full of JSON.
//
// Every value the task was given has already been masked by literal match before any of this
// was written, so what is in the file is what may be shown.
func tail(path string, n int) []string {
	doc, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := logLinesOf(doc)
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// logLinesOf reads a task's log into the lines a person reads.
//
// A line that does not decode is shown as it is. The file is written by one writer in one
// format, so that case is a file somebody edited or a write that was cut off, and showing the
// bytes is more use than dropping them.
func logLinesOf(doc []byte) []string {
	var out []string
	for _, raw := range strings.Split(strings.TrimRight(string(doc), "\n"), "\n") {
		if raw == "" {
			continue
		}
		var line driver.Line
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			out = append(out, raw)
			continue
		}
		out = append(out, line.Text)
	}
	return out
}

// printLogs writes what every container of the run wrote, after the run.
//
// After and not during. A log is written by the driver as the container writes it, masked line
// by line, and eight of them interleaved live is eight containers talking over each other; in
// order, after the run, each one is a thing a person can read. The file is named by the task
// identifier, so this is the state read twice rather than a list somebody kept.
func printLogs(w io.Writer, l local.Layout, out local.Outcome) {
	if out.State == nil {
		return
	}
	for _, name := range slices.Sorted(maps.Keys(out.State.Steps)) {
		for _, shard := range out.State.Steps[name].Shards {
			id := agk.NewTaskID(out.Run.ID, name, shard.Attempt, shard.Shard)
			path, err := l.Log(id)
			if err != nil {
				continue
			}
			doc, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "%s:\n", id)
			for _, line := range logLinesOf(doc) {
				fmt.Fprintln(w, "  "+line)
			}
		}
	}
}

// tasks counts the containers a run asked for, which is one per shard of every attempt that
// ended and one for the attempt in flight.
func tasks(state *graph.State) int {
	if state == nil {
		return 0
	}
	n := 0
	for _, ss := range state.Steps {
		for _, shard := range ss.Shards {
			n += shard.Attempt
		}
	}
	return n
}
