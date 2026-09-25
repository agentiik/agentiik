package diff

import (
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"slices"

	"github.com/agentiik/agentiik/agk"
)

// Envelopes compares what was expected against what was produced, port by port and item
// by item, with the facts ig names held aside.
//
// It answers with every difference it found rather than the first, because the two callers
// are a test harness and a proof: somebody reading why a brick no longer matches its
// fixture wants the three members that moved, not the first one three times over. The
// order is the order they would be looked for, ports in name order, then items in the
// order the batch carries them, then members in the order the document writes them.
//
// Nothing here validates. Both sides arrived through agk.Decode or out of a run, so the
// question is whether they are the same envelopes and not whether each is one.
func Envelopes(want, got map[agk.Port]agk.Envelope, ig Ignore) []Difference {
	var found []Difference
	ports := slices.Sorted(maps.Keys(want))
	for _, port := range slices.Sorted(maps.Keys(got)) {
		if _, ok := want[port]; !ok {
			ports = append(ports, port)
		}
	}
	slices.Sort(ports)

	for _, port := range ports {
		expected, declared := want[port]
		produced, published := got[port]
		switch {
		case declared && !published:
			found = append(found, Difference{Port: port, Want: phrase("an envelope"), Got: nil})
		case published && !declared:
			found = append(found, Difference{Port: port, Want: nil, Got: phrase(fmt.Sprintf("an envelope of %d items", len(produced.Items)))})
		default:
			found = append(found, envelope(port, expected, produced, ig)...)
		}
	}
	return found
}

// envelope compares one port: its metadata, then the batch it carries.
func envelope(port agk.Port, want, got agk.Envelope, ig Ignore) []Difference {
	var found []Difference
	at := func(member string, w, g any) {
		found = append(found, Difference{Port: port, Member: member, Want: w, Got: g})
	}

	// The metadata, in the order meta writes it. run_id and produced_at are the two
	// facts about which run this was rather than about what it produced, and they are
	// the two a caller holds aside.
	if !ig.has(IgnoreRunID) && want.Meta.RunID != got.Meta.RunID {
		at("meta.run_id", want.Meta.RunID, got.Meta.RunID)
	}
	if want.Meta.Step != got.Meta.Step {
		at("meta.step", want.Meta.Step, got.Meta.Step)
	}
	if want.Meta.Port != got.Meta.Port {
		at("meta.port", want.Meta.Port, got.Meta.Port)
	}
	if want.Meta.Attempt != got.Meta.Attempt {
		at("meta.attempt", want.Meta.Attempt, got.Meta.Attempt)
	}
	if want.Meta.Count != got.Meta.Count {
		at("meta.count", want.Meta.Count, got.Meta.Count)
	}
	if !ig.has(IgnoreProducedAt) && !want.Meta.ProducedAt.Equal(got.Meta.ProducedAt) {
		at("meta.produced_at", want.Meta.ProducedAt, got.Meta.ProducedAt)
	}

	// A batch of a different length already showed as meta.count, since count is the
	// number of items and an envelope that disagrees with its own count is refused
	// where it is read. It is said here too where the counts agreed, which is a value
	// built by hand rather than decoded.
	if len(want.Items) != len(got.Items) && want.Meta.Count == got.Meta.Count {
		at("items", phrase(fmt.Sprintf("%d items", len(want.Items))), phrase(fmt.Sprintf("%d items", len(got.Items))))
	}

	// Items are paired by rank. Order is part of what travels: wait_all concatenates in
	// edge declaration order and zip pairs by rank, so two envelopes carrying the same
	// items in a different order are not the same envelopes.
	for i := 0; i < min(len(want.Items), len(got.Items)); i++ {
		found = append(found, item(port, i, want.Items[i], got.Items[i], ig)...)
	}
	return found
}

