package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/cmd/agk/internal/diff"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/graph"
)

// agk brick test: "Runs the brick against a set of sample envelopes and compares against
// expected outputs."
//
// It is the command the catalog runs in CI, so what a case is has to be plain enough to write
// by hand and strict enough to mean something. A case is a directory, and every file in it is
// a document the contract already names:
//
//	<case>/in/<port>.json     the envelope the brick is given on that input port
//	<case>/out/<port>.json    the envelope expected on that output port
//	<case>/files/<name>       the bytes of an artifact an input envelope attaches
//	<case>/params.json        what the brick reads at /agk/params.json, where it reads any
//	<case>/repo/              the workflow repository tree, bound read-only at /agk/repo
//	<case>/exit               the exit code the brick is expected to leave with, default 0
//
// The case's own name is the step the brick runs as, which is what AGK_STEP carries. A port
// the manifest declares and the case does not write an expectation for is expected to publish
// nothing, so a brick with five ports and one interesting one is one file and not five.
//
// What "the same envelopes" means is internal/diff and not this file, because the proof of
// v0.1.0 rests on the same answer and two answers would make that sentence mean whichever was
// written second. The default holds aside the run identifier, the publication time, the run
// segment of every artifact URI and, here alone, the item identities: a brick is entitled to
// mint them, the documentation's own shorthand mints them, and --ignore is where a case says
// that its brick does not.
//
// Three facts of an expected envelope's metadata are not read: its run_id, its step and its
// attempt. brick.Collect already refuses an envelope whose metadata disagrees with what the
// container was given, so those three have been checked by the collection before they reach
// here, and asking a fixture to repeat them would make renaming a case a change to its
// documents.

// defaultCases is where a brick keeps them, beside the manifest it publishes.
const defaultCases = "cases"

// caseTimeout bounds one case. A harness that waited for ever on a brick that hangs is a
// harness nobody can put in CI, and this is generous because the pull is not inside it: the
// deadline runs from the moment the container is created.
const caseTimeout = 5 * time.Minute

