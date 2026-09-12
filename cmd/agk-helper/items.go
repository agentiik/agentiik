package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/agentiik/agentiik/agk"
)

// items writes the items of an input envelope, one JSON object per line.
//
//	agk items [--port <port>]
//
// JSON Lines and not the envelope, because that is what makes the documented pipe work:
// agk items | jq -r '.data.vat_number' gives one value per item, where the envelope itself
// would give one document and a jq expression to walk it. The whole item is written, id
// and files[] included, so that a script can read what is attached to the item it is
// looking at rather than only its payload.
//
// It reads the mount and never standard input. The envelope does arrive on standard input
// as well, and deliberately is not read from there: a script pipes something into this
// verb's own standard input in the documented example, and a port is what the item came
// in on, which standard input cannot name.
func items(e env, args []string) error {
	set := flags("items")
	named := set.String("port", "", "the input port to read, implied when one is mounted")
	if err := set.Parse(args); err != nil {
		return err
	}
	if err := noArguments(set, "agk items [--port <port>]"); err != nil {
		return err
	}

	port, err := inputPort(e, agk.Port(*named))
	if err != nil {
		return err
	}

	envelope, err := readEnvelope(filepath.Join(e.inDir(), string(port), EnvelopeFile))
	if err != nil {
		return err
	}

	// Written the way a payload travels inside the envelope: compact, with no HTML
	// escaping, since an item is bytes on a port and never a fragment of a page. The
	// encoder ends each value with a newline, which is what makes the output one item
	// per line.
	enc := json.NewEncoder(e.Out)
	enc.SetEscapeHTML(false)
	for _, item := range envelope.Items {
		if err := enc.Encode(item); err != nil {
			return err
		}
	}
	return nil
}

// inputPort resolves which port is being read: the one named, or the one mounted.
//
// It refuses rather than guesses. A step reading two ports and asking for neither gets
// the ports it has, because the alternative is a pipe that reads the wrong batch and a
// script that looks right.
func inputPort(e env, named agk.Port) (agk.Port, error) {
	mounted, err := mountedPorts(e)
	if err != nil {
		return "", err
	}

	if named != "" {
		if err := named.Validate(); err != nil {
			return "", err
		}
		if !slices.Contains(mounted, named) {
			if len(mounted) == 0 {
				return "", fmt.Errorf("port %s is not mounted: %s holds no input port, so this step has no edge into it", named, InDir)
			}
			return "", fmt.Errorf("port %s is not mounted: %s holds %s", named, InDir, portList(mounted))
		}
		return named, nil
	}

	switch len(mounted) {
	case 0:
		return "", fmt.Errorf("%s holds no input port: a step with no edge into it is given no envelope to read", InDir)
	case 1:
		return mounted[0], nil
	default:
		return "", fmt.Errorf("%s holds %s, so the port to read is named: agk items --port <port>", InDir, portList(mounted))
	}
}

// mountedPorts names every input port the container was given, in order.
//
// One directory per port is what the mount is, each holding envelope.json and the
// artifacts of that envelope beside it. A directory whose name is not a port name is
// passed over rather than refused: what is under this mount was put there by the runner,
// and this program is in no position to have an opinion about it.
//
// A missing /agk/in is no input port rather than an error, which is what a step with no
// edge into it has.
func mountedPorts(e env) ([]agk.Port, error) {
	entries, err := os.ReadDir(e.inDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ports []agk.Port
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		port := agk.Port(entry.Name())
		if port.Validate() != nil {
			continue
		}
		ports = append(ports, port)
	}
	slices.Sort(ports)
	return ports, nil
}
