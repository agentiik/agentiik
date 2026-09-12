package main

import (
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/cmd/agk/internal/local"
	"github.com/agentiik/agentiik/graph"
)

// The secrets a local run supplies, and the check that happens before a daemon is touched.
//
// "Local runs bypass grants entirely because there is nothing to protect: the operator
// already owns the file, the daemon and the secrets they supply on the command line." What
// they do not bypass is the mounting: the value goes to the driver, which writes it to a file,
// binds it read-only at /agk/secrets/<name>, masks it out of the log and out of the payload
// before anything is written, and never puts it in an environment variable. A local run
// mounts a secret exactly as a server run does, which is what makes a workflow that works
// here work there.

// suppliedSecrets reads the values the command line supplied.
//
// --secret-file is read as the bytes it is, with nothing trimmed: a secret is bytes, a key
// file ends in a newline that is part of it, and a command line that quietly took one off
// would hand the container a value that hashes differently from the file beside it. --secret
// carries its value literally for the same reason.
//
// A name written twice is the last one winning, and the files are read before the values, so
// that a value typed at the terminal overrides a file in a script.
func suppliedSecrets(e Env, values, files []string) (map[string][]byte, error) {
	out := map[string][]byte{}

	for _, pair := range files {
		name, path, _ := strings.Cut(pair, "=")
		if name == "" {
			return nil, fmt.Errorf("--secret-file %s names no secret: it is written --secret-file <name>=<path>", pair)
		}
		value, err := os.ReadFile(e.path(path))
		if err != nil {
			// The path is named and the value is not, which is the one rule every
			// message about a secret follows.
			return nil, fmt.Errorf("--secret-file %s: %w", name, err)
		}
		out[name] = value
	}

	for _, pair := range values {
		name, value, _ := strings.Cut(pair, "=")
		if name == "" {
			return nil, fmt.Errorf("--secret %s names no secret: it is written --secret <name>=<value>", pair)
		}
		out[name] = []byte(value)
	}

	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// checkSecrets holds the run to the secrets its steps mount, before a container exists.
//
// A run that would die at the fourth shard of a fan-out for a value that was never coming
// should die at second zero instead, and it should name every missing name at once rather
// than one per attempt: that is one trip to the terminal instead of four.
//
// The refusal names the step as well as the secret, because the person reading it is about to
// open the file and the step is the line they are looking for.
func checkSecrets(w io.Writer, wf *graph.Workflow, supplied map[string][]byte) bool {
	missing := local.Missing(wf, supplied)
	if len(missing) == 0 {
		return true
	}
	for _, name := range slices.Sorted(maps.Keys(missing)) {
		fmt.Fprintf(w, "refused: the secret %s is mounted by %s and was not supplied: a local run takes its values from --secret %s=<value> or --secret-file %s=<path>\n",
			name, steps(missing[name]), name, name)
	}
	return false
}

// steps names the steps that mount one secret, in the order they were found, which is the
// order the workflow file writes them.
func steps(names []agk.Step) string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, string(name))
	}
	if len(out) == 1 {
		return "step " + out[0]
	}
	return "steps " + strings.Join(out, ", ")
}