func brickTest(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk brick test", "agk brick test --image <ref> [--cases <dir>] [--ignore <members>]")
	image := fs.String("image", "", "The image to run, as the daemon holds it. A brick built on this machine and never pushed is not pulled.")
	cases := fs.String("cases", defaultCases, "The directory holding one directory per case.")
	held := fs.String("ignore", (diff.Default | diff.IgnoreItemIDs).String(),
		"The members of an envelope to hold aside when comparing: meta.run_id, meta.produced_at, files.uri, items.id. meta.produced_at is held aside whatever this says, since the runner stamps it.")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if *image == "" {
		fmt.Fprintln(e.Err, "--image names no image: agk brick test runs the brick, and the image is what it is")
		return exitUsage
	}
	ignore, err := diff.ParseIgnore(*held)
	if err != nil {
		fmt.Fprintf(e.Err, "--ignore %s\n", err)
		return exitUsage
	}
	// meta.produced_at is held aside whatever was asked for. A port is published when the
	// emitting step ends and the runner stamps the moment, so no fixture can carry it and
	// comparing it would refuse every case there is.
	ignore |= diff.IgnoreProducedAt

	dir := e.path(*cases)
	names, err := caseNames(dir)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}

	// One working root for the whole command, removed with it: what a case writes is
	// read back and compared, and nothing of it is wanted afterwards.
	work, err := os.MkdirTemp("", "agk-brick-test-")
	if err != nil {
		fmt.Fprintf(e.Err, "a working directory could not be prepared: %v\n", err)
		return exitNoOutcome
	}
	defer os.RemoveAll(work)

	policy := driver.DefaultPolicy()
	// The laptop and the CI runner this is typed on are both daemons without the
	// remapping, and the driver says once what that gives up. The same holds for a
	// daemon without seccomp, for a secrets directory that is not a tmpfs of the
	// runner's own, and for a brick named by its tag: a brick test is not a runner.
	policy.RequireUsernsRemap = driver.RemapLifted
	policy.RequireSeccomp = driver.SeccompLifted
	policy.RequireSecretsTmpfs = driver.SecretsTmpfsLifted
	policy.RequireDigest = driver.DigestLifted

	// The store is opened for the brick's own name, which is a namespace of one brick: an
	// artifact never crosses a namespace boundary, and the objects of this test belong to
	// nothing else. The name is in the manifest and the manifest is read through the driver
	// being built here, so the driver is given the store as a closure and the store is
	// opened before the first case, which is the only moment the closure is called at.
	var store *artifact.Store
	// The tree of the case being run, where it carries one. A brick may read the workflow
	// repository at /agk/repo, and a brick whose parameter names a file of it cannot be
	// tested at all without one: schema-validate takes its schema inline or as a path in
	// the tree, and only one of those two is a case anybody could write.
	//
	// It is the case's repo/ directory and never the case directory itself. What is under a
	// case is the harness's documents, and a brick reading those as a repository would be
	// reading the test rather than a tree somebody wrote.
	var tree string
	// A brick test keeps no log. The container's own output reaches the report through
	// the result and through what the brick wrote on its ports, which is what is being
	// tested; a log sink here would be a file nobody reads.
	d, err := driver.New(driver.Config{
		Store: func(namespace string) (*artifact.Store, error) {
			if store == nil || store.Namespace() != namespace {
				return nil, fmt.Errorf("no artifact store is open for namespace %s", namespace)
			}
			return store, nil
		},
		Policy: policy,
		// The cases run one at a time, so the tree of the one running is what this
		// answers. A driver is opened once for the whole command because the manifest
		// cache is what makes the second case as fast as the first.
		Repo:     func(context.Context, string, string, string) (string, error) { return tree, nil },
		WorkRoot: filepath.Join(work, "tasks"),
		Now:      e.now,
		Announce: func(s string) { fmt.Fprintln(e.Err, s) },
	})
	if err != nil {
		refusal(e.Err, err)
		return leaving(err)
	}
	defer d.Close()

	// The manifest is read once, before any case, and named after the first of them: it
	// says which ports are collected, and every refusal of the driver names a step.
	manifest, err := d.Manifest(ctx, agk.Step(names[0]), *image)
	if err != nil {
		refusal(e.Err, err)
		return leaving(err)
	}

	store, err = artifact.New(artifact.Dir(filepath.Join(work, "objects")), manifest.Metadata.Name, agk.DefaultLimits())
	if err != nil {
		fmt.Fprintf(e.Err, "the artifact store of this test could not be opened: %v\n", err)
		return exitNoOutcome
	}

	fmt.Fprintf(e.Out, "%s %s on %s: %s, writes %s\n", manifest.Metadata.Name, manifest.Metadata.Version, *image,
		counted(len(names), "case", "cases"), ports(manifest.OutputPorts()))

	matched := 0
	for _, name := range names {
		tree = treeOf(filepath.Join(dir, name))
		verdict := runCase(ctx, e, d, store, caseOf{
			Dir:      filepath.Join(dir, name),
			Name:     name,
			Image:    *image,
			Manifest: manifest,
			Ignore:   ignore,
		})
		if verdict.matched() {
			matched++
			fmt.Fprintf(e.Out, "case %s matched: %s\n", name, verdict.published)
			continue
		}
		report(e, name, verdict)
	}

	fmt.Fprintf(e.Out, "%s, %d matched\n", counted(len(names), "case", "cases"), matched)
	if matched != len(names) {
		return exitRefused
	}
	return exitSucceeded
}

