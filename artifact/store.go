package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"os"

	"github.com/agentiik/agentiik/agk"
)

// Store is the content-addressed store of one namespace.
//
// Every physical key it touches is built as <namespace>/sha256/<digest>, which is what
// scopes deduplication: two namespaces holding identical bytes hold two objects, and
// neither can learn of the other by asking whether a digest is already there.
type Store struct {
	objects   Objects
	namespace string
	limits    agk.Limits
}

// New opens the store of one namespace over a byte layer.
//
// It refuses an empty namespace rather than defaulting one, because a store with no
// namespace is a store whose objects are shared by everyone, and that is the one thing
// content addressing must not do.
func New(o Objects, namespace string, l agk.Limits) (*Store, error) {
	if o == nil {
		return nil, errors.New("artifact: no object layer: a store is opened over artifact.Dir or another Objects")
	}
	if err := checkNamespace(namespace); err != nil {
		return nil, err
	}
	// The limits are taken as they come, including an artifact_max_bytes of zero, which
	// agk reads as a rule deliberately turned off. An engine passes agk.DefaultLimits or
	// the namespace's own.
	return &Store{objects: o, namespace: namespace, limits: l}, nil
}

// Namespace is the namespace the store was opened for, and the first segment of every
// key it builds.
func (s *Store) Namespace() string {
	return s.namespace
}

// Put writes the bytes of r as the artifact addressed by u and returns the files[] entry
// that carries it in an envelope.
//
// It takes the logical URI rather than a bare name so that a refusal can say which step
// and which port was refused: the physical key holds a digest and nothing a person can
// read a step out of.
//
// Above artifact_max_bytes the artifact is refused as an application failure of the
// emitting step, which is the band 1 to 99 of the exit code table. The stream is not
// read past the limit: one byte more than the limit is all it takes to know, and reading
// the remainder of an oversized artifact to say so would be the wrong price.
//
// The write is skipped when the byte layer says the object is already held, because the
// key is the digest of the content: two steps producing identical bytes store one copy,
// and a replay that recomputes the same content writes nothing where the byte layer can
// ask, as the directory behind agk run --local can. A runner's cannot, so a server run
// sends the bytes again and the built-in store writes them again under the same key.
// That is still one copy, since they are the same bytes, and it costs the upload and the
// write.
func (s *Store) Put(ctx context.Context, u agk.URI, mediaType string, r io.Reader) (agk.File, error) {
	if err := checkURI(u); err != nil {
		return agk.File{}, err
	}
	if mediaType == "" {
		mediaType = defaultMediaType
	}

	// The key is not known until the last byte has been read, so the bytes are staged
	// while they are counted and digested. A file rather than memory, because what is
	// staged runs to artifact_max_bytes. The staging directory is the one TMPDIR names.
	stage, err := os.CreateTemp("", "agk-artifact-*")
	if err != nil {
		return agk.File{}, fmt.Errorf("artifact %s: %w", u, err)
	}
	defer func() {
		stage.Close()
		os.Remove(stage.Name())
	}()

	digest := sha256.New()
	source := io.Reader(ctxReader{ctx: ctx, r: r})
	capped := s.limits.ArtifactMaxBytes > 0
	if capped {
		// One byte past the limit is all it takes to refuse, and no more than that is
		// read. The guard is for a caller naming the largest limit there is, where one
		// more would wrap.
		ceiling := s.limits.ArtifactMaxBytes
		if ceiling < math.MaxInt64 {
			ceiling++
		}
		source = io.LimitReader(source, ceiling)
	}
	size, err := io.Copy(io.MultiWriter(stage, digest), source)
	if err != nil {
		return agk.File{}, fmt.Errorf("artifact %s: %w", u, err)
	}
	if capped && size > s.limits.ArtifactMaxBytes {
		return agk.File{}, &agk.Refusal{
			Step:    u.Step,
			Port:    u.Port,
			Rule:    agk.RuleArtifactMaxBytes,
			Outcome: agk.Fail,
			Limit:   s.limits.ArtifactMaxBytes,
			// A lower bound rather than the size, since the stream was left where the
			// limit stopped it. The message says so rather than printing a number that
			// reads like a measurement.
			Got:    size,
			Detail: fmt.Sprintf("artifact %s is above the %d bytes the rule allows, and was not read past it", u, s.limits.ArtifactMaxBytes),
		}
	}

	sum := hex.EncodeToString(digest.Sum(nil))
	key := Key(s.namespace, sum)
	held, err := s.objects.Has(ctx, key)
	if err != nil {
		return agk.File{}, fmt.Errorf("artifact %s: %w", u, err)
	}
	if !held {
		if _, err := stage.Seek(0, io.SeekStart); err != nil {
			return agk.File{}, fmt.Errorf("artifact %s: %w", u, err)
		}
		if err := s.objects.Put(ctx, key, stage); err != nil {
			return agk.File{}, fmt.Errorf("artifact %s: %w", u, err)
		}
	}

	return agk.File{
		Name:      u.Name,
		URI:       u,
		MediaType: mediaType,
		Size:      size,
		SHA256:    sum,
	}, nil
}

// Open reads an artifact back through its digest, verifying what it read.
//
// Resolution goes through f.SHA256 and never through f.URI: the URI is the logical name
// of an artifact on a port, and the digest is its address. The digest is checked again
// as the bytes come out, because a store that hands back something other than what was
// put under a digest has broken the one promise content addressing makes, and a consumer
// that trusted the transfer would carry the damage forward.
//
// The entry is held to the whole of the rule a file entry is held to, which is agk's and
// is asked for rather than restated here: an elided digest addresses nothing, and an
// entry the envelope it travelled in would have been refused for is not an entry this
// store resolves.
func (s *Store) Open(ctx context.Context, f agk.File) (io.ReadCloser, error) {
	if err := f.Validate(); err != nil {
		return nil, fmt.Errorf("artifact %s: %w", f.URI, err)
	}
	rc, err := s.objects.Open(ctx, Key(s.namespace, f.SHA256))
	if err != nil {
		return nil, fmt.Errorf("artifact %s: %w", f.URI, err)
	}
	return &verifiedReader{source: rc, file: f, digest: sha256.New()}, nil
}

// verifiedReader digests what it reads and refuses, at the end of the stream, bytes that
// are not the ones the file entry describes. Verification can only complete at the end,
// so a caller that reads part of an artifact and stops is told nothing: what it read is
// what the store holds, and only the whole of it can be checked.
type verifiedReader struct {
	source io.ReadCloser
	file   agk.File
	digest hash.Hash
	read   int64
	failed error
}

func (v *verifiedReader) Read(p []byte) (int, error) {
	if v.failed != nil {
		return 0, v.failed
	}
	n, err := v.source.Read(p)
	if n > 0 {
		v.digest.Write(p[:n])
		v.read += int64(n)
	}
	if errors.Is(err, io.EOF) {
		if v.failed = v.verify(); v.failed != nil {
			return n, v.failed
		}
	}
	return n, err
}

func (v *verifiedReader) verify() error {
	if got := hex.EncodeToString(v.digest.Sum(nil)); got != v.file.SHA256 {
		return fmt.Errorf("artifact %s: holds sha256 %s where the envelope carries %s", v.file.URI, got, v.file.SHA256)
	}
	if v.read != v.file.Size {
		return fmt.Errorf("artifact %s: holds %d bytes where the envelope carries size %d", v.file.URI, v.read, v.file.Size)
	}
	return nil
}

func (v *verifiedReader) Close() error {
	return v.source.Close()
}
