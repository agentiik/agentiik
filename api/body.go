package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
)

// What reading a request body costs, and what bounds it.
//
// Decoding JSON into Go values costs far more than the JSON weighs, because every value becomes
// something of its own: a string is a header of sixteen bytes before its bytes, an entry of a map
// is a slot of forty or more, an element of an []any is an interface and whatever it boxes, and
// three bytes of {} are a map. So what a body cost to decode was set by how many values it held
// rather than by how many bytes, and a body of many small values cost up to sixty times itself:
// 295 MiB for a push of 16 MiB of empty tree entries, 508 MiB for 8 MiB of inputs written
// [{},{},...], 274 MiB for 8 MiB of empty labels sent to the join, which answers anybody. A
// handful of such requests at once was an instance at its memory limit.
//
// So a body is read here one token at a time, and never into anything generic that could hold
// more than the route reads. Each request type reads its own fields, every collection is counted
// as its entries are read and refused at the first one past what the route takes, and a document
// whose shape is the caller's, as the inputs of a run are, is counted value by value. What is left
// is the body itself and the values the route keeps, whose number is bounded, and body_test.go
// measures both for every route, beside the numbers they replaced.
//
// Every route also has a cap of its own on the bytes, with its reason beside it where the request
// type is declared, rather than one cap that fits the largest body and is ten thousand times what
// most of them carry.

// smallMaxBytes is how large the body of a route that takes a few names and numbers may be: a
// runner pool, a join token, a join, a redemption and a request for a bus credential.
//
// Each of those is a few hundred bytes, and sixty-four kibibytes is a hundred times the largest of
// them: room for whatever they come to carry, and no room for a body that costs anything to read.
// The join is the one route that reads JSON from somebody nobody has authenticated, so what one of
// its bodies costs is what anybody on the network can make the API spend.
const smallMaxBytes = 64 << 10

// namesMax is how many entries a list of names in one of those bodies may hold: the labels of a
// pool, a token or a machine, and the namespaces a pool accepts.
//
// A label is something a step selects a runner by, zone=dmz or arch=arm64, and a machine is
// described by a handful of them, as a pool that restricts its namespaces lists a handful. The
// count is what bounds reading one, since an entry costs a header of sixteen bytes however short
// it is and an empty one is three bytes on the wire: at smallMaxBytes, 1024 of them is sixty-four
// bytes a name.
const namesMax = 1024

// request is a body a route reads, one field at a time.
type request interface {
	// field reads the value of the member called name, and refuses a name the route does not
	// read. It is called at most once per name, since a member written twice is refused
	// before it is reached.
	field(b *body, name string) error
}

// body is a request body being read.
type body struct {
	raw []byte
	d   *jsontext.Decoder

	// values is how many more values document may count, for a field whose shape is its
	// caller's. Everything else has a shape of its own, and a count of its own where it is a
	// collection.
	values int
}

// tooLarge is a body holding more than its route reads: more bytes than its cap, more entries in
// a collection or more values in a document than the route takes. All three are answered 413,
// because they are one refusal: the request is larger than the route is willing to read, rather
// than wrong. A body over its cap in bytes also wraps *http.MaxBytesError, which is how a route
// with a sentence of its own for that tells it apart.
type tooLarge struct {
	reason string
	bytes  *http.MaxBytesError
}

func (e *tooLarge) Error() string { return e.reason }

func (e *tooLarge) Unwrap() error {
	if e.bytes == nil {
		return nil
	}
	return e.bytes
}

