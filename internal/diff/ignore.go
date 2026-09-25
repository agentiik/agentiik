package diff

import (
	"fmt"
	"strings"
)

// Ignore is what a caller chose to hold aside.
//
// It is one value rather than a set of booleans so that agk brick test's --ignore flag is
// one flag, and so that a test can state in one place which facts it gave up. The four
// names are orthogonal: each one holds aside exactly the member it is named after, and
// nothing decides for a caller that two of them travel together.
type Ignore uint8

// The four facts that can be held aside. Each is a member of the envelope and is spelled
// the way the envelope spells it, because a person reading a difference is holding the
// document and not this package.
const (
	// IgnoreRunID holds aside meta.run_id: which run this was.
	IgnoreRunID Ignore = 1 << iota
	// IgnoreProducedAt holds aside meta.produced_at: when the port was published,
	// stamped by the runner and by agk.Concat.
	IgnoreProducedAt
	// IgnoreFileURIRun holds aside the run segment of every files[] uri. An artifact
	// is addressed agk://run/<run>/<step>/<port>/<name>, so the run identifier is
	// written there a second time; the step, the port and the name are still compared.
	IgnoreFileURIRun
	// IgnoreItemIDs holds aside items[].id. It is not in Default, because an item
	// holds its own identity so that a shard, a join and a replay can speak about it
	// after the batch has been split, concatenated or reordered, and a brick that
	// mints a fresh identifier on every pass has thrown that away. A caller that
	// names this one has said it is testing such a brick.
	IgnoreItemIDs
)

// Default is what a second run of the same thing differs by, by definition, and nothing
// else: meta.run_id, meta.produced_at and the run segment of every artifact URI. Item
// identities, item data, counts, port names, file names, media types, sizes and every
// sha256 are compared.
const Default = IgnoreRunID | IgnoreProducedAt | IgnoreFileURIRun

// names is the spelling of each flag, in the order they are written.
var names = []struct {
	flag Ignore
	name string
}{
	{IgnoreRunID, "meta.run_id"},
	{IgnoreProducedAt, "meta.produced_at"},
	{IgnoreFileURIRun, "files.uri"},
	{IgnoreItemIDs, "items.id"},
}

// has says whether one fact is held aside.
func (i Ignore) has(flag Ignore) bool { return i&flag != 0 }

// String writes what is held aside, as ParseIgnore reads it back.
//
// It is what a command prints as the default of its own flag, so that the usage text and
// the value cannot disagree: one place says which facts are given up and the other reads
// it.
func (i Ignore) String() string {
	if i == 0 {
		return "nothing"
	}
	var held []string
	for _, n := range names {
		if i.has(n.flag) {
			held = append(held, n.name)
		}
	}
	return strings.Join(held, ",")
}

// ParseIgnore reads the --ignore flag: the names, comma separated, in any order.
//
// An empty value holds nothing aside, which is a caller asking for the whole envelope to
// match including the run it came from. A name the vocabulary does not carry is refused
// naming what there is, because silently ignoring an ignore flag would make a test pass
// for a reason nobody asked for.
func ParseIgnore(s string) (Ignore, error) {
	var ig Ignore
	s = strings.TrimSpace(s)
	if s == "" || s == "nothing" {
		return 0, nil
	}
	for _, field := range strings.Split(s, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		found := false
		for _, n := range names {
			if field == n.name {
				ig |= n.flag
				found = true
			}
		}
		if !found {
			return 0, fmt.Errorf("%q is not a member an envelope carries: what can be held aside is %s", field, spellings())
		}
	}
	return ig, nil
}

// spellings lists every name, for the refusal and for a usage line.
func spellings() string {
	all := make([]string, 0, len(names))
	for _, n := range names {
		all = append(all, n.name)
	}
	return strings.Join(all, ", ")
}
