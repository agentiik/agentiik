package artifact_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
)

// sha256OfHello is written out rather than computed, so that the test fails if the store
// ever addresses content by something other than the SHA-256 the envelope carries.
const sha256OfHello = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"

func uri(step agk.Step, port agk.Port, name string) agk.URI {
	return agk.URI{Run: "01JMZ8W4K2R7Q0E3N5T9", Step: step, Port: port, Name: name}
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// memory is an Objects that keeps its objects in a map and records every write, so that
// a test can say what was stored once and what was stored twice.
type memory struct {
	mu     sync.Mutex
	blobs  map[string][]byte
	writes []string
}

func newMemory() *memory {
	return &memory{blobs: make(map[string][]byte)}
}

func (m *memory) Has(ctx context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.blobs[key]
	return ok, nil
}

func (m *memory) Put(ctx context.Context, key string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blobs[key] = b
	m.writes = append(m.writes, key)
	return nil
}

func (m *memory) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.blobs[key]
	if !ok {
		return nil, fmt.Errorf("%s: %w", key, fs.ErrNotExist)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memory) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.blobs))
	for k := range m.blobs {
		out = append(out, k)
	}
	return out
}

func TestKeyCarriesTheNamespaceAndTheDigest(t *testing.T) {
	if got, want := artifact.Key("acme", sha256OfHello), "acme/sha256/"+sha256OfHello; got != want {
		t.Fatalf("Key: got %q, want %q", got, want)
	}
}

func TestNewRefusesAStoreThatCouldNotScopeItsKeys(t *testing.T) {
	limits := agk.DefaultLimits()
	tests := []struct {
		name      string
		objects   artifact.Objects
		namespace string
		limits    agk.Limits
		ok        bool
	}{
		{name: "a namespace", objects: newMemory(), namespace: "acme", limits: limits, ok: true},
		{name: "no objects", objects: nil, namespace: "acme", limits: limits},
		{name: "no namespace", objects: newMemory(), namespace: "", limits: limits},
		{name: "a namespace carrying a separator", objects: newMemory(), namespace: "acme/other", limits: limits},
		{name: "a namespace spelt dot", objects: newMemory(), namespace: ".", limits: limits},
		{name: "a namespace spelt dot dot", objects: newMemory(), namespace: "..", limits: limits},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := artifact.New(tt.objects, tt.namespace, tt.limits)
			switch {
			case tt.ok && err != nil:
				t.Fatalf("New: %v", err)
			case tt.ok:
				if s.Namespace() != tt.namespace {
					t.Fatalf("Namespace: got %q, want %q", s.Namespace(), tt.namespace)
				}
			case err == nil:
				t.Fatalf("New: accepted %q", tt.namespace)
			}
		})
	}
}

func TestPutReturnsTheFileEntryThatTravelsInTheEnvelope(t *testing.T) {
	objects := newMemory()
	s, err := artifact.New(objects, "acme", agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	u := uri("normalize", "ok", "greeting.txt")
	file, err := s.Put(t.Context(), u, "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if file.Name != "greeting.txt" {
		t.Errorf("Name: got %q, want %q", file.Name, "greeting.txt")
	}
	if file.URI != u {
		t.Errorf("URI: got %v, want %v", file.URI, u)
	}
	if file.MediaType != "text/plain" {
		t.Errorf("MediaType: got %q", file.MediaType)
	}
	if file.Size != 5 {
		t.Errorf("Size: got %d, want 5", file.Size)
	}
	if file.SHA256 != sha256OfHello {
		t.Errorf("SHA256: got %q, want %q", file.SHA256, sha256OfHello)
	}
	if got, want := objects.keys(), []string{"acme/sha256/" + sha256OfHello}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("physical keys: got %v, want %v", got, want)
	}
}

