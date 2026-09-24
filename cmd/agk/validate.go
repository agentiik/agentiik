package main

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/graph"
)

// agk validate: "Validates the YAML, resolves includes and inheritance, detects cycles,
// checks ports against the manifests of the referenced images."
//
// Four things, and the fourth is the one that needs a daemon: a manifest lives at
// /agk/brick.yaml inside an image, reading it means pulling the image, and only the driver
// may reach a daemon. So the first three are a file and the fourth is an image, and
// --manifests skip is the flag for the machine that has the file and not the images.
//
// Nothing here applies a rule of the language. graph.Load resolves the includes in
// declaration order, then extends depth first, then defaults, then the step's own values;
// graph.Check is the steps, the edges, the cycles, the workflow outputs, the mcp surface and
// the expression scopes; graph.Build holds each step to the manifest of the brick it runs. A
// rule restated here would be a rule that could disagree with the one a run applies.

// The two things --manifests can be.
const (
	manifestsRead = "read"
	manifestsSkip = "skip"
)

func validate(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk validate", "agk validate [-f <path>] [--manifests read|skip]")
	entry := fs.String("f", "", "The entry point to validate. Defaults to "+entryPoint+" in the directory the command is run in.")
	manifests := fs.String("manifests", manifestsRead,
		"read pulls the referenced images and checks each step's ports and params against the brick manifest inside them; skip validates the file alone.")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if *manifests != manifestsRead && *manifests != manifestsSkip {
		fmt.Fprintf(e.Err, "--manifests is %q: it is read or skip\n", *manifests)
		return exitUsage
	}

	wf, tree, _, err := load(e, *entry)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}
	// The inputs are the boundary a trigger fills, and a schema that does not compile is
	// a workflow whose first run cannot start. It is compiled here so that validate and
	// run cannot disagree about it.
	if _, err := declaredInputs(wf, tree); err != nil {
		refusal(e.Err, err)
		return exitRefused
	}

	referenced := references(wf)
	if *manifests == manifestsSkip {
		fmt.Fprintln(e.Out, resolved(wf))
		// Said rather than implied. A validate that silently skipped the port check
		// is a validate that passes a workflow the pre-receive hook will reject.
		fmt.Fprintf(e.Out, "The ports were not checked against the manifests of %s: --manifests skip\n",
			counted(len(referenced), "referenced image", "referenced images"))
		return exitSucceeded
	}

	read, code := readManifests(ctx, e, referenced)
	if code != exitSucceeded {
		return code
	}
	if _, err := graph.Build(wf, read); err != nil {
		refusal(e.Err, err)
		return exitRefused
	}

	fmt.Fprintln(e.Out, resolved(wf))
	fmt.Fprintf(e.Out, "%s held to the manifests of %s\n",
		counted(len(wf.Steps), "step", "steps"),
		counted(len(read), "referenced image", "referenced images"))
	return exitSucceeded
}

// reference is one image a workflow names, and the first step that names it.
//
// The step travels with it because every refusal of the driver names one, and the step that
// asked is the step a person has to go and look at. An image several steps share is read
// once, so the first of them in name order is the one named.
type reference struct {
	Image string
	Step  agk.Step
}

// references are the images whose manifests this workflow has to be held to, which is what
// graph.Images names: the image of every step that is neither a script step nor a call.
//
// A script step is passed over when the step is chosen as well as when the image is, even
// where it names an image a brick step names too, which a step running a script inside its
// own brick's image does. No manifest is read of a script step's image, so naming one as the
// step a manifest was read for would make agk validate refuse an image in different words
// from agk run --local, which reads the same manifests and names the first step that runs
// the image as a brick.
func references(wf *graph.Workflow) []reference {
	first := map[string]agk.Step{}
	for _, name := range slices.Sorted(maps.Keys(wf.Steps)) {
		st := wf.Steps[name]
		if st.Image == "" || len(st.Script) > 0 {
			continue
		}
		if _, ok := first[st.Image]; !ok {
			first[st.Image] = name
		}
	}
	images := graph.Images(wf)
	out := make([]reference, 0, len(images))
	for _, image := range images {
		out = append(out, reference{Image: image, Step: first[image]})
	}
	return out
}

// readManifests reads one brick manifest per referenced image, through the only package in
// this module that may reach a daemon.
//
// A daemon that cannot be reached is exit 4 and not exit 1: nothing about the workflow was
// refused, and telling somebody their file is wrong because their Docker is not running is
// telling them to fix the wrong thing. Each image is reported as it is read, because a pull is
// the slow part of this command and a line per image says which one it is waiting on, and
// because the ports a brick declares are the half of the contract the file has to match.
func readManifests(ctx context.Context, e Env, referenced []reference) (map[string]brick.Manifest, int) {
	if len(referenced) == 0 {
		return map[string]brick.Manifest{}, exitSucceeded
	}
	d, code := imageReader(e)
	if code != exitSucceeded {
		return nil, code
	}
	defer d.Close()
	return manifestsThrough(ctx, e, d, referenced)
}

// imageReader opens the driver a command reads images through without running any of them. Where
// it cannot, the refusal is said and the exit code it leaves with is answered instead.
func imageReader(e Env) (*driver.Docker, int) {
	policy := driver.DefaultPolicy()
	// A laptop is the machine this command is typed on and Docker Desktop does not remap,
	// so the floor is lifted here exactly as it is for a local run and the driver says
	// once what the machine gives up. Reading a manifest creates a container from the
	// image and never starts it, which is why the floor is read at all.
	policy.RequireUsernsRemap = driver.RemapLifted

	d, err := driver.New(driver.Config{
		Policy: policy,
		// Nothing is run, so nothing of this command's is written: a validate that
		// left a working directory behind would be a validate with a side effect.
		WorkRoot: os.TempDir(),
		Now:      e.now,
		Announce: func(s string) { fmt.Fprintln(e.Err, s) },
	})
	if err != nil {
		refusal(e.Err, err)
		return nil, leaving(err)
	}
	return d, exitSucceeded
}

// manifestsThrough is readManifests through a driver the caller opened, keyed by the image each
// reference names.
func manifestsThrough(ctx context.Context, e Env, d *driver.Docker, referenced []reference) (map[string]brick.Manifest, int) {
	read := make(map[string]brick.Manifest, len(referenced))
	for _, r := range referenced {
		m, err := d.Manifest(ctx, r.Step, r.Image)
		if err != nil {
			refusal(e.Err, err)
			return nil, leaving(err)
		}
		read[r.Image] = m
		fmt.Fprintf(e.Out, "%s: %s %s, reads %s, writes %s\n", r.Image, m.Metadata.Name, m.Metadata.Version,
			ports(m.InputPorts()), ports(m.OutputPorts()))
	}
	return read, exitSucceeded
}

// resolved is the success line: what the workflow is and what it is made of.
//
// A control names its effect, so the line says the workflow, its steps, its edges and its
// boundary rather than that something was validated.
func resolved(wf *graph.Workflow) string {
	name := wf.Metadata.Name
	if wf.Metadata.Namespace != "" {
		name = wf.Metadata.Namespace + "/" + name
	}
	return fmt.Sprintf("%s is valid: %s, %s, %s, %s", name,
		counted(len(wf.Steps), "step", "steps"),
		counted(edges(wf), "edge", "edges"),
		counted(len(wf.Inputs), "declared input", "declared inputs"),
		counted(len(wf.Outputs), "declared output", "declared outputs"))
}

// ports writes a list of port names, or says there are none.
func ports[T ~string](names []T) string {
	if len(names) == 0 {
		return "no port"
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, string(n))
	}
	return strings.Join(out, " ")
}