// treeOf is the repository tree a case gives the brick, or nothing.
//
// A case with no repo/ directory gives the brick no tree, which is not a failure: most bricks
// never read one, and a tree invented for them would be a mount they did not ask for. A repo
// that is there but is a file rather than a directory is left to the driver to refuse, which
// names the step and the path.
func treeOf(dir string) string {
	path := filepath.Join(dir, "repo")
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// caseOf is one case, as everything that runs it needs it.
type caseOf struct {
	Dir      string
	Name     string
	Image    string
	Manifest brick.Manifest
	Ignore   diff.Ignore
}

// verdict is what became of one case.
//
// refused is a sentence where the case could not be run or read at all, which is a failure of
// the case and not of the brick. differences are what the brick produced against what the case
// expected, and the exit code is the one the container left with, where one ran.
type verdict struct {
	refused     string
	published   string
	hasExit     bool
	exitCode    int
	wantExit    int
	state       agk.TaskState
	differences []diff.Difference
}

func (v verdict) matched() bool {
	return v.refused == "" && len(v.differences) == 0 && v.hasExit && v.exitCode == v.wantExit
}

// runCase reads one case, runs the brick against it and compares what came back.
//
// Every document of the case is read before anything is started, so that a case nobody can
// read costs no container, and so that a case contradicting itself is told what is wrong with
// it rather than being run and reported as a mismatch.
func runCase(ctx context.Context, e Env, d *driver.Docker, store *artifact.Store, c caseOf) verdict {
	in, err := inputsOf(ctx, store, c)
	if err != nil {
		return verdict{refused: sentence(err)}
	}
	params, err := paramsOf(c.Dir)
	if err != nil {
		return verdict{refused: sentence(err)}
	}
	wantExit, err := exitOf(c.Dir)
	if err != nil {
		return verdict{refused: sentence(err)}
	}
	written, err := expectationsOf(c)
	if err != nil {
		return verdict{refused: sentence(err)}
	}
	if wantExit != 0 && len(written) > 0 {
		return verdict{refused: fmt.Sprintf("the case expects exit code %d and writes an expectation on %s: a step that failed publishes nothing, so there is nothing on a port to expect",
			wantExit, ports(slices.Sorted(maps.Keys(written))))}
	}

	run := agk.NewRunID()
	step := agk.Step(c.Name)
	declared := c.Manifest.OutputPorts()
	task := graph.Task{
		ID:        agk.NewTaskID(run, step, 1, agk.Shard{}),
		Run:       run,
		Namespace: store.Namespace(),
		Step:      step,
		Attempt:   1,
		Image:     c.Image,
		Params:    params,
		Inputs:    in,
		Outputs:   declared,
		// Every task gets a network of its own, and a brick under test is given the
		// posture its manifest asks for. egress is refused by the driver until the
		// proxy exists, which a case naming it will be told.
		Network: posture(c.Manifest.Spec.Runtime.Network),
		Timeout: graph.Duration(caseTimeout),
	}

	result, err := d.Run(ctx, task)
	if err != nil {
		// No outcome: the daemon, the image or the contract, and never the case's
		// expectation. It is reported as the refusal it is, naming the step.
		return verdict{refused: sentence(err), wantExit: wantExit}
	}

	v := verdict{
		hasExit:  result.State == agk.TaskSucceeded || result.State == agk.TaskFailed,
		exitCode: result.ExitCode,
		wantExit: wantExit,
		state:    result.State,
	}
	// The envelopes are compared where there are envelopes. A port is collected from a
	// container that succeeded, so a case whose brick failed is judged on the exit code it
	// was expected to leave with and on nothing else, and what it matched says that rather
	// than reporting empty ports it never published.
	if result.State == agk.TaskSucceeded {
		v.published = carried(declared, result.Outputs)
		v.differences = diff.Envelopes(expected(c, run, declared, written), result.Outputs, c.Ignore)
	} else {
		v.published = fmt.Sprintf("%s, exit code %d", result.State, result.ExitCode)
	}
	return v
}

// report says what became of a case that did not match, in the order the voice asks for: the
// step, then the exit code, then what was refused.
//
// It goes to standard error, because a case that did not match is a refusal and the report on
// standard output is the command's answer.
func report(e Env, name string, v verdict) {
	position := "no exit code"
	if v.hasExit {
		position = fmt.Sprintf("exit code %d", v.exitCode)
	}
	switch {
	case v.refused != "":
		fmt.Fprintf(e.Err, "case %s: %s: %s\n", name, position, v.refused)
	case !v.hasExit:
		fmt.Fprintf(e.Err, "case %s: %s: the task is %s\n", name, position, v.state)
	case v.exitCode != v.wantExit:
		fmt.Fprintf(e.Err, "case %s: %s: the case expects exit code %d, and the band is %s\n",
			name, position, v.wantExit, agk.Band(v.exitCode))
	default:
		fmt.Fprintf(e.Err, "case %s: %s: %s\n", name, position,
			counted(len(v.differences), "envelope member does not match", "envelope members do not match"))
	}
	for _, d := range v.differences {
		fmt.Fprintf(e.Err, "\t%s\n", d)
	}
}

// caseNames are the cases, in name order, so that a harness reports them the same way twice.
func caseNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("the cases could not be read: %w. A case is a directory of documents, one directory per case, under --cases", err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if err := agk.Step(entry.Name()).Validate(); err != nil {
			return nil, fmt.Errorf("the case %s cannot be a step name: %w. A case runs the brick as a step of its own name, which AGK_STEP carries", entry.Name(), err)
		}
		names = append(names, entry.Name())
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("%s holds no case: a case is a directory holding in/<port>.json and out/<port>.json", dir)
	}
	slices.Sort(names)
	return names, nil
}

