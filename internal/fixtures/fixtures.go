// Package fixtures carries the released fixture corpus of agentiik/schemas, vendored
// and pinned.
//
// The corpus is what pins the envelope's shape to the documentation: each document
// under fixtures/envelope names, in the index, the rule it holds the reader to. A test
// that reads them is testing the rules the documentation states rather than the shape
// this implementation happens to have, which is the whole reason the corpus is
// consumed here instead of being reinvented as table entries.
//
// It is vendored rather than fetched so that a test run needs no network and a change
// upstream is a commit here that a person reads. It is embedded rather than read from
// the working directory so that agk's tests and schema's tests read one copy: a second
// copy is a second answer to what the shape is.
//
// Standard library only, which is what lets agk's own tests import it without breaching
// agk's rule. Test support: nothing at runtime reaches for it.
package fixtures

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
)

// Version is the agentiik/schemas release the tree under testdata was taken from. The
// same value is written in testdata/SCHEMAS_VERSION, where a person updating the corpus
// finds it, and a test holds the two together.
const Version = "0.1.0"

//go:embed testdata
var vendored embed.FS

// FS is the vendored tree, rooted where the repository roots it: envelope.schema.json
// at the top, the corpus under fixtures/.
var FS fs.FS

func init() {
	sub, err := fs.Sub(vendored, "testdata")
	if err != nil {
		panic("fixtures: the vendored tree is not where it is embedded from: " + err.Error())
	}
	FS = sub
}

// Case is one fixture and what it pins. File is its path inside FS. Valid says which
// way it pins it: a valid document has to be accepted, an invalid one refused. Rule is
// what an invalid document is refused by, in the corpus's own words, and Covers is what
// a valid one covers; each carries the one that applies to it.
type Case struct {
	File   string
	Valid  bool
	Rule   string
	Covers string
}

// index is the part of fixtures/index.json this package reads.
type index struct {
	Version  string `json:"version"`
	Fixtures struct {
		Envelope struct {
			Valid []struct {
				File   string `json:"file"`
				Covers string `json:"covers"`
			} `json:"valid"`
			Invalid []struct {
				File string `json:"file"`
				Rule string `json:"rule"`
			} `json:"invalid"`
		} `json:"envelope"`
	} `json:"fixtures"`
}

// Envelopes returns the envelope corpus, valid documents first, in the order the index
// lists them.
func Envelopes() ([]Case, error) {
	b, err := fs.ReadFile(FS, "fixtures/index.json")
	if err != nil {
		return nil, fmt.Errorf("reading the fixture index: %w", err)
	}
	var idx index
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, fmt.Errorf("reading the fixture index: %w", err)
	}
	if idx.Version != Version {
		return nil, fmt.Errorf("the vendored corpus says it is %s and this package is pinned to %s", idx.Version, Version)
	}

	e := idx.Fixtures.Envelope
	cases := make([]Case, 0, len(e.Valid)+len(e.Invalid))
	for _, c := range e.Valid {
		cases = append(cases, Case{File: "fixtures/" + c.File, Valid: true, Covers: c.Covers})
	}
	for _, c := range e.Invalid {
		cases = append(cases, Case{File: "fixtures/" + c.File, Rule: c.Rule})
	}
	for _, c := range cases {
		if _, err := fs.Stat(FS, c.File); err != nil {
			return nil, fmt.Errorf("the index names %s, which is not in the vendored tree: %w", c.File, err)
		}
	}
	return cases, nil
}
