package main

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agentiik/agentiik/agk"
)

// The paths of the contract, this side of the mount.
//
// They are spelled here and not imported from package brick, which names the same ones,
// because brick pulls a YAML parser in behind them and this binary is mounted inside an
// image the project does not control. The duplication is held by a test rather than by a
// comment: contract_test.go imports brick, compares every one of these against it, and
// writes an envelope through brick.WriteInputs to read back with the verb that reads
// them.
const (
	// Root is the one directory the contract promises, and the only one this
	// program reads or writes under.
	Root = "/agk"

	// InDir holds one directory per input port, each with its envelope and the
	// artifacts of that envelope beside it.
	InDir = "/agk/in"

	// OutDir is the writable half: what a step leaves here is collected.
	OutDir = "/agk/out"

	// OutPortsDir holds one envelope per output port, named <port>.json.
	OutPortsDir = "/agk/out/ports"

	// OutFilesDir holds the artifacts an envelope references, each under the name
	// its files[] entry gives it.
	OutFilesDir = "/agk/out/files"

	// EnvelopeFile is what the envelope of an input port is called under its mount.
	EnvelopeFile = "envelope.json"

	// PortSuffix ends the file one output envelope is written in.
	PortSuffix = ".json"

	// BinPath is where this program is mounted, read-only, and the name a script
	// calls it by. It is here so that the one line of help that says where this came
	// from says it once, and so that a test can hold it to the path the driver binds.
	BinPath = "/agk/bin/agk"
)

// The environment variables the three verbs read. The contract's table is wider than
// this; these four are the ones a verb cannot do its work without, and nothing else here
// reads the environment.
const (
	EnvRunID    = "AGK_RUN_ID"
	EnvStep     = "AGK_STEP"
	EnvAttempt  = "AGK_ATTEMPT"
	EnvOutPorts = "AGK_OUT_PORTS"
)

// task is what the container was told about itself, which is the whole of what this
// program knows: there is no graph in here, no manifest and nothing to ask.
//
// A missing variable is refused naming the variable. The reason is the one the runner
// will give anyway: an envelope whose meta disagrees with what the container was given is
// refused when it is collected, so the refusal is worth more coming from the tool that
// could still have been told.
type task struct {
	Run     agk.RunID
	Step    agk.Step
	Attempt int
	Ports   []agk.Port
}

// readTask reads the four variables, refusing each absence and each value that is not
// what the variable carries.
func readTask(getenv func(string) string) (task, error) {
	var t task

	run := getenv(EnvRunID)
	if run == "" {
		return task{}, fmt.Errorf("%s is not set: an envelope carries the run it was produced in, and one that disagrees with the run the container was given is refused when it is collected", EnvRunID)
	}
	t.Run = agk.RunID(run)
	if err := t.Run.Validate(); err != nil {
		return task{}, fmt.Errorf("%s carries %q, and %w", EnvRunID, run, err)
	}

	step := getenv(EnvStep)
	if step == "" {
		return task{}, fmt.Errorf("%s is not set: an envelope names the step that produced it, and one that disagrees with the step the container was given is refused when it is collected", EnvStep)
	}
	t.Step = agk.Step(step)
	if err := t.Step.Validate(); err != nil {
		return task{}, fmt.Errorf("%s carries %q, and %w", EnvStep, step, err)
	}

	attempt := getenv(EnvAttempt)
	if attempt == "" {
		return task{}, fmt.Errorf("%s is not set: an envelope carries the attempt it was produced on, which is what separates the batch that was kept from the ones that failed before it", EnvAttempt)
	}
	n, err := strconv.Atoi(attempt)
	if err != nil {
		return task{}, fmt.Errorf("%s carries %q, which is not a number: an attempt is a whole number, counting from 1", EnvAttempt, attempt)
	}
	if n < 1 {
		return task{}, fmt.Errorf("%s carries %d: attempts count from 1", EnvAttempt, n)
	}
	t.Attempt = n

	ports := getenv(EnvOutPorts)
	if ports == "" {
		return task{}, fmt.Errorf("%s is not set: the output ports a step declares reach the container in it, comma separated, and a step that declares none has no port to write on", EnvOutPorts)
	}
	for _, name := range strings.Split(ports, ",") {
		port := agk.Port(name)
		if err := port.Validate(); err != nil {
			return task{}, fmt.Errorf("%s carries %q, and %w", EnvOutPorts, name, err)
		}
		t.Ports = append(t.Ports, port)
	}

	return t, nil
}

// declares says whether the step declares this port, and refuses one it does not.
//
// The refusal belongs here rather than to the collection that would also give it. A port
// no edge is reading is a batch lost in silence, and the step is the only thing that can
// still be corrected: it is holding the name it wrote.
func (t task) declares(port agk.Port) error {
	for _, declared := range t.Ports {
		if declared == port {
			return nil
		}
	}
	return fmt.Errorf("step %s does not declare the output port %s: it declares %s, which is what it reads as %s", t.Step, port, portList(t.Ports), EnvOutPorts)
}

// meta is the metadata of an envelope this container publishes, stamped from what the
// container was given.
//
// produced_at is the one member that is not. A port is published when the emitting step
// ends, which is a moment only the runner is in a position to know, and it restamps this
// when it collects. What is written here is what makes the document valid on its own
// between now and then.
func (t task) meta(port agk.Port, at time.Time) agk.Meta {
	return agk.Meta{
		RunID:      t.Run,
		Step:       t.Step,
		Port:       port,
		Attempt:    t.Attempt,
		ProducedAt: at.UTC(),
	}
}

// portList writes a list of ports the way the container was given them, comma separated,
// so that a refusal and AGK_OUT_PORTS read alike.
func portList(ports []agk.Port) string {
	if len(ports) == 0 {
		return "no port"
	}
	names := make([]string, len(ports))
	for i, p := range ports {
		names[i] = string(p)
	}
	return strings.Join(names, ",")
}

// The four working paths, derived from the root the process was given rather than from
// the constants above, so that a test runs the verbs against a temporary tree. With the
// root at Root they are the constants, which is itself a test.
func (e env) inDir() string       { return filepath.Join(e.Root, "in") }
func (e env) outDir() string      { return filepath.Join(e.Root, "out") }
func (e env) outPortsDir() string { return filepath.Join(e.Root, "out", "ports") }
func (e env) outFilesDir() string { return filepath.Join(e.Root, "out", "files") }