// inputsOf reads the envelope the case puts on each input port, and stages the bytes of any
// artifact it attaches.
//
// The port is the name of the file, the way a port is the name of a directory under /agk/in/
// and the name of a file under /agk/out/ports/. An envelope is read through agk.Decode, so a
// sample that is not an envelope is refused here rather than inside a container.
func inputsOf(ctx context.Context, store *artifact.Store, c caseOf) (map[agk.Port]agk.Envelope, error) {
	dir := filepath.Join(c.Dir, "in")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// A brick that reads nothing is a brick, and a case for it writes no
			// input envelope.
			return nil, nil
		}
		return nil, err
	}

	in := map[agk.Port]agk.Envelope{}
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".json")
		if !entry.Type().IsRegular() || !ok {
			return nil, fmt.Errorf("in/ holds %s, where one file is one envelope of one input port, written as <port>.json", entry.Name())
		}
		port := agk.Port(name)
		if err := port.Validate(); err != nil {
			return nil, fmt.Errorf("in/%s: %w", entry.Name(), err)
		}
		if _, declared := c.Manifest.Spec.Inputs[port]; !declared {
			return nil, fmt.Errorf("the case feeds %s and the manifest of %s declares %s: the runner mounts one directory per port the brick declares, and there is no directory for a port the manifest never named",
				port, c.Manifest.Metadata.Name, ports(c.Manifest.InputPorts()))
		}
		envelope, err := envelopeAt(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		staged, err := stage(ctx, store, c.Dir, envelope)
		if err != nil {
			return nil, err
		}
		in[port] = staged
	}
	return in, nil
}

// stage puts the bytes of every artifact an input envelope attaches into the store, so that
// the mount a brick reads carries the file and not only its reference.
//
// The bytes are found beside the case under files/<name>, and what the store computed is held
// to what the envelope declared: a case whose size or digest disagrees with its own bytes is
// refused, because the envelope is what the brick reads and a reference that does not describe
// the file would be a case testing nothing. The URI is kept as the case wrote it, since that
// is the address the brick is told.
func stage(ctx context.Context, store *artifact.Store, dir string, e agk.Envelope) (agk.Envelope, error) {
	for _, item := range e.Items {
		for _, file := range item.Files {
			path := filepath.Join(dir, "files", file.Name)
			bytes, err := os.Open(path)
			if err != nil {
				return agk.Envelope{}, fmt.Errorf("item %s attaches %s and the bytes are not beside the case: %w. An artifact of a sample envelope is the file files/<name>", item.ID, file.Name, err)
			}
			put, err := store.Put(ctx, file.URI, file.MediaType, bytes)
			bytes.Close()
			if err != nil {
				return agk.Envelope{}, fmt.Errorf("item %s: %s could not be stored: %w", item.ID, file.Name, err)
			}
			if put.SHA256 != file.SHA256 || put.Size != file.Size {
				return agk.Envelope{}, fmt.Errorf("item %s attaches %s as %d bytes of sha256 %s and files/%s is %d bytes of sha256 %s: the envelope is what the brick reads, so what it says about a file is what the file has to be",
					item.ID, file.Name, file.Size, file.SHA256, file.Name, put.Size, put.SHA256)
			}
		}
	}
	return e, nil
}

