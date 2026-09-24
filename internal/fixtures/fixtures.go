// Package fixtures carries the released fixture corpus of agentiik/schemas, vendored
// and pinned.
//
// The corpus is what pins the shapes to the documentation: each document under
// fixtures/ names, in the index, the rule it holds the reader to. A test that reads
// them is testing the rules the documentation states rather than the shape this
// implementation happens to have, which is the whole reason the corpus is consumed here
// instead of being reinvented as table entries.
//
// It is vendored rather than fetched so that a test run needs no network and a change
// upstream is a commit here that a person reads. It is embedded rather than read from
// the working directory so that agk's tests, schema's tests and the evaluator's tests
// read one copy: a second copy is a second answer to what the shape is.
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
const Version = "0.2.0"

//go:embed testdata
var vendored embed.FS

// FS is the vendored tree, rooted where the repository roots it: the schema documents
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

	// RefusedBy is how an invalid document is refused, and it is what divides the
	// labour between a reader and a validator. "schema" means the JSON Schema document
	// alone refuses it, which is shape and which closed decoding gives for nothing.
	// "validator" means the documentation states the rule and JSON Schema cannot
	// express it, because it needs the graph, the brick manifest, the included file or
	// the expression language; those fixtures are schema valid on purpose. It is empty
	// on a valid document.
	RefusedBy string

	// Manifest is the brick manifest elsewhere in the corpus that this fixture is
	// checked against. The rule such a fixture pins is about the two documents
	// together, so neither file alone shows it. Empty when the fixture stands alone.
	Manifest string

	// Role says that a fixture is not an entry point. The one fixture carrying it is
	// the included file itself, which is where the rule it pins can be seen at all.
	Role string
}

// entry is one fixture as the index writes it. Valid and invalid entries carry
// different keys, and one struct reads both because the keys do not collide.
type entry struct {
	File      string `json:"file"`
	Covers    string `json:"covers"`
	Rule      string `json:"rule"`
	RefusedBy string `json:"refused_by"`
	Manifest  string `json:"manifest"`
	Role      string `json:"role"`
}

// corpus is one document's fixtures: what must be accepted, and what must be refused.
type corpus struct {
	Valid   []entry `json:"valid"`
	Invalid []entry `json:"invalid"`
}

// index is the part of fixtures/index.json this package reads.
type index struct {
	Version  string `json:"version"`
	Fixtures struct {
		Envelope corpus `json:"envelope"`
		Workflow corpus `json:"workflow"`
		Brick    corpus `json:"brick"`

		// The wire is one document holding several messages, so the index names each
		// message separately and so does this.
		TaskMessage        corpus `json:"task-message"`
		TaskResult         corpus `json:"task-result"`
		RunnerRegistration corpus `json:"runner-registration"`
		RunnerHeartbeat    corpus `json:"runner-heartbeat"`
		GrantRedemption    corpus `json:"grant-redemption"`
		LogShipment        corpus `json:"log-shipment"`
		RunnerPool         corpus `json:"runner-pool"`
		Stop               corpus `json:"stop"`
	} `json:"fixtures"`
}

// Envelopes returns the envelope corpus, valid documents first, in the order the index
// lists them.
func Envelopes() ([]Case, error) { return read(func(i index) corpus { return i.Fixtures.Envelope }) }

// Workflows returns the workflow corpus, valid documents first, in the order the index
// lists them. The invalid ones carry RefusedBy, which says whether a document is refused
// by its shape or by a rule only the validator can reach.
func Workflows() ([]Case, error) { return read(func(i index) corpus { return i.Fixtures.Workflow }) }

// Bricks returns the brick manifest corpus, valid documents first, in the order the
// index lists them.
func Bricks() ([]Case, error) { return read(func(i index) corpus { return i.Fixtures.Brick }) }

