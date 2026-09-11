package agk

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"sort"
	"strings"
	"time"

	"github.com/agentiik/agentiik/internal/ulid"
)

// What each object of the document carries, written out so that the refusal of an
// unknown or a missing member says what the document does define.
const (
	envelopeCarries = "an envelope carries meta and items, both required"
	metaCarries     = "meta carries exactly run_id, step, port, attempt, count and produced_at"
	itemCarries     = "an item carries id, data and files, all three required"
	fileCarries     = "an attached file carries name, uri, media_type, size and sha256, all five required"
)

// File is one artifact attached to an item: a file held in the object store, addressed
// by its digest and referenced by URI. The reference is what travels in the envelope;
// the bytes reach the next container as a mounted path.
type File struct {
	Name      string `json:"name"`
	URI       URI    `json:"uri"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
}

// Item is a single element of an envelope. It holds its own identity rather than being
// known by its rank, because a fan-out shards on it, a join matches on it and a replay
// speaks about it after the batch has been split, concatenated or reordered.
type Item struct {
	ID    string         `json:"id"`
	Data  map[string]any `json:"data"`
	Files []File         `json:"files"`
}

// NewItem mints an item around its payload. It is the one door an item identifier is
// minted through: an item that arrives without one cannot be spoken about downstream,
// and an identifier invented twice would make two items one.
func NewItem(data map[string]any) Item {
	return Item{ID: ulid.New(), Data: data, Files: []File{}}
}

// Meta says where a batch came from and how large it is. A task is self-contained and a
// runner never sees the graph it came from, so this travels with the data instead of
// being looked up.
type Meta struct {
	RunID      RunID     `json:"run_id"`
	Step       Step      `json:"step"`
	Port       Port      `json:"port"`
	Attempt    int       `json:"attempt"`
	Count      int       `json:"count"`
	ProducedAt time.Time `json:"produced_at"`
}

// Envelope is the document that travels along a port: metadata saying where the batch
// came from, and the items themselves. A port carries exactly one, published once when
// the emitting step ends.
//
// It is a closed document. An envelope that departs from it is refused whole rather
// than having the part nobody recognises ignored, so an emitter that adds a field of
// its own does not get the rest accepted.
type Envelope struct {
	Meta  Meta   `json:"meta"`
	Items []Item `json:"items"`
}

// Empty is what a declared output port publishes when the container never wrote it, and
// what every port of a skipped step publishes. It is a legitimate envelope and not an
// error: a downstream step reads a batch of nothing and decides its own fate.
func Empty(run RunID, step Step, port Port, attempt int, at time.Time) Envelope {
	return Envelope{
		Meta: Meta{
			RunID:      run,
			Step:       step,
			Port:       port,
			Attempt:    attempt,
			Count:      0,
			ProducedAt: at.UTC(),
		},
		Items: []Item{},
	}
}

// Decode reads one envelope and returns it only once every rule holds.
//
// Reading stops at envelope_max_bytes rather than at the end of the document: a batch
// larger than may travel is an application failure of the step that emitted it, and
// holding the whole of it in memory first would make the limit the one thing it cannot
// protect. The size the rule is measured on is the document as it arrived.
func Decode(r io.Reader, l Limits) (Envelope, error) {
	b, err := read(r, l.EnvelopeMaxBytes)
	if err != nil {
		return Envelope{}, err
	}
	var e Envelope
	if err := strictUnmarshal(b, &e); err != nil {
		if !errors.Is(err, ErrEnvelopeRejected) {
			err = reject("", "the envelope is not a JSON document: %s", err)
		}
		return Envelope{}, err
	}
	if err := e.validate(l, int64(len(b))); err != nil {
		return Envelope{}, err
	}
	return e, nil
}

// read takes the document in, refusing one longer than the limit allows. A limit that
// is zero or negative is not applied.
func read(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		b, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("reading the envelope: %w", err)
		}
		return b, nil
	}
	// One byte past the limit is all it takes to refuse, and no more than that is
	// taken. The guard is for a caller naming the largest limit there is, where one
	// more would wrap and stop the reading before it had begun.
	ceiling := limit
	if ceiling < math.MaxInt64 {
		ceiling++
	}
	b, err := io.ReadAll(io.LimitReader(r, ceiling))
	if err != nil {
		return nil, fmt.Errorf("reading the envelope: %w", err)
	}
	if int64(len(b)) > limit {
		// The step and the port are in the document that was not parsed, so the
		// refusal names neither. How much longer than the limit it is went
		// unread on purpose.
		return nil, &Refusal{
			Rule:    RuleEnvelopeMaxBytes,
			Outcome: Fail,
			Limit:   limit,
			Got:     int64(len(b)),
			Detail:  fmt.Sprintf("the serialised envelope is longer than the %d bytes the rule allows, and reading stopped there", limit),
		}
	}
	return b, nil
}

// Encode writes the envelope as one compact JSON document and returns how many bytes it
// wrote. No newline follows it, so what is written is exactly the serialised envelope
// envelope_max_bytes is measured on.
//
// It does not validate. Publication validates and then writes, and a caller that wants
// the bytes of a document it knows to be refused, to put it in front of a person, gets
// them.
func (e Envelope) Encode(w io.Writer) (int64, error) {
	b, err := e.marshal()
	if err != nil {
		return 0, err
	}
	n, err := w.Write(b)
	return int64(n), err
}

// Validate applies every rule the documentation states about an envelope: the shape
// first, because a document refused for its shape is refused whole, then the size rules
// in the order that names the most specific offender first.
func (e Envelope) Validate(l Limits) error { return e.validate(l, -1) }

// validate takes the serialised size where the caller has already measured it, and
// measures it itself when that is negative. Decode measures the document that arrived;
// everywhere else the canonical encoding is what would be published.
func (e Envelope) validate(l Limits, serialised int64) error {
	if err := e.Meta.validate(); err != nil {
		return under(err, "meta")
	}
	step, port := e.Meta.Step, e.Meta.Port

	// count is how many items the envelope holds, and it is what the controller
	// schedules on and what an expression weighs a batch by without the payload ever
	// being loaded. A count that disagrees with the items is therefore not a
	// cosmetic error: it is the number the graph acts on. JSON Schema cannot state
	// this, so the schema does not; the documentation does.
	if e.Meta.Count != len(e.Items) {
		return located(reject("meta.count", "count says %d and the envelope holds %d items: count is how many items the envelope holds", e.Meta.Count, len(e.Items)), step, port)
	}

	for i, it := range e.Items {
		if err := it.validate(); err != nil {
			return located(under(err, fmt.Sprintf("items[%d]", i)), step, port)
		}
	}

	if l.MaxItems > 0 && len(e.Items) > l.MaxItems {
		// The documentation gives max_items no outcome of its own. The reading
		// taken here is the one it gives its neighbour: beyond the limit a step
		// must paginate across several outputs or write a JSON Lines artifact,
		// which is a change to the step and so an application failure of it,
		// exactly as an envelope above envelope_max_bytes is.
		r := refuse(RuleMaxItems, Fail, step, port, "the envelope", int64(len(e.Items)), int64(l.MaxItems))
		r.Detail += "; beyond it a step paginates across several outputs or writes a JSON Lines artifact"
		return r
	}

	if l.InlineMaxBytes > 0 {
		for i, it := range e.Items {
			for _, k := range sortedKeys(it.Data) {
				where := fmt.Sprintf("items[%d].data.%s", i, k)
				path, n, over, err := heaviest(where, it.Data[k], l.InlineMaxBytes)
				if err != nil {
					return located(reject(where, "%s", err), step, port)
				}
				if over {
					r := refuse(RuleInlineMaxBytes, Reject, step, port, path, n, l.InlineMaxBytes)
					r.Detail += "; a value above it is written as an artifact and referenced in files[]"
					return r
				}
			}
		}
	}

	if l.EnvelopeMaxBytes > 0 {
		n := serialised
		if n < 0 {
			var err error
			// Measured on the document Encode would write, which is the one that
			// would be published: an envelope carrying nothing writes items as
			// the empty list and not as null, and MarshalJSON is where that
			// holds for every path that serialises one.
			if n, err = sizeOf(e); err != nil {
				return located(reject("", "the envelope cannot be serialised: %s", err), step, port)
			}
		}
		if n > l.EnvelopeMaxBytes {
			return refuse(RuleEnvelopeMaxBytes, Fail, step, port, "the serialised envelope", n, l.EnvelopeMaxBytes)
		}
	}
	return nil
}

// Concat joins the envelopes of one port. A step split into shards has its shard
// envelopes concatenated port by port before publication, which is why there is no
// shard field in an envelope: what is published describes the step and not one of its
// shards.
//
// Items keep their order and their identifiers: the shards are in the order they were
// split into, and an item that came back from a container is the item that went into
// it. The metadata is rebuilt, and the size rules are measured on the result rather
// than on any shard, because the result is what travels.
func Concat(shards []Envelope, l Limits) (Envelope, error) {
	if len(shards) == 0 {
		return Envelope{}, reject("shards", "a port is published from at least one shard, and there is nothing here to publish")
	}

	first := shards[0].Meta
	out := Envelope{
		Meta: Meta{
			RunID:      first.RunID,
			Step:       first.Step,
			Port:       first.Port,
			Attempt:    first.Attempt,
			ProducedAt: first.ProducedAt.UTC(),
		},
		Items: []Item{},
	}
	for n, s := range shards {
		switch {
		case s.Meta.RunID != first.RunID:
			return Envelope{}, reject(fmt.Sprintf("shards[%d].meta.run_id", n), "the shards of one port belong to one run: shards[0] carries %q and this one carries %q", first.RunID, s.Meta.RunID)
		case s.Meta.Step != first.Step:
			return Envelope{}, reject(fmt.Sprintf("shards[%d].meta.step", n), "the shards of one port belong to one step: shards[0] carries %q and this one carries %q", first.Step, s.Meta.Step)
		case s.Meta.Port != first.Port:
			return Envelope{}, reject(fmt.Sprintf("shards[%d].meta.port", n), "shard envelopes are concatenated port by port: shards[0] carries %q and this one carries %q", first.Port, s.Meta.Port)
		}
		// Two readings the documentation does not state, taken so that the
		// rebuilt metadata cannot say less than the shards did. A shard that was
		// retried carries a higher attempt than its siblings, and the published
		// batch is attributed to the highest any shard needed. The port is
		// published when the step ends, which is when the last shard ended.
		if s.Meta.Attempt > out.Meta.Attempt {
			out.Meta.Attempt = s.Meta.Attempt
		}
		if s.Meta.ProducedAt.After(out.Meta.ProducedAt) {
			out.Meta.ProducedAt = s.Meta.ProducedAt.UTC()
		}
		out.Items = append(out.Items, s.Items...)
	}
	out.Meta.Count = len(out.Items)

	if err := out.validate(l, -1); err != nil {
		return Envelope{}, err
	}
	return out, nil
}

// Split cuts an envelope into shards of at most size items, in order, for a fan-out to
// hand one container each.
//
// Identifiers are untouched. An item keeps the identity the port that produced it gave
// it, through the split and through the merge that follows, so that a shard, a replay
// and a run inspector speak about the same element by the same name.
//
// An envelope holding no items splits into no shards, which is a fan-out over an empty
// batch starting no containers. Concat is the inverse of Split for every envelope that
// holds at least one item.
func Split(e Envelope, size int) ([]Envelope, error) {
	if size < 1 {
		return nil, fmt.Errorf("a shard holds at least one item, so size is 1 or more and not %d", size)
	}
	var out []Envelope
	for start := 0; start < len(e.Items); start += size {
		end := min(start+size, len(e.Items))
		items := make([]Item, end-start)
		copy(items, e.Items[start:end])
		m := e.Meta
		m.Count = len(items)
		out = append(out, Envelope{Meta: m, Items: items})
	}
	return out, nil
}

// validate applies the value rules of the metadata. Presence and type are settled when
// the document is read; what is left here is what the values themselves have to be.
func (m Meta) validate() error {
	if err := m.RunID.Validate(); err != nil {
		return reject("run_id", "%s", err)
	}
	if err := m.Step.Validate(); err != nil {
		return reject("step", "%s", err)
	}
	if err := m.Port.Validate(); err != nil {
		return reject("port", "%s", err)
	}
	if m.Attempt < 1 {
		return reject("attempt", "attempts count from 1 and this one says %d: the attempt number is what separates the batch that was kept from the ones that failed before it", m.Attempt)
	}
	if m.Count < 0 {
		return reject("count", "an item count is never negative and this one says %d", m.Count)
	}
	if m.ProducedAt.IsZero() {
		return reject("produced_at", "an envelope says when it was published: it is what orders batches across retries and replays")
	}
	return nil
}

func (i Item) validate() error {
	if i.ID == "" {
		return reject("id", "an item's identifier is never empty: it is what a shard, a replay and a run inspector speak about the same element by once the batch has been split, concatenated or reordered")
	}
	for n, f := range i.Files {
		if err := f.Validate(); err != nil {
			return under(err, fmt.Sprintf("files[%d]", n))
		}
	}
	return nil
}

// Validate applies every rule an attached file entry is held to.
//
// It is exported because a file entry is read outside an envelope as well as inside
// one: the store resolves one, and the mount lays one down under a name. Both are the
// same rules, and a second statement of them somewhere else is a second answer to what
// a file entry is.
func (f File) Validate() error {
	if err := segment(f.Name); err != nil {
		return reject("name", "%s", err)
	}
	if err := f.URI.validate(); err != nil {
		return reject("uri", "%s", err)
	}
	if f.Name != f.URI.Name {
		return reject("name", "the name is %q and the URI ends on %q: a file's name is the last segment of its URI", f.Name, f.URI.Name)
	}
	if err := mediaType(f.MediaType); err != nil {
		return reject("media_type", "%s", err)
	}
	if f.Size < 0 {
		return reject("size", "a size is a whole number of bytes and this one says %d", f.Size)
	}
	// Size is not measured against artifact_max_bytes. That setting caps what may
	// be written to the store, where it is applied, and the documentation says in
	// as many words that it is not a bound on this field: an envelope may name an
	// artifact written under a ceiling that has since been lowered.
	if err := digest(f.SHA256); err != nil {
		return reject("sha256", "%s", err)
	}
	return nil
}

// mediaType holds a media type to being one. A consumer reads what the bytes are from
// this rather than from the ending of the name, and it is the member a brick writing
// its own envelope is most likely to leave out, so a value that is not a media type is
// worth refusing rather than carrying.
//
// The standard library parses it, which is what accepts the parameters a media type may
// carry, and the type and subtype are checked here: mime.ParseMediaType also parses a
// Content-Disposition, so it accepts a bare token where an envelope must not.
func mediaType(s string) error {
	t, _, err := mime.ParseMediaType(s)
	if err != nil {
		return fmt.Errorf("%q is not a media type: %s", s, err)
	}
	kind, sub, ok := strings.Cut(t, "/")
	if !ok || kind == "" || sub == "" {
		return fmt.Errorf("%q is not a media type: one is a type and a subtype, as application/pdf", s)
	}
	return nil
}

// digest holds the digest to its written form. It is the artifact's address as well as
// its checksum: the <digest> the physical key sha256/<digest> is built from, so an
// elided or truncated one addresses nothing.
func digest(s string) error {
	if len(s) != 64 {
		return fmt.Errorf("%q is not a digest: one travels in full, as sixty-four lowercase hexadecimal characters, and this is %d", s, len(s))
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return fmt.Errorf("%q is not a digest: one is sixty-four lowercase hexadecimal characters, and this carries %q", s, string(c))
		}
	}
	return nil
}

// UnmarshalJSON reads an envelope, refusing a member the document does not define and a
// member it requires and does not find.
func (e *Envelope) UnmarshalJSON(b []byte) error {
	m, err := members(b, "an envelope", envelopeCarries, "meta", "items")
	if err != nil {
		return err
	}
	var out Envelope

	raw, err := member(m, "meta", envelopeCarries)
	if err != nil {
		return err
	}
	if err := into(raw, "meta", &out.Meta, metaCarries); err != nil {
		return err
	}

	// From here the metadata is read, so a refusal can name the step and the port
	// it happened on even though the items are what is being read.
	raw, err = member(m, "items", envelopeCarries)
	if err != nil {
		return located(err, out.Meta.Step, out.Meta.Port)
	}
	var items []json.RawMessage
	if err := strictUnmarshal(raw, &items); err != nil {
		return located(reject("items", "the items are a list, in order: order is part of the contract, since wait_all concatenates in edge declaration order and zip pairs by rank"), out.Meta.Step, out.Meta.Port)
	}
	if items == nil {
		return located(reject("items", "the items are a list, and a port that carried nothing carries an empty one"), out.Meta.Step, out.Meta.Port)
	}
	out.Items = make([]Item, len(items))
	for i, raw := range items {
		if err := out.Items[i].UnmarshalJSON(raw); err != nil {
			return located(under(err, fmt.Sprintf("items[%d]", i)), out.Meta.Step, out.Meta.Port)
		}
	}

	*e = out
	return nil
}

// UnmarshalJSON reads the metadata.
func (m *Meta) UnmarshalJSON(b []byte) error {
	fields, err := members(b, "the metadata", metaCarries, "run_id", "step", "port", "attempt", "count", "produced_at")
	if err != nil {
		return err
	}
	var out Meta
	for _, f := range []struct {
		key  string
		into any
		why  string
	}{
		{"run_id", &out.RunID, "a run identifier is a string, the run's ULID"},
		{"step", &out.Step, "a step is named by a string"},
		{"port", &out.Port, "a port is named by a string"},
		{"attempt", &out.Attempt, "an attempt is a whole number, counting from 1"},
		{"count", &out.Count, "a count is a whole number of items"},
		{"produced_at", &out.ProducedAt, "a publication time is an RFC 3339 timestamp, as 2026-09-10T06:00:12.418Z"},
	} {
		raw, err := member(fields, f.key, metaCarries)
		if err != nil {
			return err
		}
		if err := into(raw, f.key, f.into, f.why); err != nil {
			return err
		}
	}
	*m = out
	return nil
}

// UnmarshalJSON reads one item.
func (i *Item) UnmarshalJSON(b []byte) error {
	fields, err := members(b, "an item", itemCarries, "id", "data", "files")
	if err != nil {
		return err
	}
	var out Item

	raw, err := member(fields, "id", itemCarries)
	if err != nil {
		return err
	}
	if err := into(raw, "id", &out.ID, "an identifier is a string"); err != nil {
		return err
	}

	raw, err = member(fields, "data", itemCarries)
	if err != nil {
		return err
	}
	const dataIsAnObject = "an item's data is an object, never a scalar and never an array, so an item that is a single value carries it under a name"
	if err := into(raw, "data", &out.Data, dataIsAnObject); err != nil {
		return err
	}
	if out.Data == nil {
		return reject("data", "%s", dataIsAnObject)
	}

	raw, err = member(fields, "files", itemCarries)
	if err != nil {
		return err
	}
	const filesIsAList = "an item's files are a list, and an item with nothing attached carries an empty one"
	var files []json.RawMessage
	if err := into(raw, "files", &files, filesIsAList); err != nil {
		return err
	}
	if files == nil {
		return reject("files", "%s", filesIsAList)
	}
	out.Files = make([]File, len(files))
	for n, raw := range files {
		if err := out.Files[n].UnmarshalJSON(raw); err != nil {
			return under(err, fmt.Sprintf("files[%d]", n))
		}
	}

	*i = out
	return nil
}

// UnmarshalJSON reads one attached file.
func (f *File) UnmarshalJSON(b []byte) error {
	fields, err := members(b, "an attached file", fileCarries, "name", "uri", "media_type", "size", "sha256")
	if err != nil {
		return err
	}
	var out File
	for _, d := range []struct {
		key  string
		into any
		why  string
	}{
		{"name", &out.Name, "a name is a string"},
		{"uri", &out.URI, "an artifact URI is a string, written " + uriForm},
		{"media_type", &out.MediaType, "a media type is a string, as application/pdf"},
		{"size", &out.Size, "a size is a whole number of bytes"},
		{"sha256", &out.SHA256, "a digest is a string of sixty-four lowercase hexadecimal characters"},
	} {
		raw, err := member(fields, d.key, fileCarries)
		if err != nil {
			return err
		}
		if err := into(raw, d.key, d.into, d.why); err != nil {
			return err
		}
	}
	*f = out
	return nil
}

// MarshalJSON writes an envelope, giving items its empty form rather than null. An
// envelope that carried nothing carries an empty list, and it is written here rather
// than only where a document is published so that envelope_max_bytes is measured on the
// document that would travel: null and [] are not the same length, and a rule measured
// on a form nothing publishes is measured on the wrong thing.
func (e Envelope) MarshalJSON() ([]byte, error) {
	if e.Items == nil {
		e.Items = []Item{}
	}
	type envelope Envelope // without the method, so that this does not call itself
	return marshal(envelope(e))
}

// MarshalJSON writes an item, giving data and files their empty forms rather than null.
// Leaving either key out is refused, and null is not the empty list: a consumer should
// never have to tell absent from empty.
func (i Item) MarshalJSON() ([]byte, error) {
	if i.Data == nil {
		i.Data = map[string]any{}
	}
	if i.Files == nil {
		i.Files = []File{}
	}
	type item Item // without the method, so that this does not call itself
	return marshal(item(i))
}

// members splits a JSON object into its members and refuses any the document does not
// define. The envelope is a closed document: a member nobody recognises refuses the
// whole rather than being carried or dropped.
func members(b []byte, what, carries string, defined ...string) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := strictUnmarshal(b, &m); err != nil || m == nil {
		return nil, reject("", "%s is a JSON object: %s", what, carries)
	}
	known := make(map[string]bool, len(defined))
	for _, d := range defined {
		known[d] = true
	}
	var unknown []string
	for k := range m {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, reject(unknown[0], "not a member of the document: %s", carries)
	}
	return m, nil
}

// member takes one required member, or says which one is missing.
func member(m map[string]json.RawMessage, key, carries string) (json.RawMessage, error) {
	raw, ok := m[key]
	if !ok {
		return nil, reject(key, "required: %s", carries)
	}
	return raw, nil
}

// into reads one member, saying what the member is when what is written is not it.
func into(raw json.RawMessage, key string, v any, why string) error {
	if err := strictUnmarshal(raw, v); err != nil {
		var r *rejection
		if errors.As(err, &r) {
			return under(err, key)
		}
		return reject(key, "%s", why)
	}
	return nil
}

// strictUnmarshal reads one JSON value and nothing after it, refusing a member no
// struct defines and holding every number as it was written.
//
// The two settings are named on every path rather than once at the top: a type that
// reads itself, which is how an envelope names the member a refusal is about, is
// handed the raw bytes and would otherwise lose both.
func strictUnmarshal(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return fmt.Errorf("a second document follows the first")
	}
	return nil
}

// under says where in the document a refusal was found, one level at a time as it comes
// back up: files[0].sha256, then items[1].files[0].sha256.
func under(err error, where string) error {
	var r *rejection
	if errors.As(err, &r) {
		if r.what == "" {
			r.what = where
		} else {
			r.what = where + "." + r.what
		}
	}
	return err
}

// EncodeValue writes one value of an item's data the way it travels inside the
// envelope: compact JSON, with no HTML escaping, since a payload is bytes on a port and
// never a fragment of a page.
//
// It is what inline_max_bytes is measured on, and it is exported so that a caller
// moving a value above the threshold into the store writes exactly the bytes that were
// weighed. Measuring one encoding and storing another would put the threshold and the
// artifact out of step, and the value would come back weighing something else.
func EncodeValue(v any) ([]byte, error) { return marshal(v) }

// marshal writes one value as compact JSON, without the newline the encoder ends a
// document with, so that what is written is what the size rules are measured on.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

func (e Envelope) marshal() ([]byte, error) { return marshal(e) }

// sizeOf measures the serialised form of a value without holding it, which is what lets
// a size rule be applied to a whole envelope and to every value inside it.
func sizeOf(v any) (int64, error) {
	var c counter
	enc := json.NewEncoder(&c)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return 0, err
	}
	return c.n - 1, nil // less the newline the encoder ends a document with
}

type counter struct{ n int64 }

func (c *counter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// heaviest returns the most specific value at or under v whose serialised form is above
// the limit, and the path that names it.
//
// The most specific one is the point. A step told that items[3].data.report is too
// heavy knows what to write as an artifact; a step told that its item is too heavy has
// to go and find out which of its fields the engine meant.
func heaviest(path string, v any, limit int64) (string, int64, bool, error) {
	n, err := sizeOf(v)
	if err != nil {
		return "", 0, false, err
	}
	if n <= limit {
		return "", 0, false, nil
	}
	switch t := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(t) {
			if p, s, over, err := heaviest(path+"."+k, t[k], limit); err != nil || over {
				return p, s, over, err
			}
		}
	case []any:
		for i, c := range t {
			if p, s, over, err := heaviest(fmt.Sprintf("%s[%d]", path, i), c, limit); err != nil || over {
				return p, s, over, err
			}
		}
	}
	return path, n, true, nil
}

// sortedKeys keeps a refusal reading the same way twice: a map is walked in whatever
// order the runtime feels like, and an error that names a different field on each run
// is an error nobody trusts.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