// expectationsOf reads what the case says each port publishes: one envelope per document under
// out/, keyed by the port the document is named after.
//
// The run, the step and the attempt an expectation carries are taken from the case rather than
// from the document, because brick.Collect has already refused an envelope whose metadata
// disagrees with what the container was given: those three have been checked by the collection
// before they reach here, and asking a fixture to repeat them would make renaming a case a
// change to its documents.
func expectationsOf(c caseOf) (map[agk.Port]agk.Envelope, error) {
	dir := filepath.Join(c.Dir, "out")
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	declared := c.Manifest.OutputPorts()
	written := map[agk.Port]agk.Envelope{}
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".json")
		if !entry.Type().IsRegular() || !ok {
			return nil, fmt.Errorf("out/ holds %s, where one file is one envelope of one declared output port, written as <port>.json", entry.Name())
		}
		port := agk.Port(name)
		if !slices.Contains(declared, port) {
			return nil, fmt.Errorf("the case expects %s and the manifest of %s writes %s: outputs are collected from the ports the brick declares",
				port, c.Manifest.Metadata.Name, ports(declared))
		}
		envelope, err := envelopeAt(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		written[port] = envelope
	}
	return written, nil
}

// expected is what the case expects on every declared port, with the metadata of this run
// stamped on it.
//
// A port the case writes no expectation for is expected to publish nothing, which is what lets
// a brick of five ports be tested with one document: a declared port the container never wrote
// publishes an empty envelope, and that is success rather than an error.
func expected(c caseOf, run agk.RunID, declared []agk.Port, written map[agk.Port]agk.Envelope) map[agk.Port]agk.Envelope {
	step := agk.Step(c.Name)
	want := make(map[agk.Port]agk.Envelope, len(declared))
	for _, port := range declared {
		envelope, ok := written[port]
		if !ok {
			// The publication time is stamped when the port is published, which no
			// fixture can know and which is held aside here whatever --ignore says.
			envelope = agk.Empty(run, step, port, 1, time.Time{})
		}
		envelope.Meta.RunID, envelope.Meta.Step, envelope.Meta.Port, envelope.Meta.Attempt = run, step, port, 1
		want[port] = envelope
	}
	return want
}

// envelopeAt reads one envelope document, held to every rule agk states about one.
func envelopeAt(path string) (agk.Envelope, error) {
	f, err := os.Open(path)
	if err != nil {
		return agk.Envelope{}, err
	}
	defer f.Close()
	envelope, err := agk.Decode(f, agk.DefaultLimits())
	if err != nil {
		return agk.Envelope{}, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return envelope, nil
}

// paramsOf is what the brick reads at /agk/params.json, where the case writes any.
//
// They are not held to the manifest's own schemas here. A parameter is validated against the
// manifest where a step supplies one, which is the evaluator's business and needs a workflow;
// a case supplies a value directly, and a brick that refuses one is a brick doing its job.
func paramsOf(dir string) (map[string]any, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "params.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var params map[string]any
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, fmt.Errorf("params.json is not a JSON object of parameter names and values: %w", err)
	}
	return params, nil
}

// exitOf is the code the case expects the brick to leave with, and zero where it says nothing.
//
// It is a file of its own rather than a key of a document, because the ordinary case writes
// none: a case that expects a failure is saying one thing, and one line is the whole of it.
func exitOf(dir string) (int, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "exit"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("exit holds %q: it is the exit code the brick is expected to leave with, written as a number", strings.TrimSpace(string(raw)))
	}
	return code, nil
}

// carried says what a case produced, for the line that says it matched.
func carried(declared []agk.Port, out map[agk.Port]agk.Envelope) string {
	parts := make([]string, 0, len(declared))
	for _, port := range declared {
		parts = append(parts, fmt.Sprintf("%s on %s", counted(len(out[port].Items), "item", "items"), port))
	}
	if len(parts) == 0 {
		return "no declared port"
	}
	return strings.Join(parts, ", ")
}

// posture is the network a brick asks for in its manifest, as the evaluator spells it.
func posture(network string) graph.Network {
	switch network {
	case "egress":
		return graph.NetworkEgress
	case "internal":
		return graph.NetworkInternal
	default:
		return graph.NetworkNone
	}
}
