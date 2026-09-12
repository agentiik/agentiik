package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/agentiik/agentiik/agk"
)

// limits are the four size rules, at the values the documentation prints as their
// defaults.
//
// A namespace may carry its own, and the container is not told them: nothing in the
// contract's environment table names a limit. So these are what this side can know, and a
// refusal naming one says which number it applied. The runner applies the namespace's own
// when it collects, which is where the authority for them is; what this buys is that a
// script learns at the line that wrote the batch rather than at the end of the step.
func limits() agk.Limits { return agk.DefaultLimits() }

// readEnvelope reads one envelope off a path, through agk.Decode, so that every rule the
// document is held to holds here too. The path is named in the refusal because a person
// reading it is inside the container looking at files.
func readEnvelope(path string) (agk.Envelope, error) {
	f, err := os.Open(path)
	if err != nil {
		return agk.Envelope{}, err
	}
	defer f.Close()

	e, err := agk.Decode(f, limits())
	if err != nil {
		return agk.Envelope{}, fmt.Errorf("%s: %w", path, err)
	}
	return e, nil
}

// readPort reads the envelope already on an output port, and says whether there was one.
//
// A port with no file yet is not an error here: agk emit writes the first one and appends
// to it afterwards. The verb that needs one to exist says so itself.
func readPort(e env, port agk.Port) (agk.Envelope, bool, error) {
	path := portPath(e, port)
	envelope, err := readEnvelope(path)
	if errors.Is(err, fs.ErrNotExist) {
		return agk.Envelope{}, false, nil
	}
	if err != nil {
		return agk.Envelope{}, false, err
	}
	return envelope, true, nil
}

// agrees refuses an envelope already on a port that was not written under this run, this
// step and this attempt.
//
// It is the same reading the collection takes, brought forward. A file left under
// /agk/out/ports/ by something other than this task would be appended to and then refused
// whole, and the batch that was appended would be lost with it.
func agrees(envelope agk.Envelope, t task, port agk.Port, path string) error {
	switch {
	case envelope.Meta.RunID != t.Run:
		return fmt.Errorf("%s carries meta.run_id %q and the run is %q, which this container reads as %s", path, envelope.Meta.RunID, t.Run, EnvRunID)
	case envelope.Meta.Step != t.Step:
		return fmt.Errorf("%s carries meta.step %q and the step is %q, which this container reads as %s", path, envelope.Meta.Step, t.Step, EnvStep)
	case envelope.Meta.Port != port:
		return fmt.Errorf("%s carries meta.port %q and is the file of port %q", path, envelope.Meta.Port, port)
	case envelope.Meta.Attempt != t.Attempt:
		return fmt.Errorf("%s carries meta.attempt %d and this is attempt %d, which this container reads as %s", path, envelope.Meta.Attempt, t.Attempt, EnvAttempt)
	}
	return nil
}

// writePort writes the envelope of one port, validated first and in one step.
//
// Validated first, because a document this program wrote and the collection then refused
// would be a failure at the end of the step for a line in the middle of it. The rules are
// agk's own, so inline_max_bytes, envelope_max_bytes and max_items are answered here in
// the same words they will be answered in later.
//
// In one step, because a script is stopped at its deadline and a container killed halfway
// through a write leaves a truncated document where an envelope was. The temporary file
// sits in /agk/out and not in /agk/out/ports/, where anything that is not <port>.json is
// refused by the collection, and the rename is within the one mount /agk/out is.
func writePort(e env, port agk.Port, envelope agk.Envelope) error {
	if err := envelope.Validate(limits()); err != nil {
		return err
	}
	if err := os.MkdirAll(e.outPortsDir(), 0o755); err != nil {
		return err
	}
	return writeFile(e, portPath(e, port), func(w *os.File) error {
		_, err := envelope.Encode(w)
		return err
	})
}

// writeFile lays one file down whole: a temporary file under /agk/out, the bytes, the
// mode, then the rename.
//
// The mode is explicit because os.CreateTemp makes a file only its owner can read, and
// what this program writes is read by the runner afterwards, as a different account.
func writeFile(e env, path string, write func(*os.File) error) error {
	tmp, err := os.CreateTemp(e.outDir(), ".agk-"+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // a no-op once the rename has happened

	if err := write(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// portPath is the file one port's envelope is written in.
func portPath(e env, port agk.Port) string {
	return filepath.Join(e.outPortsDir(), string(port)+PortSuffix)
}

// identities refuses two items of one envelope carrying one identifier.
//
// An identity is what a fan-out shards on, what a zip pairs by and what a replay speaks
// about after the batch has been split and concatenated, so two items sharing one is not a
// duplicate record: it is two things the graph will treat as one. The check is on the
// whole envelope rather than on what this call added, because a second emit appends and
// the contradiction is in the document that travels.
func identities(envelope agk.Envelope, field string) error {
	seen := make(map[string]bool, len(envelope.Items))
	for _, item := range envelope.Items {
		if !seen[item.ID] {
			seen[item.ID] = true
			continue
		}
		if field != "" {
			return fmt.Errorf("two items carry the identifier %q, derived from the field %s: an identity is what a fan-out shards on and what a merge pairs by, so a field two items agree on is not one", item.ID, field)
		}
		return fmt.Errorf("two items carry the identifier %q: an identity is what a fan-out shards on and what a merge pairs by", item.ID)
	}
	return nil
}
