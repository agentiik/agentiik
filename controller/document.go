package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/graph"
)

// The document: the evaluator's state as the database is allowed to hold it.
//
// "Failover is a state resume, never a rebuild", and package graph says what that costs: "load
// the State, call Next, get the Plan the instance that died would have got. A state that could
// only be rebuilt by replaying the run would make that sentence false." So the state is stored
// whole.
//
// With one surgery, which the Storage chapter forces: "Envelopes and logs are not stored in the
// database: it keeps only their digests and URIs." The state carries envelopes inline, on every
// published port and on every shard of every step, and a ten thousand shard fan-out at
// envelope_max_bytes would be forty gigabytes in one column rewritten on every decision.
//
// So every envelope is written to the object store and replaced by a hollow one: the same Meta,
// and no items. A hollow envelope is recognisable as hollow rather than merely small, because
// Meta.Count says how many items there were and agk.Envelope.Validate refuses one whose count
// and items disagree. Nothing can therefore hand the evaluator a state that was never put back
// together and have it quietly schedule against an empty batch.

// DocumentVersion is the shape this package writes around the evaluator's own.
//
// Two versions rather than one because two things can move independently: what the evaluator
// holds, which graph.StateVersion covers, and where this package chose to put the parts of it
// that are not state.
const DocumentVersion = 1

// Document is a run's evaluation state as it is stored.
type Document struct {
	Version int `json:"version"`

	// State is the evaluator's, hollowed. It is the same document at the same version, so
	// graph.New refuses one it does not understand rather than resuming a run with a field
	// it silently dropped.
	State *graph.State `json:"state"`

	// Envelopes are the bytes that were taken out, each named by where it goes back.
	Envelopes []EnvelopeRef `json:"envelopes,omitempty"`
}