func TestPutNamesTheBytesWhenTheCallerNamesNoMediaType(t *testing.T) {
	s, err := artifact.New(newMemory(), "acme", agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	file, err := s.Put(t.Context(), uri("normalize", "ok", "blob"), "", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	// The envelope requires media_type on every file entry, so the store never returns
	// one without it.
	if file.MediaType != "application/octet-stream" {
		t.Fatalf("MediaType: got %q, want application/octet-stream", file.MediaType)
	}
}

func TestIdenticalBytesAreStoredOnce(t *testing.T) {
	objects := newMemory()
	s, err := artifact.New(objects, "acme", agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	// Two steps producing identical bytes, and then the same step replayed.
	for _, u := range []agk.URI{
		uri("normalize", "ok", "a.txt"),
		uri("archive", "out", "b.txt"),
		uri("normalize", "ok", "a.txt"),
	} {
		if _, err := s.Put(t.Context(), u, "text/plain", strings.NewReader("hello")); err != nil {
			t.Fatalf("Put %v: %v", u, err)
		}
	}
	if len(objects.writes) != 1 {
		t.Fatalf("writes: got %v, want the object written once", objects.writes)
	}
}

func TestTwoNamespacesNeverShareAPhysicalObject(t *testing.T) {
	objects := newMemory()
	first, err := artifact.New(objects, "acme", agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	second, err := artifact.New(objects, "globex", agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	u := uri("normalize", "ok", "greeting.txt")
	a, err := first.Put(t.Context(), u, "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.Put(t.Context(), u, "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}

	if a.SHA256 != b.SHA256 {
		t.Fatalf("digests differ for identical bytes: %s and %s", a.SHA256, b.SHA256)
	}
	// The second namespace did not skip its write on the strength of the first
	// namespace's object, which is what an existence check reaching across a namespace
	// would have done.
	if len(objects.writes) != 2 {
		t.Fatalf("writes: got %v, want one per namespace", objects.writes)
	}
	want := map[string]bool{
		"acme/sha256/" + sha256OfHello:   true,
		"globex/sha256/" + sha256OfHello: true,
	}
	for _, key := range objects.keys() {
		if !want[key] {
			t.Fatalf("physical key %q is not scoped by a namespace", key)
		}
	}

	// And a third namespace holding neither reads nothing through the same digest.
	third, err := artifact.New(objects, "initech", agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := third.Open(t.Context(), a); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open from a namespace that holds nothing: got %v, want a missing object", err)
	}
}

func TestArtifactMaxBytes(t *testing.T) {
	limits := agk.DefaultLimits()
	limits.ArtifactMaxBytes = 16

	t.Run("at the limit", func(t *testing.T) {
		s, err := artifact.New(newMemory(), "acme", limits)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Put(t.Context(), uri("render", "out", "a.bin"), "", strings.NewReader(strings.Repeat("x", 16))); err != nil {
			t.Fatalf("Put of an artifact at the limit: %v", err)
		}
	})

	t.Run("above the limit", func(t *testing.T) {
		objects := newMemory()
		s, err := artifact.New(objects, "acme", limits)
		if err != nil {
			t.Fatal(err)
		}
		counted := &countingReader{r: strings.NewReader(strings.Repeat("x", 1024))}
		_, err = s.Put(t.Context(), uri("render", "out", "big.bin"), "", counted)

		var refusal *agk.Refusal
		if !errors.As(err, &refusal) {
			t.Fatalf("Put: got %v, want a refusal", err)
		}
		if refusal.Rule != agk.RuleArtifactMaxBytes {
			t.Errorf("Rule: got %q, want %q", refusal.Rule, agk.RuleArtifactMaxBytes)
		}
		// The documentation reads artifact_max_bytes as a cap on a single artifact, and
		// a step that writes past it has failed as an application, which is the band 1
		// to 99 of the exit code table rather than a rejected envelope.
		if refusal.Outcome != agk.Fail {
			t.Errorf("Outcome: got %v, want Fail", refusal.Outcome)
		}
		if !errors.Is(err, agk.ErrStepFailed) {
			t.Errorf("errors.Is ErrStepFailed: got false for %v", err)
		}
		if refusal.Step != "render" || refusal.Port != "out" {
			t.Errorf("refusal names step %q and port %q", refusal.Step, refusal.Port)
		}
		if refusal.Limit != 16 {
			t.Errorf("Limit: got %d, want 16", refusal.Limit)
		}
		if !strings.Contains(refusal.Error(), "big.bin") {
			t.Errorf("the message does not name the artifact: %s", refusal.Error())
		}
		if len(objects.writes) != 0 {
			t.Errorf("writes: got %v, want nothing stored", objects.writes)
		}
		// One byte past the limit is all it takes to know, and the rest of an oversized
		// artifact is not read to say so.
		if counted.n > 17 {
			t.Errorf("read %d bytes past a limit of 16", counted.n)
		}
	})
}

func TestALimitThatIsZeroIsNotApplied(t *testing.T) {
	// agk reads a limit of zero as a rule deliberately turned off, reading a fixture
	// known to be oversized for instance, and the store takes the limits as they come
	// rather than holding a second reading of its own.
	s, err := artifact.New(newMemory(), "acme", agk.Limits{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Put(t.Context(), uri("render", "out", "big.bin"), "", strings.NewReader(strings.Repeat("x", 4096))); err != nil {
		t.Fatalf("Put: %v", err)
	}
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func TestPutRefusesAURIItCouldNotAddressFrom(t *testing.T) {
	s, err := artifact.New(newMemory(), "acme", agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		uri  agk.URI
	}{
		{name: "no name", uri: uri("normalize", "ok", "")},
		{name: "a name carrying a separator", uri: uri("normalize", "ok", "nested/file.txt")},
		{name: "a name spelt dot dot", uri: uri("normalize", "ok", "..")},
		{name: "no run", uri: agk.URI{Step: "normalize", Port: "ok", Name: "a.txt"}},
		{name: "a step that is not an identifier", uri: uri("in.raw", "ok", "a.txt")},
		{name: "a port that is not an identifier", uri: uri("normalize", "my port", "a.txt")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.Put(t.Context(), tt.uri, "", strings.NewReader("hello")); err == nil {
				t.Fatalf("Put: accepted %v", tt.uri)
			}
		})
	}
}

func TestOpenReadsTheBytesBackThroughTheDigest(t *testing.T) {
	s, err := artifact.New(newMemory(), "acme", agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	file, err := s.Put(t.Context(), uri("normalize", "ok", "greeting.txt"), "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := s.Open(t.Context(), file)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("read %q, want %q", got, "hello")
	}
}

func TestOpenRefusesWhatTheDigestDoesNotDescribe(t *testing.T) {
	tests := []struct {
		name    string
		file    func(agk.File) agk.File
		corrupt func(*memory, agk.File)
	}{
		{
			name: "a digest that is not sixty-four hexadecimal characters",
			file: func(f agk.File) agk.File { f.SHA256 = "c1f4a91d"; return f },
		},
		{
			name: "a digest in upper case",
			file: func(f agk.File) agk.File { f.SHA256 = strings.ToUpper(f.SHA256); return f },
		},
		{
			name: "a digest nothing is held under",
			file: func(f agk.File) agk.File { f.SHA256 = digestOf([]byte("something else")); return f },
		},
		{
			name: "an object whose bytes are not the ones the digest names",
			corrupt: func(m *memory, f agk.File) {
				m.blobs[artifact.Key("acme", f.SHA256)] = []byte("tampered")
			},
		},
		{
			name: "a size the object does not have",
			file: func(f agk.File) agk.File { f.Size = 4096; return f },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := newMemory()
			s, err := artifact.New(objects, "acme", agk.DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			file, err := s.Put(t.Context(), uri("normalize", "ok", "greeting.txt"), "text/plain", strings.NewReader("hello"))
			if err != nil {
				t.Fatal(err)
			}
			if tt.corrupt != nil {
				tt.corrupt(objects, file)
			}
			if tt.file != nil {
				file = tt.file(file)
			}

			rc, err := s.Open(t.Context(), file)
			if err != nil {
				return // refused before a byte was handed back, which is the earliest it can be
			}
			defer rc.Close()
			if _, err := io.ReadAll(rc); err == nil {
				t.Fatal("read: the bytes came back whole")
			}
		})
	}
}

// Describe is how a caller holds what it is about to publish to the size rules before it
// writes anything, so the entry it answers is the one Put would answer for the same bytes,
// the media type Put fills in included, and nothing reaches the byte layer.
func TestDescribeAnswersWhatPutWouldAndWritesNothing(t *testing.T) {
	objects := newMemory()
	s, err := artifact.New(objects, "acme", agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, mediaType := range []string{"text/csv", ""} {
		described, err := s.Describe(t.Context(), uri("render", "out", "a.csv"), mediaType, strings.NewReader("hello"))
		if err != nil {
			t.Fatalf("Describe: %v", err)
		}
		if len(objects.writes) != 0 {
			t.Fatalf("Describe wrote %v, and it writes nothing", objects.writes)
		}
		put, err := s.Put(t.Context(), uri("render", "out", "a.csv"), mediaType, strings.NewReader("hello"))
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		if described != put {
			t.Errorf("Describe answered %+v and Put %+v for the same bytes", described, put)
		}
		objects.writes = nil
	}
}

// Above artifact_max_bytes Describe refuses exactly as Put does, so a caller that describes
// first learns of the refusal before it has written anything at all.
func TestDescribeRefusesWhatPutWouldRefuse(t *testing.T) {
	limits := agk.DefaultLimits()
	limits.ArtifactMaxBytes = 16
	s, err := artifact.New(newMemory(), "acme", limits)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Describe(t.Context(), uri("render", "out", "big.bin"), "", strings.NewReader(strings.Repeat("x", 1024)))
	var refusal *agk.Refusal
	if !errors.As(err, &refusal) || refusal.Rule != agk.RuleArtifactMaxBytes || refusal.Outcome != agk.Fail {
		t.Errorf("Describe answered %v, and an artifact above artifact_max_bytes is an application failure", err)
	}
}
