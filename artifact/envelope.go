package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/agentiik/agentiik/agk"
)

// An envelope as an object.
//
// An envelope is content like anything else here, so it is stored the same way and addressed the
// same way: two steps publishing identical bytes publish one object, and a replay that recomputes
// the same content writes nothing. It lives in this package rather than in the controller because
// the controller writes them and the API reads them back, and a digest check written twice is a
// digest check that eventually differs in one of the two places.

// PutEnvelope writes one and answers what names it.
func PutEnvelope(ctx context.Context, objects Objects, namespace string, e agk.Envelope) (digest string, size int64, err error) {
	body, digest, err := encodeEnvelope(e)
	if err != nil {
		return "", 0, err
	}
	key := Key(namespace, digest)
	held, err := objects.Has(ctx, key)
	if err != nil {
		return "", 0, err
	}
	if !held {
		if err := objects.Put(ctx, key, bytes.NewReader(body)); err != nil {
			return "", 0, err
		}
	}
	return digest, int64(len(body)), nil
}

// EnvelopeDigest answers what names one envelope and how long it is, which is what PutEnvelope
// would store it under, without storing it.
//
// A runner records how a task ended, envelopes by digest, before it has uploaded any of them, and
// the digest it records is only worth anything if it is the one the upload then writes. So both
// are computed here, from the one encoding, and never twice.
func EnvelopeDigest(e agk.Envelope) (digest string, size int64, err error) {
	body, digest, err := encodeEnvelope(e)
	if err != nil {
		return "", 0, err
	}
	return digest, int64(len(body)), nil
}

// encodeEnvelope is an envelope's bytes as an object holds them, and the digest they are stored
// under.
func encodeEnvelope(e agk.Envelope) ([]byte, string, error) {
	var buf bytes.Buffer
	if _, err := e.Encode(&buf); err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:]), nil
}

// ErrNotAnEnvelope is what an object read back under a digest is when it is not the envelope that
// digest names: longer than an envelope may be, holding bytes whose digest is another, or holding
// bytes that do not decode as an envelope.
//
// It has a name because a reader has to tell it apart from an object that could not be read. A
// store that did not answer, or does not hold the object yet, may be different on the next try;
// this is the same on every try, since the digest names the bytes and the bytes do not change.
var ErrNotAnEnvelope = errors.New("artifact: not an envelope")

// GetEnvelope reads one back, and refuses bytes that are not the bytes the digest names.
//
// The check is not belt and braces. A store that handed back something else under a digest has
// broken the one promise content addressing makes, and a reader that trusted the transfer would
// schedule against, or hand a runner, something nobody ever published.
//
// Reading stops one byte past envelope_max_bytes, for the reason agk.Decode stops there. The digest
// is not always one the reader wrote: the controller reads back what a runner names, and a runner
// may name any object it could upload, which runs to artifact_max_bytes. Taken whole, one such
// object would be held in memory before it was found to be too long for an envelope. An object
// that long is refused without its digest being checked, since whatever it holds is not one.
func GetEnvelope(ctx context.Context, objects Objects, namespace, digest string, l agk.Limits) (agk.Envelope, error) {
	r, err := objects.Open(ctx, Key(namespace, digest))
	if err != nil {
		return agk.Envelope{}, err
	}
	defer r.Close()

	// A limit of zero is not applied, as agk reads it, and the largest limit there is
	// would wrap with one more.
	var from io.Reader = r
	limit := l.EnvelopeMaxBytes
	if limit > 0 && limit < math.MaxInt64 {
		from = io.LimitReader(r, limit+1)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(from); err != nil {
		return agk.Envelope{}, err
	}
	if limit > 0 && int64(buf.Len()) > limit {
		return agk.Envelope{}, fmt.Errorf("%w: the object under sha256/%s is longer than the %d bytes an envelope may be, and reading stopped there", ErrNotAnEnvelope, digest, limit)
	}
	sum := sha256.Sum256(buf.Bytes())
	if got := hex.EncodeToString(sum[:]); got != digest {
		return agk.Envelope{}, fmt.Errorf("%w: the object under sha256/%s holds sha256/%s", ErrNotAnEnvelope, digest, got)
	}
	e, err := agk.Decode(&buf, l)
	if err != nil {
		return agk.Envelope{}, fmt.Errorf("%w: the object under sha256/%s: %w", ErrNotAnEnvelope, digest, err)
	}
	return e, nil
}
