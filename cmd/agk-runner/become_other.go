//go:build !unix

package main

import (
	"errors"
	"io/fs"

	"github.com/agentiik/agentiik/runner"
)

// errNotUnix is every answer here: a runner host is Linux, and serve drops from root only there.
var errNotUnix = errors.New("serve started as root drops to the agent's account on a unix host alone")

func socketGroup(string) (int, error) { return 0, errNotUnix }

func giveDir(string, fs.FileMode, runner.Owner) error { return errNotUnix }

func becomeAgent(runner.Owner, []int) error { return errNotUnix }
