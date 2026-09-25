package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/cmd/agk/internal/helper"
	"github.com/agentiik/agentiik/cmd/agk/internal/local"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/schema"
)

// agk run --local: "Runs the whole workflow against the local Docker daemon, with no
// controller, no bus and no database. Artifacts land in a working directory."
//
// This file is the command line of it and nothing else: the flags, the order the six merged
// packages are called in, and every byte printed. The run itself is
// cmd/agk/internal/local, which owns the working directory, the loop the evaluator
// deliberately does not have, and the four small things the driver asks for. If a scheduling
// decision or a collection appeared here it would be in the wrong place twice over, because
// both already have an owner and both would then have two.
//
// The order is fixed and every step of it is a call into a merged package. The file, then the
// inputs, then the secrets, then the daemon, then the manifests, then the run. The three that
// come before the daemon are the three that can refuse without anything having been started,
// which is the whole reason they come first: a run that dies at the fourth shard of a fan-out
// for a value that was never coming wasted four containers and said less than a refusal at
// second zero.

// Three readings are taken here and recorded rather than only decided.
//
// There is no --max-parallel. max_parallel is a rule the workflow file states, and a flag that
// changed how many containers run would make the local run a different engine from the server
// one, which is the one thing #command-line's Decision block says this command exists to
// prevent. A laptop that cannot take eight containers is a workflow that should say
// max_parallel: 3, and that is a fact the file can show.
//
// There is no --keep either, and that is the absence of a flag rather than a flag that does
// nothing. What a person would want kept is what a task was given and what it wrote, and that
// directory is removed with its container by the driver: "the working directory of a task is
// created fresh, owned by an unprivileged account, and removed with the container, so no
// residue of one namespace survives into the next task on that host". Nothing on this side can
// ask it not to, so a --keep here would be a flag that printed nothing and kept nothing. What is
// kept is kept already: the run record, the state, the logs and the output envelopes stay under
// the working directory, and the artifacts stay in the object store. Keeping a task's directory
// as well needs one setting on driver.Policy, which is an addition to that package and a
// sentence on the page, and neither is this group's to make quietly.
//
// --logs prints what each container wrote after the run and not while it runs. A log is written
// by the driver as the container writes it, masked line by line, and eight of them interleaved
// live is eight containers talking over each other; in order, after the run, each one is a thing
// a person can read. Following one live is agk logs, on a run on an installation.

// init adds the row for this command to the table.
//
// It is added by the file that implements it so that cmd/agk/main.go does not have to know
// what a local run is made of, and it is added where the documentation writes it, between
// agk graph and agk push, because the usage text is that table printed.
func init() {
	at := slices.IndexFunc(commands, func(c command) bool { return c.name == "push" })
	if at < 0 {
		at = len(commands)
	}
	commands = slices.Insert(commands, at, command{
		name:    "run",
		summary: "With --local, runs the whole workflow against the local Docker daemon, with no controller, no bus and no database. Artifacts land in a working directory. With --namespace, starts a run of a pushed commit on an installation and follows it until it ends.",
		run:     runLocal,
	})
}