// item compares one item of one port.
func item(port agk.Port, rank int, want, got agk.Item, ig Ignore) []Difference {
	// The handle a reader has is the identity in the document they are holding, which
	// is the expected one. Where identities are held aside there is no handle, so the
	// rank the item travels at is named instead.
	name := want.ID
	if ig.has(IgnoreItemIDs) || name == "" {
		name = fmt.Sprintf("#%d", rank+1)
	}
	var found []Difference
	at := func(member string, w, g any) {
		found = append(found, Difference{Port: port, Item: name, Member: member, Want: w, Got: g})
	}

	if !ig.has(IgnoreItemIDs) && want.ID != got.ID {
		at("id", want.ID, got.ID)
	}
	value("data", want.Data, got.Data, at)

	if len(want.Files) != len(got.Files) {
		at("files", phrase(fmt.Sprintf("%d files", len(want.Files))), phrase(fmt.Sprintf("%d files", len(got.Files))))
	}
	for i := 0; i < min(len(want.Files), len(got.Files)); i++ {
		file(want.Files[i], got.Files[i], i, ig, at)
	}
	return found
}

// file compares one attached artifact: the five members a files[] entry carries.
//
// The digest is compared because a comparison that skipped the artifacts would pass a run
// that produced different bytes, which is the one thing an attachment is there to say.
func file(want, got agk.File, rank int, ig Ignore, at func(string, any, any)) {
	// The entry is named by the file where both sides agree on the name, because that
	// is what a reader finds in the directory, and by its rank where they do not.
	where := fmt.Sprintf("files[%d]", rank)
	if want.Name == got.Name {
		where = "files[" + want.Name + "]"
	}
	if want.Name != got.Name {
		at(where+".name", want.Name, got.Name)
	}
	wantURI, gotURI := want.URI, got.URI
	if ig.has(IgnoreFileURIRun) {
		// An artifact is addressed agk://run/<run>/<step>/<port>/<name>, so the run
		// identifier is written here a second time. The step, the port and the name
		// are still compared: they say which port of which step produced the bytes.
		wantURI.Run, gotURI.Run = "", ""
	}
	if wantURI != gotURI {
		at(where+".uri", wantURI, gotURI)
	}
	if want.MediaType != got.MediaType {
		at(where+".media_type", want.MediaType, got.MediaType)
	}
	if want.Size != got.Size {
		at(where+".size", want.Size, got.Size)
	}
	if want.SHA256 != got.SHA256 {
		at(where+".sha256", want.SHA256, got.SHA256)
	}
}

// value compares one member of an item's data, naming the path it sits at.
//
// The path is the one the document writes, data.customer.vat_number and data.regions[2],
// so that a reader can find the line. A member whose shape changed is named at the shape
// and not at a leaf inside it: an object where a list was written is one difference and
// not one per element.
func value(path string, want, got any, at func(string, any, any)) {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			at(path, want, got)
			return
		}
		keys := slices.Sorted(maps.Keys(w))
		for _, key := range slices.Sorted(maps.Keys(g)) {
			if _, ok := w[key]; !ok {
				keys = append(keys, key)
			}
		}
		slices.Sort(keys)
		for _, key := range keys {
			value(path+"."+key, w[key], g[key], at)
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			at(path, want, got)
			return
		}
		if len(w) != len(g) {
			at(path, phrase(fmt.Sprintf("%d elements", len(w))), phrase(fmt.Sprintf("%d elements", len(g))))
		}
		for i := 0; i < min(len(w), len(g)); i++ {
			value(fmt.Sprintf("%s[%d]", path, i), w[i], g[i], at)
		}
	default:
		if !same(want, got) {
			at(path, want, got)
		}
	}
}

// same says whether two leaves are the same value.
//
// Numbers are compared by value and not by spelling. An envelope is decoded with the
// numbers kept as they were written, which is what keeps a large identifier exact, and 1
// and 1.0 are then two spellings of one number: a fixture written by hand should not have
// to guess which one the brick wrote.
func same(want, got any) bool {
	wn, wok := number(want)
	gn, gok := number(got)
	if wok && gok {
		return wn.Cmp(gn) == 0
	}
	if wok != gok {
		return false
	}
	return want == got
}

// number reads a JSON number out of a decoded value, whichever way the decoder kept it.
func number(v any) (*big.Rat, bool) {
	var text string
	switch n := v.(type) {
	case json.Number:
		text = n.String()
	case float64:
		text = fmt.Sprintf("%v", n)
	case int:
		text = fmt.Sprintf("%d", n)
	case int64:
		text = fmt.Sprintf("%d", n)
	default:
		return nil, false
	}
	r, ok := new(big.Rat).SetString(text)
	return r, ok
}

// phrase is a difference stated in words rather than as a value: the absence of a port,
// the length of a batch. It prints as it reads, where a value prints as the document
// writes it.
type phrase string
