package brick

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
)

// WriteInputs materialises what a step is given, under dir, and returns the mounts the
// driver binds read-only at /agk/in/<port>/.
//
// Each port gets a directory holding envelope.json and, beside it, the bytes of every
// files[] entry under its own name. The brick therefore opens a path and never a store
// URL: the agk:// URI stays in the envelope, logical and unfetchable from inside the
// container, which is what keeps the container off the object store and off the network
// it would need to reach one.
//
// The directory a file lands in is named by the key of in, which is the port of the
// consuming step, not meta.port of the envelope that travelled: a step reads what
// arrives on its own input port, whatever the port it was emitted from was called. A
// refusal names that input port and the step the envelope came from, which is the pair
// that locates the fault: an envelope one step wrote, arriving where another reads it.
func WriteInputs(ctx context.Context, s *artifact.Store, dir string, in map[agk.Port]agk.Envelope) ([]Mount, error) {
	if s == nil {
		return nil, errors.New("brick: no artifact store: the bytes of an input file are read through one")
	}
	if dir == "" {
		return nil, errors.New("brick: no working directory to write the inputs under")
	}

	mounts := make([]Mount, 0, len(in))
	// Sorted, so that two runs of the same step prepare the same mounts in the same
	// order and a task is legible when it is read twice.
	for _, port := range slices.Sorted(maps.Keys(in)) {
		envelope := in[port]
		if err := port.Validate(); err != nil {
			return nil, fmt.Errorf("brick: step %s: input port: %w", envelope.Meta.Step, err)
		}
		portDir := filepath.Join(dir, string(port))
		if err := os.MkdirAll(portDir, 0o755); err != nil {
			return nil, fmt.Errorf("brick: step %s: port %s: %w", envelope.Meta.Step, port, err)
		}
		if err := writeEnvelope(filepath.Join(portDir, envelopeFileName), envelope); err != nil {
			return nil, fmt.Errorf("brick: step %s: port %s: %w", envelope.Meta.Step, port, err)
		}
		if err := writeFiles(ctx, s, portDir, port, envelope); err != nil {
			return nil, err
		}
		mounts = append(mounts, Mount{
			Port:     port,
			Source:   portDir,
			Target:   InDir + "/" + string(port),
			ReadOnly: true,
		})
	}
	return mounts, nil
}

// writeFiles lays the artifacts of one port down beside its envelope.
//
// A name is written once per port. Two items attaching the same bytes under the same
// name is ordinary, a fan-in of a shared document being the usual case, and the second
// entry costs nothing. Two items attaching different bytes under one name on one port is
// a contradiction in the addressing itself, since agk://run/<run>/<step>/<port>/<name>
// then names two artifacts, and it is refused rather than resolved by letting one
// overwrite the other under the mount.
//
// The envelope is one of the files the mount holds, so its own name is spoken for. An
// artifact called envelope.json is refused for the same reason two artifacts of one name
// are: the mount cannot hold both, and the one that would be lost is the document the
// brick reads its batch from.
func writeFiles(ctx context.Context, s *artifact.Store, portDir string, port agk.Port, e agk.Envelope) error {
	written := make(map[string]string)
	for _, item := range e.Items {
		for _, file := range item.Files {
			// The whole of the file entry rule, asked of agk rather than restated
			// here: a name that carries a separator or a parent reference is a
			// name that writes outside the mount it was meant for, and the rule
			// for one is the rule the envelope it arrived in was held to.
			if err := file.Validate(); err != nil {
				return fmt.Errorf("brick: step %s: port %s: item %s: %w", e.Meta.Step, port, item.ID, err)
			}
			if file.Name == envelopeFileName {
				return fmt.Errorf("brick: step %s: port %s: item %s: a file called %q cannot be laid down under the mount, where that name is the port's own envelope, which the brick reads at %s/%s/%s", e.Meta.Step, port, item.ID, file.Name, InDir, port, envelopeFileName)
			}
			if held, ok := written[file.Name]; ok {
				if held == file.SHA256 {
					continue
				}
				return fmt.Errorf("brick: step %s: port %s: two files called %q carry different bytes, sha256 %s and %s, and one mount cannot hold both", e.Meta.Step, port, file.Name, held, file.SHA256)
			}
			if err := writeFile(ctx, s, filepath.Join(portDir, file.Name), file); err != nil {
				return fmt.Errorf("brick: step %s: port %s: item %s: %w", e.Meta.Step, port, item.ID, err)
			}
			written[file.Name] = file.SHA256
		}
	}
	return nil
}

func writeFile(ctx context.Context, s *artifact.Store, path string, f agk.File) (err error) {
	source, err := s.Open(ctx, f)
	if err != nil {
		return err
	}
	defer source.Close()

	// Removed first because an input written by an earlier attempt is read-only, and a
	// retry preparing the same directory has to be able to lay the file down again.
	os.Remove(path)
	target, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("file %s: %w", f.Name, err)
	}
	defer func() {
		target.Close()
		if err != nil {
			// Store.Open verifies the digest of what it read, so a copy that failed
			// leaves bytes nothing vouches for. They do not stay under a mount.
			os.Remove(path)
		}
	}()
	if _, err := io.Copy(target, source); err != nil {
		return fmt.Errorf("file %s: %w", f.Name, err)
	}
	if err := target.Close(); err != nil {
		return fmt.Errorf("file %s: %w", f.Name, err)
	}
	// The driver binds the mount read-only, and the bytes are made read-only here too,
	// so that a brick run without a container, by agk brick test, cannot write over its
	// own input either.
	if err := os.Chmod(path, 0o444); err != nil {
		return fmt.Errorf("file %s: %w", f.Name, err)
	}
	return nil
}

func writeEnvelope(path string, e agk.Envelope) (err error) {
	os.Remove(path)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		f.Close()
		if err != nil {
			os.Remove(path)
		}
	}()
	// Written as it travelled, agk:// URIs and all. The URI is the logical name of the
	// artifact on its port; the bytes are the file beside this one.
	if _, err := e.Encode(f); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, 0o444)
}