// EnvelopeRef is one envelope, and the place in the state it belongs to.
//
// Shard is the position in the step's slice and not agk.Shard.Index, because a step with no
// fan-out has one shard whose index is zero, and two shards must never address one place. It is
// minus one for an envelope published by the step rather than by one of its shards.
type EnvelopeRef struct {
	Step  agk.Step `json:"step"`
	Shard int      `json:"shard"`
	Port  agk.Port `json:"port"`

	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// publishedByTheStep is the Shard value of an envelope the step published rather than one a
// shard produced. It is db.PublishedByTheStep spelled on this side of the boundary.
const publishedByTheStep = -1

// Elide writes every envelope of a state to the object store and answers the document to
// persist. The state it was given is not touched: the evaluator holds that pointer, and a
// caller that found its state hollowed under it would be right to be surprised.
func Elide(ctx context.Context, s *graph.State, namespace string, objects artifact.Objects) (Document, error) {
	if s == nil {
		return Document{}, errors.New("controller: a document of no state")
	}
	if namespace == "" {
		return Document{}, errors.New("controller: a document with no namespace, and an object key is prefixed by one")
	}

	// Copied through its own JSON, which is the one deep copy that cannot drift from what
	// is about to be written: whatever the round trip does to the copy, it does to the
	// stored document too.
	copied, err := roundTrip(s)
	if err != nil {
		return Document{}, err
	}

	d := Document{Version: DocumentVersion, State: copied}
	for _, name := range sortedSteps(copied) {
		st := copied.Steps[name]
		for _, port := range sortedPorts(st.Ports) {
			ref, err := put(ctx, namespace, objects, st.Ports[port])
			if err != nil {
				return Document{}, fmt.Errorf("controller: the envelope of %s on %s: %w", name, port, err)
			}
			ref.Step, ref.Shard, ref.Port = name, publishedByTheStep, port
			d.Envelopes = append(d.Envelopes, ref)
			st.Ports[port] = hollow(st.Ports[port])
		}
		for i := range st.Shards {
			for _, port := range sortedPorts(st.Shards[i].Ports) {
				ref, err := put(ctx, namespace, objects, st.Shards[i].Ports[port])
				if err != nil {
					return Document{}, fmt.Errorf("controller: the envelope of %s shard %d on %s: %w", name, i, port, err)
				}
				ref.Step, ref.Shard, ref.Port = name, i, port
				d.Envelopes = append(d.Envelopes, ref)
				st.Shards[i].Ports[port] = hollow(st.Shards[i].Ports[port])
			}
		}
		copied.Steps[name] = st
	}
	return d, nil
}

// Rehydrate puts the envelopes back and answers the state the evaluator is resumed from.
//
// An envelope whose object is gone is an error and not an empty batch. The bytes of a published
// port are what a downstream step reads, so a run resumed without them would schedule against
// nothing and call it success.
func Rehydrate(ctx context.Context, d Document, namespace string, objects artifact.Objects, l agk.Limits) (*graph.State, error) {
	if d.State == nil {
		return nil, errors.New("controller: a document carrying no state")
	}
	if d.Version != DocumentVersion {
		return nil, fmt.Errorf("controller: this is a version %d document and this process writes version %d: a state is resumed and never guessed at", d.Version, DocumentVersion)
	}

	s, err := roundTrip(d.State)
	if err != nil {
		return nil, err
	}
	for _, ref := range d.Envelopes {
		st, ok := s.Steps[ref.Step]
		if !ok {
			return nil, fmt.Errorf("controller: the document names an envelope of step %s, which the state does not have", ref.Step)
		}
		e, err := get(ctx, namespace, objects, ref, l)
		if err != nil {
			return nil, fmt.Errorf("controller: the envelope of %s on %s: %w", ref.Step, ref.Port, err)
		}
		switch {
		case ref.Shard == publishedByTheStep:
			if st.Ports == nil {
				st.Ports = map[agk.Port]agk.Envelope{}
			}
			st.Ports[ref.Port] = e
		case ref.Shard >= 0 && ref.Shard < len(st.Shards):
			if st.Shards[ref.Shard].Ports == nil {
				st.Shards[ref.Shard].Ports = map[agk.Port]agk.Envelope{}
			}
			st.Shards[ref.Shard].Ports[ref.Port] = e
		default:
			return nil, fmt.Errorf("controller: the document names shard %d of step %s, which has %d", ref.Shard, ref.Step, len(st.Shards))
		}
		s.Steps[ref.Step] = st
	}
	return s, nil
}

// put writes one envelope and answers what names it.
func put(ctx context.Context, namespace string, objects artifact.Objects, e agk.Envelope) (EnvelopeRef, error) {
	digest, size, err := artifact.PutEnvelope(ctx, objects, namespace, e)
	if err != nil {
		return EnvelopeRef{}, err
	}
	return EnvelopeRef{Digest: digest, Size: size}, nil
}

// putAgain writes one whatever the store says it holds, since what it holds may be what a sweep
// that claimed it is about to delete.
func putAgain(ctx context.Context, namespace string, objects artifact.Objects, digest string, e agk.Envelope) error {
	var b bytes.Buffer
	if _, err := e.Encode(&b); err != nil {
		return err
	}
	return objects.Put(ctx, artifact.Key(namespace, digest), &b)
}

// get reads one back, and refuses bytes that are not the bytes the digest names.
func get(ctx context.Context, namespace string, objects artifact.Objects, ref EnvelopeRef, l agk.Limits) (agk.Envelope, error) {
	return artifact.GetEnvelope(ctx, objects, namespace, ref.Digest, l)
}

// hollow is an envelope with its items taken out and its Meta left alone.
//
// Meta.Count is what makes it recognisable: agk.Envelope.Validate refuses an envelope whose
// count and items disagree, so a state that was stored and never put back together is caught
// rather than scheduled against.
func hollow(e agk.Envelope) agk.Envelope {
	return agk.Envelope{Meta: e.Meta, Items: []agk.Item{}}
}

// roundTrip copies a state through the encoding it is stored in.
func roundTrip(s *graph.State) (*graph.State, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("controller: the state could not be written down: %w", err)
	}
	var out graph.State
	if err := asWritten(b, &out); err != nil {
		return nil, fmt.Errorf("controller: the state could not be read back: %w", err)
	}
	return &out, nil
}

// asWritten reads a state, or a document holding one, with every number as it was written, a
// json.Number, as the evaluator holds one in agk run --local: "a number written without a
// fraction or an exponent is an int in an expression, and any other a double". Read as a float64,
// vars.n + 1 has no overload on any pass after the first, where a local run, which keeps its state
// in memory, finishes the run.
func asWritten(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	return d.Decode(v)
}
