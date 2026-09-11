// Package artifact is the content-addressed store, and nothing else.
//
// The logical URI agk://run/<run>/<step>/<port>/<name> resolves to a physical key
// sha256/<digest>, so two steps producing identical bytes store one copy and a replay
// that recomputes the same content writes nothing. Deduplication is scoped per
// namespace: a Store is opened for one namespace and builds every physical key as
// <namespace>/sha256/<digest>, so two namespaces never share a physical object, an
// existence check cannot reach across one, and forgetting the namespace is not
// expressible from here.
//
// Objects is the byte layer underneath, three methods wide, so that the local directory
// of an agk run --local and the object store of a server run share this logic instead
// of growing a second copy of it.
//
// Reference counting, retain and expiry are deliberately absent. The specification puts
// expiry on the reference and not on the object: the row binding a run, a step and a
// port to a digest is what is dropped, and the object is collected once its reference
// count reaches zero. That row is the artifacts table, which arrives with the control
// plane, and its shape is not knowable from here.
package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/agentiik/agentiik/agk"
)

// Key builds the physical key an object is held under: <namespace>/sha256/<digest>.
//
// It is written once, and every caller goes through it, because the namespace prefix is
// the whole of the scoping rule. A key built by hand somewhere else is a key that can
// forget the namespace, and an existence check on a key without one is an existence
// check that answers for every tenant at once.
func Key(namespace, digest string) string {
	return namespace + "/sha256/" + digest
}

// Objects is the byte layer under the store: content in, content out, keyed by the
// opaque strings Key builds. A local directory backs agk run --local and an object
// store backs a server run, and they differ here and nowhere else.
//
// What a second implementation owes:
//
//   - A key is opaque and already scoped. It is built by Key alone, its separator is
//     the forward slash, and the namespace is its first segment. An implementation
//     must not widen a lookup beyond the exact key it was given, and must not offer a
//     caller any way to enumerate across keys: an existence check that reaches past
//     its namespace is the one thing the scoping rule forbids.
//   - An object is immutable, because its key is the digest of its bytes. A key that
//     is held holds those bytes for as long as it exists: Put of a key already held
//     may skip the write, and must never replace what is there with different bytes.
//   - Put is all or nothing. A key becomes visible to Has and Open with the whole
//     object behind it, or not at all, so that an interrupted write leaves nothing
//     half formed for the next reader to fetch and reject. Concurrent Puts of one key
//     are safe: two steps computing identical bytes is the ordinary case here rather
//     than a race to be avoided.
//   - Has answers false and no error for a key that is absent. Absence is the expected
//     answer, not a failure.
//   - Open returns an error satisfying errors.Is(err, fs.ErrNotExist) for a key that is
//     absent, which is how a caller tells an artifact that is gone from a store that is
//     broken.
//   - Open yields the bytes Put was given, byte for byte. Store.Open digests what it
//     reads and refuses a mismatch, so any transformation on the way through is read as
//     corruption.
//   - ctx is honoured. An artifact runs to artifact_max_bytes, and a caller that gives
//     up on a transfer of that size must be able to stop it.
type Objects interface {
	Has(ctx context.Context, key string) (bool, error)
	Put(ctx context.Context, key string, r io.Reader) error
	Open(ctx context.Context, key string) (io.ReadCloser, error)
}

// defaultMediaType is what an artifact is written as when the caller names no media
// type. The envelope requires media_type on every file entry, and a store that emitted
// an entry without one would be handing back a document the runner then refuses, so the
// gap is filled with the type that says no more than "bytes".
const defaultMediaType = "application/octet-stream"

// checkNamespace refuses a namespace that could not be the first segment of a physical
// key. The specification names no grammar for a namespace, so nothing is imposed beyond
// what the key demands: a namespace carrying a separator, or one spelt . or .., is a
// namespace whose objects could be addressed from inside another's prefix, which is
// exactly what deduplication being scoped per namespace forbids.
func checkNamespace(namespace string) error {
	switch {
	case namespace == "":
		return errors.New("artifact: no namespace: every physical key is built as <namespace>/sha256/<digest>, so a store without one cannot be opened")
	case namespace == "." || namespace == "..":
		return fmt.Errorf("artifact: namespace %q is not a key segment", namespace)
	case strings.ContainsAny(namespace, `/\`):
		return fmt.Errorf("artifact: namespace %q carries a path separator, which would let its objects be addressed from inside another namespace", namespace)
	case strings.ContainsRune(namespace, 0):
		return fmt.Errorf("artifact: namespace %q carries a null byte", namespace)
	}
	return nil
}

// checkKey refuses a key that is not the shape Key builds. The directory
// implementation turns a key into a path, and a key holding .. or a leading slash is a
// key that writes outside the root it was opened on.
func checkKey(key string) error {
	switch {
	case key == "":
		return errors.New("artifact: empty object key")
	case path.IsAbs(key):
		return fmt.Errorf("artifact: object key %q is absolute", key)
	case path.Clean(key) != key:
		return fmt.Errorf("artifact: object key %q is not in its cleaned form", key)
	case key == ".." || strings.HasPrefix(key, "../") || strings.Contains(key, "/../") || strings.HasSuffix(key, "/.."):
		return fmt.Errorf("artifact: object key %q leaves its root", key)
	case strings.ContainsRune(key, 0):
		return fmt.Errorf("artifact: object key %q carries a null byte", key)
	}
	return nil
}

// checkURI refuses a logical URI the store cannot address from.
//
// The rule belongs to the vocabulary, and the URI is written out and read back to apply
// it, so that the store holds no second copy of what a run, a step, a port and a name
// may be. An artifact written under a URI that does not parse would travel in an
// envelope the runner then refuses, and that refusal belongs where the URI is chosen
// rather than where the envelope is read.
func checkURI(u agk.URI) error {
	if _, err := agk.ParseURI(u.String()); err != nil {
		return fmt.Errorf("artifact: %w", err)
	}
	return nil
}

// ctxReader stops a copy when the context is done. An artifact runs to
// artifact_max_bytes, and a transfer of that size that nobody is waiting for any more is
// a transfer that has to be able to stop between two chunks rather than at the end.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