func runLocal(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk run", "agk run --local [-f <path>] [--input name=value] [--input-file name=path] [--inputs <path>] [--secret name=value] [--secret-file name=path] [--dir <path>] [--helper <path>|none] [--require-userns-remap] [-o json] [-v] [--logs]\n\tagk run --namespace <namespace> [--server <url>] [--commit <commit>] [-f <path>] [--input name=value] [--input-file name=path] [--inputs <path>] [-o json] [-v]")
	isLocal := fs.Bool("local", false, "Runs against the Docker daemon of this machine.")
	entry := fs.String("f", "", "The entry point to run. Defaults to "+entryPoint+" in the directory the command is run in.")
	namespace := fs.String("namespace", "", "Runs on an installation instead, in this namespace, the workflow the commit holds, as agk push registered it.")
	server := fs.String("server", "", "The installation to run on. Defaults to "+serverVariable+".")
	commit := fs.String("commit", "", "The commit to run on an installation: a hash, a branch or a tag the repository holds. Defaults to HEAD.")

	var inputs, inputFiles, secretValues, secretFiles pairs
	fs.Var(&inputs, "input", "A workflow input, written name=value. The value is read as JSON and falls back to the string it is. Repeatable.")
	fs.Var(&inputFiles, "input-file", "A workflow input whose value is the contents of a file, written name=path. Repeatable.")
	document := fs.String("inputs", "", "A JSON object of workflow input names and values, read before --input-file and --input.")
	fs.Var(&secretValues, "secret", "A secret the run supplies, written name=value. Repeatable.")
	fs.Var(&secretFiles, "secret-file", "A secret whose value is the contents of a file, written name=path. Repeatable.")

	dir := fs.String("dir", "", "The working directory artifacts, logs and state land in. Defaults to "+local.DefaultDir+" beside the entry point.")
	helperPath := fs.String("helper", "", "The static helper to mount read-only at "+driver.BinPath+" for a script step. none mounts nothing.")
	requireRemap := fs.Bool("require-userns-remap", false, "Refuses a daemon that does not remap user namespaces. The floor is lifted by default here, because Docker Desktop does not offer the remapping.")
	output := fs.String("o", "", "json writes the output envelopes to standard output as one object keyed by name.")
	verbose := fs.Bool("v", false, "Narrates every task transition and not only every step transition.")
	showLogs := fs.Bool("logs", false, "Prints what each container wrote, after the run.")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if *output != "" && *output != "json" {
		fmt.Fprintf(e.Err, "-o is %q: json is the one format there is\n", *output)
		return exitUsage
	}
	if code, ok := oneKindOfRun(e, fs, *isLocal, *namespace); !ok {
		return code
	}
	if !*isLocal {
		return runOnServer(ctx, e, serverRun{
			entry: *entry, namespace: *namespace, server: *server, commit: *commit,
			inputs: inputs, inputFiles: inputFiles, document: *document,
			json: *output == "json", verbose: *verbose,
		})
	}

	// 1. The file. graph.Load resolves the includes in declaration order, then extends
	// depth first, then defaults, then the step's own values; graph.Check is the steps,
	// the edges, the cycles, the workflow outputs, the mcp surface and the expression
	// scopes. The tree is the working tree, unmodified.
	wf, tree, root, err := load(e, *entry)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}

	// 2. The inputs, through package schema: a $ref resolves inside the tree, required and
	// default are applied, and an undeclared input is refused, all of it before a run
	// exists, which is what graph.Options.Inputs expects.
	declared, err := declaredInputs(wf, tree)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}
	supplied, err := suppliedInputs(inputs, e.paths(inputFiles), e.path(*document))
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}
	bound, err := schema.Bind(declared, supplied)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}

	// 3. The secrets every step mounts, named now rather than at the fourth shard.
	secrets, err := suppliedSecrets(e, secretValues, secretFiles)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}
	if !checkSecrets(e.Err, wf, secrets) {
		return exitRefused
	}

	// 4. The daemon. Probe dials, asks what it is and closes, because the architecture the
	// helper has to be built for is the daemon's own and driver.Policy.Helper has to be set
	// before driver.New: an amd64 binary bound into an arm64 container gives a script an
	// exec format error rather than a program.
	daemon, err := driver.Probe(ctx, "")
	if err != nil {
		refusal(e.Err, err)
		return leaving(err)
	}
	fmt.Fprintf(e.Err, "the daemon at %s speaks API %s and runs %s/%s\n", daemon.Socket, daemon.APIVersion, daemon.OSType, daemon.Architecture)

	layout, err := local.NewLayout(workingDir(e, *dir, root))
	if err != nil {
		refusal(e.Err, err)
		return exitNoOutcome
	}

	// 5. The helper: --helper <path>, then --helper=none, then $AGK_HELPER, then the
	// embedded binary for the daemon's platform, then nothing and one sentence saying so.
	// It is a convenience and never a requirement, which is why nothing here refuses a
	// build that carries none.
	binary, mounted, err := helper.Resolve(*helperPath, e.getenv("AGK_HELPER"), daemon.OSType, daemon.Architecture, layout.Bin())
	if err != nil {
		refusal(e.Err, err)
		return exitNoOutcome
	}
	if !mounted {
		fmt.Fprintf(e.Err, "no static helper is mounted at %s: a script step that calls agk items, agk emit or agk attach will not find one, and jq and a redirect do the same job\n", driver.BinPath)
	}

	// The driver says its own sentences from inside Run, at the same time as the loop
	// narrates the transitions, so both go through one writer with a lock on it.
	story := &serial{w: e.Err}

	session, err := local.Open(ctx, local.Daemon{
		Socket:             daemon.Socket,
		Helper:             binary,
		RequireUsernsRemap: *requireRemap,
		Limits:             agk.DefaultLimits(),
		Announce:           func(s string) { fmt.Fprintln(story, s) },
	}, layout)
	if err != nil {
		refusal(e.Err, err)
		return leaving(err)
	}
	defer session.Close()

	// 6. The manifests, one per image the workflow names, read through the only package
	// that may reach a daemon and kept in its cache by image digest, and then graph.Build,
	// which holds every step to the brick it runs.
	g, err := session.Resolve(ctx, wf)
	if err != nil {
		refusal(e.Err, err)
		return leaving(err)
	}

	// 7. The run. Everything below this line has already been written: the loop is
	// internal/local's, and inside driver.Run are the mounts, the environment, the wait,
	// the collection and the spill.
	narration := newNarration(story, g, *verbose)
	out, err := session.Run(ctx, local.Request{
		Graph:   g,
		Tree:    root,
		Inputs:  bound,
		Vars:    wf.Vars,
		Secrets: secrets,
		Events:  narration.say,
	})
	if err != nil {
		refusal(e.Err, err)
		return noOutcome(err)
	}

	if *showLogs {
		printLogs(e.Err, layout, out)
	}

	report := e.Out
	if *output == "json" {
		// Standard output carries the command's answer and nothing else, so the
		// report moves aside when the answer is a document something else will read.
		report = e.Err
	}
	if out.Run.State != agk.Succeeded {
		reportFailure(e.Err, out)
		return exitNotSucceeded
	}

	reportSuccess(report, e.Dir, layout, out)
	if *output == "json" {
		if err := writeOutputs(e.Out, out.Outputs); err != nil {
			refusal(e.Err, err)
			return exitNoOutcome
		}
	}
	return exitSucceeded
}

