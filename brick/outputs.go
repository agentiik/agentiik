package brick

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/agk"
)

// portSuffix ends the file one envelope is written in. A port becomes a file name under
// /agk/out/ports/, and this is what follows it.
const portSuffix = ".json"

// Collect reads back what a step produced: one envelope for every output port it
// declares, taken from <port>.json under /agk/out/ports/.
//
// A declared port the container never wrote publishes an empty envelope, and that is
// success rather than an error. It is what lets a brick write only the port it has
// something to say on: a step that rejected nothing leaves rejected.json unwritten, the
// edge below still reads a batch of nothing, and the step downstream decides its own
// fate through when. A brick that writes nothing at all under /agk/out/ports/ and exits
// 0 therefore publishes an empty envelope on each of its ports, which is the whole of
// what it owes the contract.
//
// dir is the host side of /agk/out. What a container left under files/ is not read here:
// an artifact is uploaded and referenced by whatever holds the store, and the standard
// output shorthand for the single out port belongs there too, since only the caller that
// started the container captured the stream.
//
// m carries what the runner knows about the batch and the container does not decide: the
// run, the step, the attempt and the publication time. m.Port is not read, because the
// port is the name of the file the envelope was found in.
//
// The metadata a container writes is the metadata it was given. run_id, step, port and
// attempt reach it as AGK_RUN_ID, AGK_STEP, the file it chose to write and AGK_ATTEMPT,
// so an envelope disagreeing on any of them is not the batch that belongs on this port
// and is refused rather than quietly corrected. produced_at is the one member stamped
// here: a port is published when the emitting step ends, and that is a moment only the
// runner is in a position to know.
func Collect(dir string, declared []agk.Port, m agk.Meta, l agk.Limits) (map[agk.Port]agk.Envelope, error) {
	if dir == "" {
		return nil, errors.New("brick: no working directory to collect the outputs from")
	}

	ports := make(map[agk.Port]bool, len(declared))
	for _, port := range declared {
		if err := port.Validate(); err != nil {
			return nil, fmt.Errorf("brick: step %s: output port: %w", m.Step, err)
		}
		ports[port] = true
	}
	// Sorted, so that a container that broke the contract on two ports at once is
	// refused by the same one every time it is collected.
	names := slices.Sorted(maps.Keys(ports))

	if len(names) > 0 {
		// The metadata is stamped on every port nobody wrote, so a run, a step or an
		// attempt that could not travel in an envelope would travel unnoticed on
		// exactly the ports there is nothing else to check.
		if err := agk.Empty(m.RunID, m.Step, names[0], m.Attempt, m.ProducedAt).Validate(l); err != nil {
			return nil, fmt.Errorf("brick: step %s: the metadata to publish under is not metadata an envelope carries: %w", m.Step, err)
		}
	}

	portsDir := filepath.Join(dir, "ports")
	if err := checkPortsDir(portsDir, ports, names, m.Step); err != nil {
		return nil, err
	}

	out := make(map[agk.Port]agk.Envelope, len(names))
	for _, port := range names {
		e, err := collectPort(portsDir, port, m, l)
		if err != nil {
			return nil, err
		}
		out[port] = e
	}
	return out, nil
}

// checkPortsDir refuses what a container left under /agk/out/ports/ that is not one
// envelope of one declared port.
//
// A port the step does not declare is refused rather than dropped, because the declared
// names are the ones the container was given as AGK_OUT_PORTS: a file under another name
// is a brick writing to a port no edge is reading, and publishing the rest as though
// nothing had happened would lose that batch in silence.
//
// A missing directory is not a refusal. A brick that wrote nothing under /agk/out/ports/
// and exited 0 is a legitimate brick, and one that never created the directory has done
// no less than one that left it empty.
//
// An envelope is a file the container wrote and nothing else. A directory, a symbolic
// link or a device left under this name is refused rather than opened, because what is
// read here is read by the runner and on the runner's side of the boundary: a link would
// have the collection read a file the container could not reach itself.
func checkPortsDir(portsDir string, ports map[agk.Port]bool, declared []agk.Port, step agk.Step) error {
	entries, err := os.ReadDir(portsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("brick: step %s: %w", step, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		port, ok := strings.CutSuffix(name, portSuffix)
		if !entry.Type().IsRegular() || !ok || !ports[agk.Port(port)] {
			return fmt.Errorf("brick: step %s: %s holds %s, where one file is one envelope of one declared output port, written as <port>%s: the step declares %s", step, OutPortsDir, name, portSuffix, portNames(declared))
		}
	}
	return nil
}

// collectPort reads the envelope of one port, or publishes the empty one a port nobody
// wrote publishes.
func collectPort(portsDir string, port agk.Port, m agk.Meta, l agk.Limits) (agk.Envelope, error) {
	name := string(port) + portSuffix
	f, err := os.Open(filepath.Join(portsDir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return agk.Empty(m.RunID, m.Step, port, m.Attempt, m.ProducedAt), nil
	}
	if err != nil {
		return agk.Envelope{}, fmt.Errorf("brick: step %s: port %s: %w", m.Step, port, err)
	}
	defer f.Close()

	// The path is named rather than the step and the port, which a refusal coming out
	// of agk already carries: what this adds is the file inside the container a person
	// then goes and looks at.
	where := OutPortsDir + "/" + name
	e, err := agk.Decode(f, l)
	if err != nil {
		return agk.Envelope{}, fmt.Errorf("brick: %s: %w", where, err)
	}

	switch {
	case e.Meta.RunID != m.RunID:
		return agk.Envelope{}, fmt.Errorf("brick: %s: meta.run_id says %q and the run is %q, which the container reads as AGK_RUN_ID: %w", where, e.Meta.RunID, m.RunID, agk.ErrEnvelopeRejected)
	case e.Meta.Step != m.Step:
		return agk.Envelope{}, fmt.Errorf("brick: %s: meta.step says %q and the step is %q, which the container reads as AGK_STEP: %w", where, e.Meta.Step, m.Step, agk.ErrEnvelopeRejected)
	case e.Meta.Port != port:
		return agk.Envelope{}, fmt.Errorf("brick: %s: meta.port says %q and the envelope was written in the file of port %q: %w", where, e.Meta.Port, port, agk.ErrEnvelopeRejected)
	case e.Meta.Attempt != m.Attempt:
		return agk.Envelope{}, fmt.Errorf("brick: %s: meta.attempt says %d and this is attempt %d, which the container reads as AGK_ATTEMPT: %w", where, e.Meta.Attempt, m.Attempt, agk.ErrEnvelopeRejected)
	}

	e.Meta.ProducedAt = m.ProducedAt.UTC()
	return e, nil
}

// portNames writes the declared ports the way the container was given them, comma
// separated, so that a refusal and AGK_OUT_PORTS read alike.
func portNames(declared []agk.Port) string {
	if len(declared) == 0 {
		return "no output port"
	}
	names := make([]string, len(declared))
	for i, port := range declared {
		names[i] = string(port)
	}
	return strings.Join(names, ",")
}