// statusOf is how a body readAtMost refused is answered: 413 where it held more than its route
// reads, and 400 where it was not what the route reads at all.
func statusOf(err error) int {
	if errors.As(err, new(*tooLarge)) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

// readAtMost reads a body into a request, closed and at most limit bytes long.
//
// Closed: a member the request does not read is refused rather than half understood, one written
// twice is refused rather than decided by whichever came last, and so is anything after the
// document. A decoder reads one document and stops, so a second one would otherwise be accepted
// and dropped, and a declaration followed by a value would tell whoever sent it the value had been
// kept. The refusal does not repeat what followed, which may be that value.
//
// A name written twice is found by the request, as a field or as a path, rather than by the
// decoder, which would find it by keeping every name of every object it reads, a map of them for
// a large one: the inputs of a run written as one object of a hundred thousand members cost four
// times their body that way. What the API keeps without reading, as it keeps the inputs, it keeps
// as written, and a name written twice there is decided by whoever reads it, by the last as
// encoding/json and PostgreSQL both decide it.
func readAtMost(r *http.Request, into request, limit int64) error {
	raw, err := slurp(r, limit)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return errors.New("the request body is empty, and this route reads a JSON object")
	}
	b := &body{raw: raw, d: jsontext.NewDecoder(bytes.NewBuffer(raw), jsontext.AllowDuplicateNames(true))}
	if err := b.fields(into); err != nil {
		return err
	}
	if _, err := b.d.ReadToken(); err != io.EOF {
		return errors.New("the request body is one JSON document, and this one carries something after it")
	}
	return nil
}

// slurpFirstBytes is the most a body is given to arrive into before any of it has: the size of
// the buffer net/http already reads each connection through, so that a caller who sends headers
// and nothing after them holds no more than the connection already does.
const slurpFirstBytes = 4 << 10

// slurpGrowth is how many times larger each buffer a body grows into is than the one it has
// filled, which bounds what it holds at that many times what has arrived.
//
// Four, because what the buffers cost in all is the body and a third of it again, where doubling
// would cost the body twice over: the one large file of a push is read, and decoded once, for a
// little over twice the push, and doubling would make that nearly three times.
const slurpGrowth = 4

// slurp reads a body whole, and refuses one longer than limit.
//
// Whole, and before any of it is decoded, because a decoder reading from the connection holds the
// value it is in the middle of in a buffer that doubles as the value grows, and leaves each smaller
// one behind it: the one large file of a push cost twice its size in buffers before a byte of it
// was decoded. Read whole, the decoder reads the body where it lies, and a string is copied once,
// into whatever keeps it.
//
// A body declaring more than the limit is refused before a byte of it is read. Every other body is
// held as it arrives rather than as it declares: a length is only what its caller says, and one
// allocated before its bytes came let a caller who declared a push and sent nothing hold 16 MiB
// for a few hundred bytes of headers, until it hung up. So a body starts in at most
// slurpFirstBytes and grows slurpGrowth times at each buffer it fills, through sizes chosen so
// that the last is exactly the length it declares, or the limit where it declares none.
func slurp(r *http.Request, limit int64) ([]byte, error) {
	larger := func() error {
		return &tooLarge{
			reason: fmt.Sprintf("the request body is larger than the %d bytes this route reads", limit),
			bytes:  &http.MaxBytesError{Limit: limit},
		}
	}
	if r.ContentLength > limit {
		return nil, larger()
	}
	// Why a body could not be read whole: it went past the limit, which only one that declared
	// no length can, or it was cut short.
	unread := func(err error) error {
		switch {
		case err == nil || errors.As(err, new(*http.MaxBytesError)):
			return larger()
		case r.ContentLength >= 0:
			return fmt.Errorf("the request body ends before the %d bytes it declares", r.ContentLength)
		}
		return fmt.Errorf("the request body could not be read: %w", err)
	}

	most := limit
	if r.ContentLength >= 0 {
		most = r.ContentLength
	}
	// most divided by slurpGrowth as often as it takes to fit slurpFirstBytes, rounded up, so
	// that multiplying it back reaches most in as many steps and no buffer is larger than it
	// needs to be.
	first := most
	for first > slurpFirstBytes {
		first = (first + slurpGrowth - 1) / slurpGrowth
	}

	in := http.MaxBytesReader(nil, r.Body, limit)
	raw := make([]byte, 0, first)
	for {
		if len(raw) == cap(raw) {
			if int64(len(raw)) == most {
				break
			}
			raw = append(make([]byte, 0, min(most, int64(cap(raw))*slurpGrowth)), raw...)
		}
		n, err := in.Read(raw[len(raw):cap(raw)])
		raw = raw[:len(raw)+n]
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, unread(err)
		}
	}
	if r.ContentLength < 0 && int64(len(raw)) == limit {
		// Read to the limit with no end yet: one byte further tells a body of exactly the limit
		// from a longer one, whose byte past it MaxBytesReader answers with its error, without a
		// buffer one byte larger than the limit, which would round up to the allocator's next
		// size.
		if _, err := io.ReadFull(in, make([]byte, 1)); err != io.EOF {
			return nil, unread(err)
		}
	}
	if int64(len(raw)) < r.ContentLength {
		return nil, unread(io.ErrUnexpectedEOF)
	}
	return raw, nil
}

