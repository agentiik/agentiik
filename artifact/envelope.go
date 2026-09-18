package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

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
	var buf bytes.Buffer
	size, err = e.Encode(&buf)
	if err != nil {
		return "", 0, err
	}
	sum := sha256.Sum256(buf.Bytes())
	digest = hex.EncodeToString(sum[:])

	key := Key(namespace, digest)
	held, err := objects.Has(ctx, key)
	if err != nil {
		return "", 0, err
	}
	if !held {
		if err := objects.Put(ctx, key, bytes.NewReader(buf.Bytes())); err != nil {
			return "", 0, err
		}
	}
	return digest, size, nil
}

// GetEnvelope reads one back, and refuses bytes that are not the bytes the digest names.
//
// The check is not belt and braces. A store that handed back something else under a digest has
// broken the one promise content addressing makes, and a reader that trusted the transfer would
// schedule against, or hand a runner, something nobody ever published.
func GetEnvelope(ctx context.Context, objects Objects, namespace, digest string, l agk.Limits) (agk.Envelope, error) {
	r, err := objects.Open(ctx, Key(namespace, digest))
	if err != nil {
		return agk.Envelope{}, err
	}
	defer r.Close()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		return agk.Envelope{}, err
	}
	sum := sha256.Sum256(buf.Bytes())
	if got := hex.EncodeToString(sum[:]); got != digest {
		return agk.Envelope{}, fmt.Errorf("artifact: the object under sha256/%s holds sha256/%s", digest, got)
	}
	return agk.Decode(&buf, l)
}
