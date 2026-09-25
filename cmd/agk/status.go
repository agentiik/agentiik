package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
)

// agk status: how one run on an installation stands, read from GET /api/v1/runs/{id}: "run state,
// per-step state, envelope digests", and its inputs where the caller holds run:read_data.
//
// It reads and decides nothing, so it leaves with 0 whatever state the run is in: a script that
// waits on a run's verdict runs it with agk run, which exits 3 for one that did not succeed, or
// reads the state out of -o json. A step reads as the narration names it, verdict then ports, with
// the digest of each port beside its count, and the failures are the report agk run ends with.

func status(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk status", "agk status <run> [--server <url>] [-o json] [-v]")
	server := fs.String("server", "", "The installation the run is on. Defaults to "+serverVariable+".")
	output := fs.String("o", "", "json writes the installation's answer as it gave it.")
	verbose := fs.Bool("v", false, "Lists every task, and not only the ones that failed.")
	named, code, ok := positional(fs, args)
	if !ok {
		return code
	}
	if len(named) != 1 {
		fmt.Fprintln(e.Err, "agk status names one run, by the identifier agk run printed")
		return exitUsage
	}
	if *output != "" && *output != "json" {
		fmt.Fprintf(e.Err, "-o is %q: json is the one format there is\n", *output)
		return exitUsage
	}
	at, ok := reach(e, *server)
	if !ok {
		return exitUsage
	}
	run := named[0]

	var raw json.RawMessage
	if err := at.getJSON(ctx, "/api/v1/runs/"+url.PathEscape(run), &raw); err != nil {
		fmt.Fprintf(e.Err, "%s\n", aboutRun(run, err))
		if errors.Is(err, errUnreachable) {
			return exitNoOutcome
		}
		return exitRefused
	}
	if *output == "json" {
		var indented bytes.Buffer
		if err := json.Indent(&indented, raw, "", "  "); err != nil {
			fmt.Fprintf(e.Err, "the installation's answer about run %s is not JSON: %s\n", run, err)
			return exitNoOutcome
		}
		fmt.Fprintln(e.Out, indented.String())
		return exitSucceeded
	}
	var d db.RunDetail
	if err := json.Unmarshal(raw, &d); err != nil {
		fmt.Fprintf(e.Err, "the installation's answer about run %s could not be read: %s\n", run, err)
		return exitNoOutcome
	}
	describe(e.Out, d, e.now(), *verbose)
	return exitSucceeded
}

// describe writes how a run stands: what it is of and how far it got, each step as the narration
// would say it now, and what it was given, produced and ended with.
func describe(w io.Writer, d db.RunDetail, now time.Time, verbose bool) {
	fmt.Fprintf(w, "run %s: %s/%s@%s, %s\n", d.Run, d.Namespace, d.Workflow, short(d.Commit), standing(d, now))
	by := ""
	if d.TriggeredBy != "" {
		by = " by " + d.TriggeredBy
	}
	fmt.Fprintf(w, "%s%s at %s\n", d.Trigger, by, d.CreatedAt.UTC().Format(time.RFC3339))

	width := 0
	for _, s := range d.Steps {
		width = max(width, len(s.Step))
	}
	byStep := map[agk.Step][]db.TaskSummary{}
	for _, t := range d.Tasks {
		byStep[t.Step] = append(byStep[t.Step], t)
	}
	for _, s := range d.Steps {
		line := fmt.Sprintf("%-*s  %s", width, s.Step, s.Verdict)
		if s.Attempts > 1 {
			line += fmt.Sprintf(", attempt %d", s.Attempts)
		}
		for _, port := range slices.Sorted(maps.Keys(s.Ports)) {
			p := s.Ports[port]
			line += fmt.Sprintf(", %s %d %s", port, p.Items, shortDigest(p.Digest))
			if !p.PurgedAt.IsZero() {
				line += " (purged)"
			}
		}
		fmt.Fprintln(w, line)
		if !verbose {
			continue
		}
		for _, t := range byStep[s.Step] {
			fmt.Fprintf(w, "%-*s  %s %s\n", width, "", taskMark, describeTask(t))
		}
	}

	if len(d.Inputs) > 0 {
		fmt.Fprintf(w, "inputs: %s\n", strings.Join(slices.Sorted(maps.Keys(d.Inputs)), ", "))
	}
	for _, name := range slices.Sorted(maps.Keys(d.Outputs)) {
		fmt.Fprintf(w, "output %s: %s\n", name, counted(itemsOf(d.Outputs[name]), "item", "items"))
	}
	if d.ReplayFromStartOnly {
		fmt.Fprintln(w, "an input a step could restart from has been purged, so the run replays from its start only")
	}
	if fs := failuresOf(d); len(fs) > 0 {
		fmt.Fprintf(w, "%s:\n", counted(len(fs), "failure", "failures"))
		for _, f := range fs {
			failure(w, f)
		}
	}
}

// standing is the run's state with how long it has been in it or took.
func standing(d db.RunDetail, now time.Time) string {
	switch {
	case d.StartedAt.IsZero() && d.State.Terminal():
		return fmt.Sprintf("%s before it started", d.State)
	case d.StartedAt.IsZero():
		return fmt.Sprintf("%s for %s", d.State, elapsed(max(now.Sub(d.CreatedAt), 0)))
	case d.State.Terminal():
		return fmt.Sprintf("%s after %s", d.State, elapsed(d.FinishedAt.Sub(d.StartedAt)))
	}
	return fmt.Sprintf("%s for %s", d.State, elapsed(max(now.Sub(d.StartedAt), 0)))
}

// describeTask is one task on one line: its shard, attempt and state, the runner holding it and
// the exit code its ending carries.
func describeTask(t db.TaskSummary) string {
	line := ""
	if t.Shard != nil && !t.Shard.IsZero() {
		line = t.Shard.String() + " "
	}
	line += fmt.Sprintf("attempt %d %s", t.Attempt, t.State)
	if t.ExitCode != nil {
		line += fmt.Sprintf(", exit code %d", *t.ExitCode)
	}
	if t.Runner != "" {
		line += ", on " + t.Runner
	}
	return line
}

// shortDigest is a digest as a person compares two of them: its algorithm and twelve characters.
func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return "sha256:" + d
}