// object reads an object one member at a time, and refuses the member after the most-th with
// tooMany where most is not negative. Null is an object with no members, as encoding/json reads it.
func (b *body) object(most int, tooMany string, each func(name string) error) error {
	t, err := b.d.ReadToken()
	if err != nil {
		return malformed(err)
	}
	switch t.Kind() {
	case jsontext.KindNull:
		return nil
	case jsontext.KindBeginObject:
	default:
		return b.mistyped(t.Kind(), "an object")
	}
	for n := 0; b.d.PeekKind() != jsontext.KindEndObject; n++ {
		// The name is read before the count is held to, so that what follows the last member
		// the route takes is refused as too many only where it is a member.
		name, err := b.d.ReadToken()
		if err != nil {
			return malformed(err)
		}
		if n == most {
			return &tooLarge{reason: tooMany}
		}
		if err := each(name.String()); err != nil {
			return err
		}
	}
	_, err = b.d.ReadToken()
	return malformed(err)
}

// fields reads an object whose members are the fields of a request, and refuses a field written
// twice. The names are the request's own and few, since a name it does not read is refused at
// once, so they are looked for among those already read rather than kept in a map.
func (b *body) fields(into request) error {
	var read []string
	return b.object(-1, "", func(name string) error {
		if slices.Contains(read, name) {
			return fmt.Errorf("the request body writes %.64q twice, and a field written twice is refused rather than decided by whichever came last", name)
		}
		if err := into.field(b, name); err != nil {
			return err
		}
		read = append(read, name)
		return nil
	})
}

// twice refuses a name a collection already holds, the path of a file in a tree for one.
func twice(what, name string) error {
	return fmt.Errorf("the request body names %s %.64q twice, and one written twice is refused rather than decided by whichever came last", what, name)
}

// text reads a string. Null leaves it as it was.
func text[T ~string](b *body, into *T) error {
	t, err := b.d.ReadToken()
	if err != nil {
		return malformed(err)
	}
	switch t.Kind() {
	case jsontext.KindNull:
		return nil
	case jsontext.KindString:
		*into = T(t.String())
		return nil
	}
	return b.mistyped(t.Kind(), "a string")
}

// texts reads an array of strings, and refuses the entry after the most-th with tooMany. Null is
// nil.
func texts[T ~string](b *body, into *[]T, most int, tooMany string) error {
	t, err := b.d.ReadToken()
	if err != nil {
		return malformed(err)
	}
	switch t.Kind() {
	case jsontext.KindNull:
		return nil
	case jsontext.KindBeginArray:
	default:
		return b.mistyped(t.Kind(), "an array of strings")
	}
	out := []T{}
	for {
		k := b.d.PeekKind()
		if k == jsontext.KindEndArray {
			break
		}
		if len(out) == most && k != jsontext.KindInvalid {
			return &tooLarge{reason: tooMany}
		}
		var s T
		if err := text(b, &s); err != nil {
			return err
		}
		out = append(out, s)
	}
	if _, err := b.d.ReadToken(); err != nil {
		return malformed(err)
	}
	*into = out
	return nil
}

// integer reads a whole number that the type holds. Null leaves it as it was.
func integer[T ~int | ~int64](b *body, into *T) error {
	t, err := b.d.ReadToken()
	if err != nil {
		return malformed(err)
	}
	switch t.Kind() {
	case jsontext.KindNull:
		return nil
	case jsontext.KindNumber:
		n, err := t.Int()
		if err != nil || int64(T(n)) != n {
			return fmt.Errorf("the request body holds a number at %.100q that is not a whole number of 64 bits, and it holds one there", b.d.StackPointer())
		}
		*into = T(n)
		return nil
	}
	return b.mistyped(t.Kind(), "a whole number")
}