// workingDir is where artifacts, logs and state land: --dir where it was given, and
// .agk beside the entry point where it was not.
//
// Beside the entry point rather than in the directory the command was typed in, because the
// run belongs to the workflow and not to wherever somebody happened to be standing. It is
// also a wart said out loud rather than hidden: .agk then sits inside the tree that is bound
// read-only at /agk/repo, so a container can see what previous runs wrote. At three in the
// morning discoverability is worth more than tidiness, and --dir moves it for anyone who
// minds.
func workingDir(e Env, flag, tree string) string {
	if flag != "" {
		return e.path(flag)
	}
	return filepath.Join(tree, local.DefaultDir)
}

// writeOutputs puts the output envelopes on standard output as one object keyed by name, so
// that a pipe into jq works.
func writeOutputs(w io.Writer, outputs map[string]agk.Envelope) error {
	doc, err := json.MarshalIndent(outputs, "", "  ")
	if err != nil {
		return fmt.Errorf("the output envelopes could not be written: %w", err)
	}
	_, err = fmt.Fprintln(w, string(doc))
	return err
}

// noOutcome is the code an error out of the run itself leaves with.
//
// The driver's own sentinels keep the meaning leaving already gives them. Anything else out of
// a run that had already started is an outcome nobody could determine rather than a file
// somebody should go and fix, which is the difference between exit 4 and exit 1: a run that
// broke its own state says nothing about the workflow.
func noOutcome(err error) int {
	if code := leaving(err); code == exitNoOutcome {
		return code
	}
	if errors.Is(err, graph.ErrRefused) {
		return exitRefused
	}
	return exitNoOutcome
}

// paths resolves every path of a repeatable name=path flag against the directory the command
// was run in, so that --input-file orders=./orders.json means what it says.
func (e Env) paths(ps pairs) []string {
	out := make([]string, 0, len(ps))
	for _, pair := range ps {
		name, path, found := strings.Cut(pair, "=")
		if !found {
			out = append(out, pair)
			continue
		}
		out = append(out, name+"="+e.path(path))
	}
	return out
}

// Which flags belong to which run. A local run's secrets, directory, helper and floor are this
// machine's, and on an installation the namespace declares the secrets and the runner holds the
// rest; the namespace, the installation and the commit name a server run. A flag of the other
// kind is refused rather than ignored, since a run that silently dropped --secret would be a run
// somebody believes had it.
var (
	localOnly  = []string{"secret", "secret-file", "dir", "helper", "require-userns-remap", "logs"}
	serverOnly = []string{"namespace", "server", "commit"}
)

// oneKindOfRun says whether the flags name one run, local or on an installation, and answers
// exitUsage where they do not.
func oneKindOfRun(e Env, fs *flag.FlagSet, isLocal bool, namespace string) (int, bool) {
	var set []string
	fs.Visit(func(f *flag.Flag) { set = append(set, f.Name) })
	switch {
	case isLocal:
		if wrong := slices.DeleteFunc(slices.Clone(set), func(n string) bool { return !slices.Contains(serverOnly, n) }); len(wrong) > 0 {
			fmt.Fprintf(e.Err, "--%s names a run on an installation, and --local runs on this machine: drop one or the other\n", wrong[0])
			return exitUsage, false
		}
	case namespace == "":
		fmt.Fprintln(e.Err, "run: --local runs the whole workflow on the Docker daemon of this machine, and --namespace runs it on an installation, in that namespace: pass one")
		return exitUsage, false
	default:
		if wrong := slices.DeleteFunc(slices.Clone(set), func(n string) bool { return !slices.Contains(localOnly, n) }); len(wrong) > 0 {
			fmt.Fprintf(e.Err, "--%s is a local run's, and a run on an installation has none: %s\n", wrong[0], whyNotOnServer(wrong[0]))
			return exitUsage, false
		}
	}
	return exitSucceeded, true
}

// whyNotOnServer says where a local run's flag lives for a run on an installation.
func whyNotOnServer(name string) string {
	switch name {
	case "secret", "secret-file":
		return "its secrets are the ones its namespace declares, which PUT /api/v1/{ns}/secrets/{name} writes"
	case "logs":
		return "agk logs follows what its containers write"
	}
	return "the runner that takes each task holds its own working directory, helper and floor"
}
