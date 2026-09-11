package brick

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
)

// spilledMediaType is what a spilled value is written as. The artifact holds the JSON
// encoding of the value that left data, so the media type says JSON whatever the value
// was: a consumer reads the bytes back with a JSON decoder and gets what the field held.
const spilledMediaType = "application/json"

// spilledSuffix ends the name of a spilled artifact, so that what a field became is
// legible in a directory listing of a mount.
const spilledSuffix = ".json"

// maxFieldSegment caps the part of a spilled name taken from the field, leaving room
// under the two hundred and fifty-five bytes a file name usually has for the item
// identifier and the suffix.
const maxFieldSegment = 64

// Spill writes any value of data heavier than inline_max_bytes to the store and replaces
// it with a files[] entry.
//
// Above inline_max_bytes a value must be written as an artifact and referenced in
// files[], and the runner rejects an envelope that carries one inline. This is the other
// half of that threshold: where the runner refuses what a container emitted, Spill moves
// a value the engine itself constructed, so that the engine never emits an envelope its
// own rule would reject.
//
// The unit is a top level field of data, because that is the unit a port's schema names
// and the unit a consumer asks for by name. agk refuses at a finer grain, naming the most
// specific value above the threshold, so that a step is told which of its fields to
// write as an artifact; what is moved here is the field carrying that value, and the
// envelope that comes back carries nothing above the threshold either way.
//
// What is measured is the JSON encoding of the value, which is exactly what would have
// travelled inline, and it is those same bytes that are stored. The field leaves data
// and the artifact arrives in files[] of the same item, so the item keeps its identity
// and its rank.
//
// Items and files already carried are left as they are: a files[] entry is already a
// reference, and nothing is spilled twice.
func Spill(ctx context.Context, s *artifact.Store, e agk.Envelope, l agk.Limits) (agk.Envelope, error) {
	if s == nil {
		return agk.Envelope{}, errors.New("brick: no artifact store: a value above the threshold is written to one")
	}
	if l.InlineMaxBytes <= 0 {
		// agk reads a limit of zero or less as a rule deliberately turned off, and the
		// rule turned off here means no value leaves data, rather than every value
		// leaving it. An engine passes agk.DefaultLimits or the namespace's own.
		return e, nil
	}

	// One name per port, because the mount lays every artifact of a port side by side in
	// one directory and agk://run/<run>/<step>/<port>/<name> addresses one of them. The
	// names already carried are spoken for too: a spilled field landing on one of them
	// would make the port hold two artifacts under one name, which is what the mount
	// below then refuses to lay down.
	taken := map[string]bool{envelopeFileName: true}
	for _, item := range e.Items {
		for _, file := range item.Files {
			taken[file.Name] = true
		}
	}
	items := slices.Clone(e.Items)
	for i, item := range items {
		var spilled []agk.File
		data := item.Data
		copied := false
		for _, field := range slices.Sorted(maps.Keys(item.Data)) {
			// The encoding agk weighs the value by, and the bytes stored are those
			// same ones: measuring one thing and storing another would put the
			// threshold and the artifact out of step.
			encoded, err := agk.EncodeValue(item.Data[field])
			if err != nil {
				return agk.Envelope{}, fmt.Errorf("brick: step %s: port %s: item %s: field %s: %w", e.Meta.Step, e.Meta.Port, item.ID, field, err)
			}
			if int64(len(encoded)) <= l.InlineMaxBytes {
				continue
			}
			uri := agk.URI{
				Run:  e.Meta.RunID,
				Step: e.Meta.Step,
				Port: e.Meta.Port,
				Name: spilledName(item, i, field, taken),
			}
			file, err := s.Put(ctx, uri, spilledMediaType, bytes.NewReader(encoded))
			if err != nil {
				return agk.Envelope{}, err
			}
			if !copied {
				// Copied only once something actually spills, so that an envelope with
				// nothing to spill comes back sharing the maps it arrived with, and the
				// map the caller passed in is never emptied underneath it.
				data = maps.Clone(item.Data)
				copied = true
			}
			delete(data, field)
			spilled = append(spilled, file)
		}
		if len(spilled) == 0 {
			continue
		}
		items[i].Data = data
		items[i].Files = append(slices.Clone(item.Files), spilled...)
	}

	e.Items = items
	return e, nil
}

// spilledName builds the name a spilled field travels under.
//
// It carries the item's identifier because two items of one envelope may spill the same
// field, and the port's directory holds one file per name. It carries the field so that
// what the artifact holds can be read off the name, and it is filed down to what a name
// may be: a field of data is any JSON string, and a name is one path segment.
func spilledName(item agk.Item, rank int, field string, taken map[string]bool) string {
	// The identifier, not the rank, because an item is known by its identity after the
	// batch has been split, concatenated or reordered. The rank stands in only for an
	// item whose identifier has not been minted yet.
	owner := item.ID
	if owner == "" {
		owner = fmt.Sprintf("item-%d", rank)
	}
	base := fileSegment(owner) + "-" + fileSegment(field)
	name := base + spilledSuffix
	for n := 2; taken[name]; n++ {
		// Two fields filed down to the same segment, which two keys spelt a/b and a_b
		// would do. The order fields are visited in is sorted, so the suffix a field
		// receives is the same on every run.
		name = fmt.Sprintf("%s-%d%s", base, n, spilledSuffix)
	}
	taken[name] = true
	return name
}

// fileSegment files a string down to what may be one segment of a path and of a URI,
// which is what a field of data, being any JSON string, is not.
func fileSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= maxFieldSegment {
			break
		}
	}
	out := b.String()
	if strings.Trim(out, ".") == "" {
		// A field spelt "" or "." or ".." leaves nothing a file can be called.
		out = "field"
	}
	return out
}