// bytes reads what encoding/json writes a []byte as: standard base64, in a string. Null is nil.
//
// Decoded from the body where it lies, with no copy of the text on the way, since base64 needs no
// escape. A writer may escape a character anyway, and a string that does is unquoted first, into
// a copy no larger than itself.
func (b *body) bytes(into *[]byte) error {
	v, err := b.d.ReadValue()
	if err != nil {
		return malformed(err)
	}
	switch v.Kind() {
	case jsontext.KindNull:
		*into = nil
		return nil
	case jsontext.KindString:
	default:
		return b.mistyped(v.Kind(), "a string of base64")
	}
	encoded := []byte(v[1 : len(v)-1])
	if bytes.IndexByte(encoded, '\\') >= 0 {
		if encoded, err = jsontext.AppendUnquote(nil, v); err != nil {
			return malformed(err)
		}
	}
	out := make([]byte, base64.StdEncoding.DecodedLen(len(encoded)))
	n, err := base64.StdEncoding.Decode(out, encoded)
	if err != nil {
		// Without the decoder's own error, which points at the byte where the string stopped
		// being base64: the string may be a secret, and a secret is not something to point into.
		return fmt.Errorf("the request body holds a string at %.100q that is not base64, where it holds bytes written as base64", b.d.StackPointer())
	}
	*into = out[:n]
	return nil
}

// document reads a document whose shape is its caller's, as the inputs of a run are, and answers
// the bytes it is written in rather than anything decoded from them. Every value it holds counts
// against most, an object or an array as well as each thing it holds, and the one past it is
// refused with tooMany.
//
// Counted rather than decoded, because nothing here reads the values: they are kept as they were
// written, a slice of the body, and what a document costs to decode is paid by whoever decodes it.
// The count is what bounds that, here and wherever the document is read again.
func (b *body) document(most int, tooMany string) (jsontext.Value, error) {
	from := b.d.InputOffset()
	b.values = most
	if err := b.skim(tooMany); err != nil {
		return nil, err
	}
	return jsontext.Value(b.written(from)), nil
}

// written is what was read since the offset from, as it is written in the body: less the
// whitespace, the colon or the comma before it, which belong to the value before it.
func (b *body) written(from int64) []byte {
	return bytes.TrimLeft(b.raw[from:b.d.InputOffset()], " \t\r\n:,")
}

// numberMaxDigits is how far from the point a number in a document may reach: how many digits
// after it PostgreSQL writes it back with, and how large its exponent may be either way.
//
// A document is written down as jsonb, which keeps a number at the scale it was written with and
// writes it back in full at every read: 0e-16383 is eight bytes sent and 16,385 read back, each
// time the controller decides on the run, and one digit further is a number PostgreSQL refuses to
// hold at all. 340 is as far as a 64-bit float reaches, since the smallest of them written the
// shortest way, 4.9406564584124654e-324, has 340 digits after the point. So every number whoever
// decodes the inputs can hold is taken, and none is read back more than 340 bytes longer than it
// was sent, which is 34 MB for all the values a run's inputs may hold: about what storing every
// number as a 64-bit float cost when encoding/json wrote them, 5e-324 being read back in 326.
const numberMaxDigits = 340

// skim reads one value and everything in it, counting each against b.values.
func (b *body) skim(tooMany string) error {
	from := b.d.InputOffset()
	t, err := b.d.ReadToken()
	if err != nil {
		return malformed(err)
	}
	if b.values <= 0 {
		return &tooLarge{reason: tooMany}
	}
	b.values--
	switch t.Kind() {
	case jsontext.KindNumber:
		return b.number(t, b.written(from))
	case jsontext.KindString:
		return b.storable(b.written(from))
	case jsontext.KindBeginArray:
		for b.d.PeekKind() != jsontext.KindEndArray {
			if err := b.skim(tooMany); err != nil {
				return err
			}
		}
		_, err = b.d.ReadToken()
	case jsontext.KindBeginObject:
		for b.d.PeekKind() != jsontext.KindEndObject {
			name := b.d.InputOffset()
			if _, err := b.d.ReadToken(); err != nil {
				return malformed(err)
			}
			if err := b.storable(b.written(name)); err != nil {
				return err
			}
			if err := b.skim(tooMany); err != nil {
				return err
			}
		}
		_, err = b.d.ReadToken()
	}
	return malformed(err)
}

