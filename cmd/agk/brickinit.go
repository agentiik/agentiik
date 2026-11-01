package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/agentiik/agentiik/cmd/agk/internal/templates"
)

// agk brick init: "Scaffolds a brick in a chosen language, with its manifest and test harness."
//
// What it writes is the template of agentiik/brick-sdk for that language, embedded in the binary
// by cmd/agk/internal/templates, so that it needs no network and writes what the release shipped:
// a manifest, the brick, a Dockerfile and two cases agk brick test runs, one the brick publishes
// and one it refuses with 120. The brick works as written, which is the point of a scaffold: the
// first thing its author does is build it and watch its cases pass, and every change after that
// is a change from something that worked.
//
// The language is asked for rather than defaulted. Each template is as much a brick as the
// others, and a default would make one of them the answer to a question nobody asked.
func brickInit(_ context.Context, e Env, args []string) int {
	fs := flags(e, "agk brick init", "agk brick init <name> --language "+strings.Join(templates.Languages(), "|")+" [--dir <path>]")
	language := fs.String("language", "", "The language of the brick: "+strings.Join(templates.Languages(), ", ")+".")
	dir := fs.String("dir", "", "Where to write it, which must not exist or be empty. Default: a directory named as the brick, here.")
	words, code, ok := positional(fs, args)
	if !ok {
		return code
	}
	if len(words) != 1 {
		fmt.Fprintln(e.Err, "agk brick init takes the brick's name, one word: it is metadata.name, the directory and the image's name")
		fs.Usage()
		return exitUsage
	}
	name := words[0]
	if *language == "" {
		fmt.Fprintf(e.Err, "--language names no language: there is a template in %s\n", strings.Join(templates.Languages(), ", "))
		return exitUsage
	}
	to := *dir
	if to == "" {
		to = name
	}
	if !filepath.IsAbs(to) {
		to = filepath.Join(e.Dir, to)
	}
	written, err := templates.Write(*language, name, to)
	if errors.Is(err, templates.ErrExists) {
		fmt.Fprintf(e.Err, "agk brick init: refused: %s, and a scaffold written there would overwrite what is in it\n", err)
		return exitRefused
	}
	if err != nil {
		fmt.Fprintf(e.Err, "agk brick init: %s\n", err)
		return exitUsage
	}

	rel, err := filepath.Rel(e.Dir, to)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = to
	}
	fmt.Fprintf(e.Out, "wrote the %s brick %s in %s:\n", *language, name, rel)
	for _, f := range written {
		fmt.Fprintf(e.Out, "  %s\n", f)
	}
	fmt.Fprintln(e.Out, "\nbuild it and run its cases:")
	fmt.Fprintf(e.Out, "  cd %s\n", rel)
	if *language == "go" {
		fmt.Fprintln(e.Out, "  go mod tidy")
	}
	fmt.Fprintf(e.Out, "  docker build -t %s:dev .\n", name)
	fmt.Fprintf(e.Out, "  agk brick test --image %s:dev\n", name)
	return exitSucceeded
}
