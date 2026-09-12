package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"

	"github.com/agentiik/agentiik/agk"
)

// emit publishes items on an output port.
//
//	agk emit <port> [--from <file>|-] [--filter <expr>] [--id <field>]
//
// It writes /agk/out/ports/<port>.json as a valid envelope, with the metadata taken from
// what the container was given. The shapes it reads are the three a script produces: a
// JSON array becomes its elements, a JSON Lines stream becomes its lines, and a single
// object becomes one item.
//
// A scalar is refused. An item's data is an object, never a scalar and never an array, so
// wrapping 42 would mean inventing the key it sits under, and a key this program chose
// would be a key no workflow expression could have known to read.
//
// A second emit on the same port appends to what is there. A script that emits in a loop
// is the ordinary case, and a silent overwrite would lose the earlier batch; a script that
// wants one batch writes one command.
func emit(e env, args []string) error {
	first, rest := leading(args)
	set := flags("emit")
	from := set.String("from", "-", "the file the payload is read from, or - for standard input")
	expr := set.String("filter", "", "keep the items a field path is true of, as .valid, or false of, as .valid | not")
	field := set.String("id", "", "the field an item's identity is derived from")
	if err := set.Parse(rest); err != nil {
		return err
	}
	const usage = "agk emit <port> [--from <file>|-] [--filter <expr>] [--id <field>]"
	name, err := argument(first, set, usage)
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("no port named: %s, and the ports this step declares reach it as %s", usage, EnvOutPorts)
	}

	port := agk.Port(name)
	if err := port.Validate(); err != nil {
		return err
	}
	t, err := readTask(e.Getenv)
	if err != nil {
		return err
	}
	if err := t.declares(port); err != nil {
		return err
	}

	keep, err := parseFilter(*expr)
	if err != nil {
		return err
	}
	if err := idField(*field); err != nil {
		return err
	}

	payload, err := readPayload(e, *from)
	if err != nil {
		return err
	}
	data, err := objects(payload)
	if err != nil {
		return err
	}
	if *from == "-" && isEnvelope(data) {
		return fmt.Errorf("standard input carries an envelope, which is what the contract puts on the container's standard input: a port is emitted from a document of its own, so name the payload with --from, and agk items is what reads the items of the input envelope")
	}

	items := make([]agk.Item, 0, len(data))
	for i, d := range data {
		if !keep.keeps(d) {
			continue
		}
		item, err := newItem(d, *field)
		if err != nil {
			return fmt.Errorf("item %d of the payload: %w", i+1, err)
		}
		items = append(items, item)
	}

	envelope, existing, err := readPort(e, port)
	if err != nil {
		return err
	}
	if existing {
		if err := agrees(envelope, t, port, portPath(e, port)); err != nil {
			return err
		}
	} else {
		envelope = agk.Envelope{Meta: t.meta(port, e.Now()), Items: []agk.Item{}}
	}
	envelope.Items = append(envelope.Items, items...)
	envelope.Meta.Count = len(envelope.Items)
	if err := identities(envelope, *field); err != nil {
		return err
	}

	return writePort(e, port, envelope)
}

// readPayload takes the payload in, from a file or from standard input.
//
// The ceiling is envelope_max_bytes. A payload longer than the largest envelope cannot
// become one whatever is done to it, and reading it whole first would make the limit the
// one thing it could not protect. A step with more than that to say paginates across
// several ports or writes a JSON Lines artifact, which is the documentation's own answer
// for max_items and is the same answer here.
func readPayload(e env, from string) ([]byte, error) {
	r := e.In
	what := "standard input"
	if from != "-" {
		f, err := os.Open(from)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r, what = f, from
	}

	ceiling := limits().EnvelopeMaxBytes
	if ceiling <= 0 || ceiling == math.MaxInt64 {
		return io.ReadAll(r)
	}
	b, err := io.ReadAll(io.LimitReader(r, ceiling+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > ceiling {
		return nil, fmt.Errorf("%s carries more than the %d bytes an envelope may hold, and reading stopped there: a step with more than that to say paginates across several ports or writes a JSON Lines artifact", what, ceiling)
	}
	return b, nil
}

// objects reads the payload into one data object per item.
//
// The three shapes are told apart by what is in the stream rather than by a flag. One
// array and nothing after it is a batch; anything else is one item per value, which is
// what makes a JSON Lines file and a single object the same rule read twice.
//
// An empty payload is no items and not a refusal. A script that loops over what it found
// and found nothing has emitted an empty batch, which is a legitimate envelope: the edge
// below reads a batch of nothing and the step downstream decides its own fate.
func objects(payload []byte) ([]map[string]any, error) {
	d := json.NewDecoder(bytes.NewReader(payload))
	d.UseNumber()

	var values []any
	for {
		var v any
		err := d.Decode(&v)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("the payload is not JSON: %s", err)
		}
		values = append(values, v)
	}

	if len(values) == 1 {
		if batch, ok := values[0].([]any); ok {
			values = batch
		}
	}

	out := make([]map[string]any, len(values))
	for i, v := range values {
		data, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("item %d of the payload is %s: an item's data is an object, never a scalar and never an array, so an item that is a single value carries it under a name", i+1, describe(v))
		}
		out[i] = data
	}
	return out, nil
}

