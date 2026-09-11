package driver

import (
	"strings"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
)

// DefaultShell is what a step that names no shell gets.
//
// The step keyword table writes the default as ["/bin/sh", "-e"], which is the
// interpreter and the flag that makes the first non-zero exit end the step. What is
// actually executed is one command string, so the element that hands it over is here
// too: without -c there is nothing for the program to arrive on. A shell a step writes
// itself is written the same way the table writes the default, without that flag, so it
// is appended where it is missing rather than demanded of the author.
var DefaultShell = []string{"/bin/sh", "-e", "-c"}

// commandFlag is what hands a program to an interpreter. It ends the argv of every
// shell this package composes, whether the step named one or took the default.
const commandFlag = "-c"

// StdoutField is the member an empty script's item carries its captured standard output
// under.
//
// The documentation names the item and not its shape. One name is chosen here, once, so
// that a workflow reading ${{ item.data.stdout }} and a driver writing it agree; it
// belongs on the page, and until it is there this constant is the statement of it.
const StdoutField = "stdout"

// shorthandPort is the port a script that wrote nothing publishes on. The documentation
// names out and no other, so a step declaring different ports gets no shorthand rather
// than a batch on a port no edge is reading.
const shorthandPort agk.Port = "out"

// statusVar holds the exit code across the after_script, and afterFunc runs it. Both are
// written so that a script cannot shadow them by accident: a name beginning with two
// underscores is a name nothing in a workflow file has any reason to use.
const (
	statusVar = "__agk_status"
	afterFunc = "__agk_after"
)

// Whether a step is a script at all is isScript, in env.go, because the environment asks
// the same question: a script step is one where the image is a base image, no manifest is
// read from it and nothing about its ports is inferred. That is the whole of what
// changes. The mounts, the environment, the isolation and the exit code table are the
// contract every brick honours.

// scriptShell resolves the interpreter the commands run under: the step's own, then the
// one the runner policy sets, then DefaultShell.
//
// The policy sits between the two because an operator configures a runner and an author
// configures a step, and the more specific of the two wins. The returned slice is a copy,
// so that a caller appending the program to it cannot write into the default.
func scriptShell(t graph.Task, policy []string) []string {
	sh := t.Shell
	if len(sh) == 0 {
		sh = policy
	}
	if len(sh) == 0 {
		sh = DefaultShell
	}
	out := make([]string, len(sh))
	copy(out, sh)
	if out[len(out)-1] != commandFlag {
		out = append(out, commandFlag)
	}
	return out
}

// scriptCommand composes the one command a container runs from a step's three script
// keywords. A step that is a brick gets nothing, because what runs then is the entry
// point the image declares.
//
// The interpreter comes back as the entry point and the program as the command, which
// overrides the entry point a base image declares. A script step has to override it: a
// base image is not a brick, and postgres:17-alpine, which the documentation runs a
// script in, would otherwise hand the program to its own entry point script rather than
// run it.
//
// before_script, script and after_script are one shell invocation in one container, not
// three containers and not three execs. Three containers lose /tmp between them, which
// the documented example writes its intermediate results to, and three execs add a second
// lifecycle to get wrong for no gain.
func scriptCommand(t graph.Task, policy []string) (entrypoint, cmd []string) {
	if !isScript(t) {
		return nil, nil
	}
	return scriptShell(t, policy), []string{scriptProgram(t.BeforeScript, t.Script, t.AfterScript)}
}

// scriptProgram writes the program the three keyword lists become.
//
// The order is the one the keywords describe: before_script is prepended to script, and
// after_script is appended and runs in the same container even when script failed, so
// that a diagnostic dump survives a failure. That last property is what the EXIT trap is
// for. The status is captured at the top of the handler, before anything of the author's
// runs, and the handler exits with it, so after_script's own exit code does not change
// the step's verdict. A command in after_script that exits explicitly is the author
// overriding the verdict by hand, which no shell can refuse on our behalf.
//
// Nothing is interleaved between the author's commands. The first non-zero exit ending
// the step is the shell's own rule, which -e states and which the default carries; a step
// that names a shell without it has said in its own file that it wants the commands to
// run on. Injecting a status check after each command would take that choice back, and it
// would sit inside a here-document an entry opened.
//
// Commands are joined by newlines rather than by semicolons, because a command may end on
// a comment and a semicolon after one is swallowed with it.
func scriptProgram(before, script, after []string) string {
	var b strings.Builder
	if len(after) > 0 {
		b.WriteString(afterFunc + "() {\n")
		b.WriteString(statusVar + "=$?\n")
		// The handler runs whatever happened, so -e is turned off inside it: a
		// first failing diagnostic would otherwise take the rest of the dump with
		// it, and the shell would exit on that code rather than the step's.
		b.WriteString("set +e\n")
		writeCommands(&b, after)
		b.WriteString("exit $" + statusVar + "\n")
		b.WriteString("}\n")
		b.WriteString("trap " + afterFunc + " EXIT\n")
	}
	writeCommands(&b, before)
	writeCommands(&b, script)
	return b.String()
}

// writeCommands writes one keyword list, each entry on its own line and unindented. The
// entries are written exactly as the author wrote them, indentation included: a command
// may open a here-document whose terminator has to stand at the beginning of a line.
func writeCommands(b *strings.Builder, commands []string) {
	for _, c := range commands {
		b.WriteString(c)
		b.WriteString("\n")
	}
}

// stdoutShorthand publishes, for a script that wrote nothing and exited 0, one item on
// out carrying its captured standard output and any file it left in /agk/out/files/.
//
// That is what makes a two-line script worth writing: a step that pipes something through
// jq and prints the result does not have to learn the envelope to say what it found. It
// is a rule about scripts and not about bricks, so a brick that wrote nothing publishes
// empty envelopes and nothing else, which is what its contract already promises.
//
// It applies only where nothing was written at all. A script that wrote one of its ports
// has said what it publishes, and adding a batch it did not ask for to another port would
// put items on an edge the author did not write.
//
// collected is what brick.Collect returned and files are the artifacts already uploaded
// from /agk/out/files/. The captured output travels as an ordinary value of an item,
// which is to say inline_max_bytes applies to it exactly as it applies to anything else a
// step emits: this runs before the spill, and a script that printed more than may travel
// inline has its output moved into the store by it.
//
// The bytes are published as they were captured, the trailing newline included. Trimming
// would be the driver editing a payload, and a step that wants its output trimmed has a
// shell to do it with.
func stdoutShorthand(t graph.Task, code int, collected map[agk.Port]agk.Envelope, stdout []byte, files []agk.File) map[agk.Port]agk.Envelope {
	if !isScript(t) || code != 0 || !wroteNoItem(collected) {
		return collected
	}
	e, ok := collected[shorthandPort]
	if !ok {
		// The step declares no out. The documentation names that port and no
		// other, and publishing on a port the step did not declare would be
		// refused by the collection that just read them.
		return collected
	}

	if files == nil {
		files = []agk.File{}
	}
	item := agk.NewItem(map[string]any{StdoutField: string(stdout)})
	item.Files = files

	e.Items = []agk.Item{item}
	e.Meta.Count = len(e.Items)
	collected[shorthandPort] = e
	return collected
}

// wroteNoItem says whether the container left nothing on any of its declared ports. A
// port nobody wrote comes back as an empty envelope rather than as a missing key, so what
// is asked of each of them is whether it carries an item.
func wroteNoItem(collected map[agk.Port]agk.Envelope) bool {
	for _, e := range collected {
		if len(e.Items) > 0 {
			return false
		}
	}
	return true
}