// number refuses a number of a document that whoever decodes it cannot hold, or that reaches
// further from the point than numberMaxDigits.
//
// Whoever decodes it holds it in a 64-bit float, so 1e400, which no float holds, would be refused
// by the controller's own decoding at every pass on the run rather than by this request in front
// of whoever sent it, and 1e-400, which is not zero but which a float holds only as zero, would be
// read as zero with nobody told.
func (b *body) number(t jsontext.Token, written []byte) error {
	mantissa, exponent := written, 0
	if e := bytes.IndexAny(written, "eE"); e >= 0 {
		// The grammar has at least one digit after the e, and at most one sign before them.
		digits := written[e+1:]
		mantissa = written[:e]
		for _, c := range bytes.TrimLeft(digits, "+-") {
			// Held short of overflowing, since an exponent past numberMaxDigits is refused
			// however far past it is.
			exponent = min(exponent*10+int(c-'0'), numberMaxDigits+1)
		}
		if digits[0] == '-' {
			exponent = -exponent
		}
	}
	if f, err := t.Float(); err != nil || f == 0 && bytes.ContainsAny(mantissa, "123456789") {
		return fmt.Errorf("the request body holds a number at %.100q that no 64-bit float holds, and a value is decoded into one", b.d.StackPointer())
	}
	scale := 0
	if _, fraction, ok := bytes.Cut(mantissa, []byte(".")); ok {
		scale = len(fraction)
	}
	if scale-exponent > numberMaxDigits || exponent > numberMaxDigits || exponent < -numberMaxDigits {
		return fmt.Errorf("the request body holds a number at %.100q written to more than %d digits from the point, and no 64-bit float needs more", b.d.StackPointer(), numberMaxDigits)
	}
	return nil
}

// storable refuses a string or a name of a document that holds U+0000, which JSON writes \u0000
// and jsonb refuses to hold: kept as written, it would be refused by PostgreSQL, as a failure of
// the API's own, rather than by this request in front of whoever sent it.
func (b *body) storable(quoted []byte) error {
	if bytes.IndexByte(quoted, '\\') < 0 {
		return nil
	}
	for i := 0; i < len(quoted); i++ {
		if quoted[i] != '\\' {
			continue
		}
		if bytes.HasPrefix(quoted[i+1:], []byte("u0000")) {
			return fmt.Errorf("the request body holds U+0000 in a string at %.100q, which PostgreSQL does not keep in JSON", b.d.StackPointer())
		}
		// What follows a backslash is escaped, so \\u0000 is a backslash and five characters.
		i++
	}
	return nil
}

// mistyped refuses a value of the wrong kind, saying where it is and what is written there.
func (b *body) mistyped(got jsontext.Kind, want string) error {
	if at := b.d.StackPointer(); at != "" {
		return fmt.Errorf("the request body holds %s at %.100q, where it holds %s", kindOf(got), at, want)
	}
	return fmt.Errorf("the request body is %s, and this route reads %s", kindOf(got), want)
}

// unknown refuses a member no field of the request is called. Only the first 64 characters of the
// name are repeated, since a name is as long as its caller likes.
func unknown(name string) error {
	return fmt.Errorf("the request body carries %.64q, which is not a field of it, and a field nobody reads is refused rather than half understood", name)
}

// malformed is a body that is not JSON, or not JSON the route could read. Nil is nil, so that the
// last read of a function can be returned through it.
func malformed(err error) error {
	if err == nil {
		return nil
	}
	if err == io.EOF || errors.Is(err, io.ErrUnexpectedEOF) {
		return errors.New("the request body ends in the middle of its document")
	}
	return fmt.Errorf("the request body: %w", err)
}

func kindOf(k jsontext.Kind) string {
	switch k {
	case jsontext.KindNull:
		return "null"
	case jsontext.KindTrue, jsontext.KindFalse:
		return "a boolean"
	case jsontext.KindString:
		return "a string"
	case jsontext.KindNumber:
		return "a number"
	case jsontext.KindBeginObject:
		return "an object"
	case jsontext.KindBeginArray:
		return "an array"
	}
	return "nothing"
}