// TaskMessages returns the task message corpus, which is what the controller publishes and what
// a runner reads. Nothing checked it until the bus was built and turned out to be putting a
// document on the queue that the schema refuses in sixteen places, one of which was the input
// envelopes' items.
func TaskMessages() ([]Case, error) {
	return read(func(i index) corpus { return i.Fixtures.TaskMessage })
}

// TaskResults returns the result corpus, which is what a runner sends back.
func TaskResults() ([]Case, error) {
	return read(func(i index) corpus { return i.Fixtures.TaskResult })
}

// GrantRedemptions returns the redemption corpus: what a runner sends to turn a grant into the
// values its task was given, and what the API answers, the repository tree among them. Each
// document is the pair, request and response, because neither half is legible without the other.
func GrantRedemptions() ([]Case, error) {
	return read(func(i index) corpus { return i.Fixtures.GrantRedemption })
}

// RunnerPools returns the runner pool corpus: a pool and the join token issued from it, which is
// what an administrator writes and what the API answers.
func RunnerPools() ([]Case, error) {
	return read(func(i index) corpus { return i.Fixtures.RunnerPool })
}

// Stops returns the stop corpus: what the controller publishes on agentiik.stops to have a task in
// flight stopped, and what a runner reads.
func Stops() ([]Case, error) {
	return read(func(i index) corpus { return i.Fixtures.Stop })
}

// RunnerHeartbeats returns the heartbeat corpus: what a runner says every ten seconds, and what the
// API answers, each document the pair, since every order the answer carries is about the keys the
// request listed.
func RunnerHeartbeats() ([]Case, error) {
	return read(func(i index) corpus { return i.Fixtures.RunnerHeartbeat })
}

// LogShipments returns the log shipment corpus: one chunk of a task's log as a runner ships it,
// and what the API answers, each document the pair, since the answer is where the runner stands
// after the chunk it sent.
func LogShipments() ([]Case, error) {
	return read(func(i index) corpus { return i.Fixtures.LogShipment })
}

// Wire is the schema document every message above is held to.
func Wire() ([]byte, error) { return fs.ReadFile(FS, "wire.schema.json") }

// read returns one corpus of the index as cases, and refuses an index that names a file
// the vendored tree does not carry: a corpus that has drifted from its index is a test
// that passes by reading less than it says it reads.
func read(pick func(index) corpus) ([]Case, error) {
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

	c := pick(idx)
	cases := make([]Case, 0, len(c.Valid)+len(c.Invalid))
	for _, e := range c.Valid {
		cases = append(cases, Case{
			File:     "fixtures/" + e.File,
			Valid:    true,
			Covers:   e.Covers,
			Manifest: manifestPath(e.Manifest),
			Role:     e.Role,
		})
	}
	for _, e := range c.Invalid {
		cases = append(cases, Case{
			File:      "fixtures/" + e.File,
			Rule:      e.Rule,
			RefusedBy: e.RefusedBy,
			Manifest:  manifestPath(e.Manifest),
			Role:      e.Role,
		})
	}
	for _, c := range cases {
		if _, err := fs.Stat(FS, c.File); err != nil {
			return nil, fmt.Errorf("the index names %s, which is not in the vendored tree: %w", c.File, err)
		}
		if c.Manifest == "" {
			continue
		}
		if _, err := fs.Stat(FS, c.Manifest); err != nil {
			return nil, fmt.Errorf("the index names the manifest %s, which is not in the vendored tree: %w", c.Manifest, err)
		}
	}
	return cases, nil
}

// manifestPath puts a manifest reference on the same footing as a fixture path, so that
// a caller reads both out of FS the same way.
func manifestPath(name string) string {
	if name == "" {
		return ""
	}
	return "fixtures/" + name
}

// RunnerRegistrations returns the join corpus: what a machine sends the API to become a runner, and
// what the API answers, each document the pair.
func RunnerRegistrations() ([]Case, error) {
	return read(func(i index) corpus { return i.Fixtures.RunnerRegistration })
}
