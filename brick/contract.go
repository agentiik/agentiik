// Package brick is the two edges of the container, as directories, with no Docker code
// in reach.
//
// An image becomes a brick as soon as it honours the contract: it is given an envelope
// on standard input and under /agk/in/<port>/, and what it leaves under /agk/out/ is
// collected. Nothing else is promised, no daemon, no callback and no socket, and this
// package knows the contract and nothing about how a container is started. That is what
// lets agk brick test run a brick against sample envelopes with no driver in reach.
//
// WriteInputs materialises what a step is given, Collect reads back what it produced,
// and Spill is the other half of the size threshold: it moves a value the engine itself
// constructed above inline_max_bytes into the store and replaces it with a files[]
// entry.
package brick

import (
	"github.com/agentiik/agentiik/agk"
)

// The paths the contract names. A brick reads its inputs under InDir and writes its
// outputs under OutDir, and those two directories are the whole of what is promised.
const (
	Root        = "/agk"
	InDir       = "/agk/in"
	OutDir      = "/agk/out"
	OutPortsDir = "/agk/out/ports"
	OutFilesDir = "/agk/out/files"
)

// envelopeFileName is what an input envelope is called under its mount. A brick reads
// /agk/in/<port>/envelope.json, and the artifacts of that envelope sit beside it under
// their own names.
const envelopeFileName = "envelope.json"

// Mount is one input directory, as the driver has to bind it.
//
// ReadOnly is a stated field rather than something the driver is left to remember: an
// input a step can write to is an input a retry of that step reads differently, and the
// contract says the mount is read-only.
type Mount struct {
	Port     agk.Port
	Source   string
	Target   string
	ReadOnly bool
}