// isEnvelope says whether what arrived on standard input is the envelope the contract puts
// there.
//
// The check exists because the accident is cheap and silent. A script that writes agk emit
// out with nothing piped into it reads the container's own standard input, which carries
// the input envelope, and one item whose data is {meta, items} would travel down the edge
// looking like a batch. Refusing it costs a payload nobody writes: an object carrying
// exactly those two members and meant as an item's data.
//
// It is asked of standard input alone. A file named with --from was named by the author.
func isEnvelope(data []map[string]any) bool {
	if len(data) != 1 {
		return false
	}
	if len(data[0]) != 2 {
		return false
	}
	_, meta := data[0]["meta"]
	_, items := data[0]["items"]
	return meta && items
}

// describe says what a value is, in the words the rule that refused it uses.
func describe(v any) string {
	switch value := v.(type) {
	case nil:
		return "null"
	case []any:
		return "an array"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case json.Number:
		return "the number " + value.String()
	default:
		return "not an object"
	}
}

// newItem mints one item around its payload.
//
// Without --id the identity is minted by agk, which is correct and is also the one thing
// that makes a script step produce a different envelope on an identical rerun: a ULID
// carries the moment it was minted at. A script that wants to be reproducible names a
// field, and the two runs then agree item for item.
func newItem(data map[string]any, field string) (agk.Item, error) {
	if field == "" {
		return agk.NewItem(data), nil
	}
	id, err := identity(data, field)
	if err != nil {
		return agk.Item{}, err
	}
	return agk.Item{ID: id, Data: data, Files: []agk.File{}}, nil
}

// identity derives an item's identifier from one field of its data.
//
// The value is written as itself and not hashed. An identifier is what a run inspector, a
// log line and a refusal all name the item by, and a digest of the invoice number is a
// thing nobody can look up; the field was chosen by the author precisely because it
// identifies the thing.
//
// Only a scalar will do. An object or a list has no one written form to be an identifier,
// and null is the field being absent under another spelling.
func identity(data map[string]any, field string) (string, error) {
	v, ok := data[field]
	if !ok {
		return "", fmt.Errorf("no field %s, and the identity is derived from it", field)
	}
	switch value := v.(type) {
	case string:
		if value == "" {
			return "", fmt.Errorf("the field %s is empty, and an item's identifier is never empty", field)
		}
		return value, nil
	case json.Number:
		return value.String(), nil
	case bool:
		return strconv.FormatBool(value), nil
	default:
		return "", fmt.Errorf("the field %s is %s, and an identity is derived from a scalar: a value with no one written form cannot be the name a fan-out, a merge and a replay speak about the item by", field, describe(v))
	}
}

// idField holds --id to naming one field of an item's data.
//
// One field and not a path. The identity is a property of the item, a path into a nested
// document is a property of its shape, and the documented flag is spelled --id <field>.
func idField(field string) error {
	if field == "" {
		return nil
	}
	for i := 0; i < len(field); i++ {
		switch c := field[i]; {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return fmt.Errorf("--id names one field of an item's data, as --id invoice_number, and %q carries %q", field, string(c))
		}
	}
	return nil
}
