package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/graph"
	versions "github.com/agentiik/agentiik/version"
)

// agk validate: "Validates the YAML, resolves includes and inheritance, detects cycles,
// checks ports against the manifests of the referenced images."
//
// Four things, and the fourth is the one that needs a daemon: a manifest lives at
// /agk/brick.yaml inside an image, reading it means pulling the image, and only the driver
// may reach a daemon. So the first three are a file and the fourth is an image, and
// --manifests skip is the flag for the machine that has the file and not the images.
//
// Nothing here applies a rule of the language. version.Check is the one validation agk push,
// the push route and a pre-receive hook make: it resolves the includes in declaration order,
// then extends depth first, then defaults, then the step's own values; it checks the steps,
// the edges, the cycles, the workflow outputs, the mcp surface and the expression scopes; and
// it holds each step to the manifest of the brick it runs. A rule restated here would be a rule
// that could disagree with the one a hook applies.

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

	tree, dir, base, err := workingCopy(e, *entry)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}
	// The one validation agk push and a hook make, reaching what this machine reaches: the
	// manifests, through the local daemon, and none of the installation's stores. The inputs
	// are compiled in it, since a schema that does not compile is a workflow whose first run
	// cannot start.
	images := &daemon{e: e}
	defer images.close()
	c := versions.Checking{Entry: base}
	if *manifests == manifestsRead {
		c.Manifest = images.manifest
	}
	checked, err := versions.Check(ctx, tree, c)
	if err != nil {
		refusal(e.Err, inTree(err, dir))
		return leaving(err)
	}
	if checked.Library {
		fmt.Fprintf(e.Out, "%s is a library's root, a fragment other workflows include and nothing runs, and it resolves: %s\n",
			base, counted(len(checked.Included), "include", "includes"))
		return exitSucceeded
	}
	wf := checked.Workflow

	fmt.Fprintln(e.Out, resolved(wf))
	if *manifests == manifestsSkip {
		// Said rather than implied. A validate that silently skipped the port check
		// is a validate that passes a workflow the pre-receive hook will reject.
		fmt.Fprintf(e.Out, "The ports were not checked against the manifests of %s: --manifests skip\n",
			counted(len(graph.Images(wf)), "referenced image", "referenced images"))
		return exitSucceeded
	}
	fmt.Fprintf(e.Out, "%s held to the manifests of %s\n",
		counted(len(wf.Steps), "step", "steps"),
		counted(images.read, "referenced image", "referenced images"))
	return exitSucceeded
}

// daemon is the local daemon as version.Check's resolvers reach it: opened the first time an
// image has to be read, so that a workflow naming none, or naming each by digest with script
// steps alone, needs no Docker at all, and each image reported as it is read.
//
// "Reading /agk/brick.yaml means pulling an image", and only the driver may reach a daemon, so
// the manifests are read through it. A daemon that cannot be reached is exit 4 and not exit 1:
// nothing about the workflow was refused, and telling somebody their file is wrong because their
// Docker is not running is telling them to fix the wrong thing. A line per image says which one
// a pull is waiting on, and the ports a brick declares are the half of the contract the file has
// to match.
type daemon struct {
	e    Env
	d    *driver.Docker
	read int

	// texts are the manifests read, as the images hold them, by the image each was read out
	// of: what agk push records in the repository it pushes to.
	texts map[string][]byte
}

func (m *daemon) open() (*driver.Docker, error) {
	if m.d == nil {
		d, err := openImages(m.e)
		if err != nil {
			return nil, err
		}
		m.d = d
	}
	return m.d, nil
}

func (m *daemon) close() {
	if m.d != nil {
		m.d.Close()
	}
}

// pin resolves a tag to the digest this machine's daemon holds for it, on the machine that built
// or pulled the image: "a tag is a mutable pointer, and a commit must determine what ran".
func (m *daemon) pin(ctx context.Context, image string, step agk.Step) (string, error) {
	d, err := m.open()
	if err != nil {
		return "", err
	}
	pin, err := d.Pin(ctx, step, image)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(m.e.Out, "%s resolved to %s\n", image, pin)
	return pin, nil
}

// manifest reads the brick manifest inside an image, for the step that first runs it.
func (m *daemon) manifest(ctx context.Context, image string, step agk.Step) ([]byte, error) {
	d, err := m.open()
	if err != nil {
		return nil, err
	}
	mf, text, err := d.ManifestFile(ctx, step, image)
	if err != nil {
		return nil, err
	}
	if m.texts == nil {
		m.texts = map[string][]byte{}
	}
	m.texts[image] = text
	m.read++
	fmt.Fprintf(m.e.Out, "%s: %s %s, reads %s, writes %s\n", image, mf.Metadata.Name, mf.Metadata.Version,
		ports(mf.InputPorts()), ports(mf.OutputPorts()))
	return mf.Document(), nil
}

// openImages opens the driver a command reads images through without running any of them.
func openImages(e Env) (*driver.Docker, error) {
	policy := driver.DefaultPolicy()
	// A laptop is the machine this command is typed on and Docker Desktop does not remap,
	// so the floors are lifted here exactly as they are for a local run and the driver
	// says once what the machine gives up. Reading a manifest creates a container from the
	// image and never starts it, which is why the floors are read at all.
	policy.RequireUsernsRemap = driver.RemapLifted
	policy.RequireSeccomp = driver.SeccompLifted

	return driver.New(driver.Config{
		Policy: policy,
		// Nothing is run, so nothing of this command's is written: a validate that
		// left a working directory behind would be a validate with a side effect.
		WorkRoot: os.TempDir(),
		Now:      e.now,
		Announce: func(s string) { fmt.Fprintln(e.Err, s) },
	})
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
