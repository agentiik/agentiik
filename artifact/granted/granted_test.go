package granted_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/artifact/granted"
)

// A runner's objects, held to the built-in store as the API serves it: the object routes over a
// directory, behind a real HTTP server, with every principal refused so that what works here works
// because of a signature and nothing else.

const run agk.RunID = "01JMZ8W4K2R7Q0E3N5T9"

// store is the built-in store and what reached it.
type store struct {
	signed *artifact.Signed

	// objects is the directory behind the routes, read directly to say what is held without
	// going through a URL.
	objects artifact.Objects

	mu   sync.Mutex
	sent []string
}

// serve stands the built-in store up under the limits an installation sets, which are the
// defaults when they are the zero value.
func serve(t *testing.T, limits agk.Limits) *store {
	t.Helper()
	// Unstarted, so that its address is known before the presigner that writes it into every URL
	// is built, and the handler is in place before the first request can arrive.
	srv := httptest.NewUnstartedServer(nil)
	s := &store{objects: artifact.Dir(t.TempDir())}
	signed, err := artifact.NewSigned(s.objects, artifact.SignedOptions{
		Key:    []byte("0123456789abcdef0123456789abcdef"),
		Base:   "http://" + srv.Listener.Addr().String() + "/objects",
		Limits: limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := api.NewRouter(api.DenyAll{}, func(*http.Request) (api.Principal, error) { return "", nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewObjects(rt, signed); err != nil {
		t.Fatal(err)
	}
	s.signed = signed
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.sent = append(s.sent, r.Method+" "+r.URL.Path)
		s.mu.Unlock()
		rt.ServeHTTP(w, r)
	})
	srv.Start()
	t.Cleanup(srv.Close)
	return s
}

// requests is what reached the store, in order.
func (s *store) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sent)
}

// holds says whether the directory behind the routes holds a key.
func (s *store) holds(t *testing.T, key string) bool {
	t.Helper()
	held, err := s.objects.Has(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	return held
}

// policy is the upload policy a redemption answers for a task of finance, good for an hour.
func (s *store) policy(t *testing.T) artifact.Policy {
	t.Helper()
	p, err := s.signed.Policy(t.Context(), "finance", run, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// get is the presigned GET a redemption answers for one key, good until an instant.
func (s *store) get(t *testing.T, key string, until time.Time) string {
	t.Helper()
	u, err := s.signed.Presign(t.Context(), artifact.MethodGet, key, run, until)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func objects(t *testing.T, o granted.Options) *granted.Objects {
	t.Helper()
	g, err := granted.New(o)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func digestOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// What one task writes through its policy is what the next reads back through the URL its own
// redemption names, by way of artifact.Store and the envelope helpers, which are what the driver
// and the runner go through.
func TestATaskWritesThroughItsPolicyAndTheNextReadsThroughItsURLs(t *testing.T) {
	s := serve(t, agk.Limits{})
	policy := s.policy(t)
	until := time.Now().UTC().Add(time.Hour)

	const content = "the whole of an invoice"
	key := artifact.Key("finance", digestOf(content))
	writer, err := artifact.New(objects(t, granted.Options{Uploads: policy}), "finance", agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	f, err := writer.Put(t.Context(), agk.URI{Run: run, Step: "invoice", Port: "out", Name: "invoice.pdf"}, "application/pdf", strings.NewReader(content))
	if err != nil {
		t.Fatalf("storing an artifact under the policy: %s", err)
	}
	if f.SHA256 != digestOf(content) || !s.holds(t, key) {
		t.Fatalf("the store answered %s and holds it: %t", f.SHA256, s.holds(t, key))
	}

	envelope := agk.Empty(run, "invoice", "out", 1, time.Date(2026, 9, 10, 6, 0, 0, 0, time.UTC))
	item := agk.NewItem(map[string]any{"invoice": "INV-2026-0917"})
	item.Files = []agk.File{f}
	envelope.Items = []agk.Item{item}
	envelope.Meta.Count = 1
	digest, _, err := writer.PutEnvelope(t.Context(), envelope)
	if err != nil {
		t.Fatalf("storing the envelope under the policy: %s", err)
	}

	// The next task is told of both by its own redemption, and reads them through their URLs.
	next := objects(t, granted.Options{
		Get: map[string]string{
			artifact.Key("finance", digest): s.get(t, artifact.Key("finance", digest), until),
			key:                             s.get(t, key, until),
		},
		Uploads: policy,
	})
	back, err := artifact.GetEnvelope(t.Context(), next, "finance", digest, agk.DefaultLimits())
	if err != nil {
		t.Fatalf("the envelope does not read back through its URL: %s", err)
	}
	if back.Meta.Count != 1 || len(back.Items) != 1 || len(back.Items[0].Files) != 1 {
		t.Fatalf("the envelope reads back as %d items", back.Meta.Count)
	}
	reader, err := artifact.New(next, "finance", agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	rc, err := reader.Open(t.Context(), back.Items[0].Files[0])
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if got, err := io.ReadAll(rc); err != nil || string(got) != content {
		t.Errorf("the artifact reads back as %q: %v", got, err)
	}

	// Has knows nothing, so the same bytes are posted again, and the store takes them again.
	before := len(s.requests())
	if _, err := writer.Put(t.Context(), agk.URI{Run: run, Step: "archive", Port: "out", Name: "copy.pdf"}, "application/pdf", strings.NewReader(content)); err != nil {
		t.Fatalf("posting an object the store already holds: %s", err)
	}
	if sent := s.requests()[before:]; !slices.Equal(sent, []string{"POST /objects/finance"}) {
		t.Errorf("storing bytes the store already held sent %q", sent)
	}
}

// The bytes are held to the key as they arrive, and a key they do not hash to stores nothing.
func TestBytesThatDoNotHashToTheirKeyAreRefusedAndNothingIsStored(t *testing.T) {
	s := serve(t, agk.Limits{})
	o := objects(t, granted.Options{Uploads: s.policy(t)})

	key := artifact.Key("finance", digestOf("an invoice nobody made"))
	err := o.Put(t.Context(), key, strings.NewReader("something else entirely"))
	if !errors.Is(err, artifact.ErrWrongDigest) {
		t.Fatalf("posting bytes that are not the object answered %v", err)
	}
	if s.holds(t, key) {
		t.Error("a refused post left something behind")
	}
}

// A policy stops working when its grant does, and covers its prefix and nothing else. A key it
// does not cover is refused before anything is sent. A policy that claims a prefix it was not
// signed for gets as far as the store, which refuses it.
func TestAnExpiredPolicyAndAKeyOutsideItsPrefixAreRefused(t *testing.T) {
	s := serve(t, agk.Limits{})
	const content = "the whole of an invoice"
	digest := digestOf(content)

	expired, err := s.signed.Policy(t.Context(), "finance", run, time.Now().UTC().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	key := artifact.Key("finance", digest)
	err = objects(t, granted.Options{Uploads: expired}).Put(t.Context(), key, strings.NewReader(content))
	if !errors.Is(err, artifact.ErrNotSigned) {
		t.Errorf("posting under an expired policy answered %v", err)
	}
	if s.holds(t, key) {
		t.Error("a post under an expired policy stored the object")
	}

	before := len(s.requests())
	o := objects(t, granted.Options{Uploads: s.policy(t)})
	for _, outside := range []string{
		artifact.Key("ops", digest),
		artifact.Key("finance", strings.ToUpper(digest)),
		artifact.Key("finance", digest[:63]),
		"finance/sha256/invoice.pdf",
	} {
		if err := o.Put(t.Context(), outside, strings.NewReader(content)); !errors.Is(err, artifact.ErrNotSigned) {
			t.Errorf("posting under %s answered %v", outside, err)
		}
	}
	// A Store opened for another namespace builds keys the policy does not cover.
	ops, err := artifact.New(o, "ops", agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ops.Put(t.Context(), agk.URI{Run: run, Step: "invoice", Port: "out", Name: "invoice.pdf"}, "", strings.NewReader(content)); !errors.Is(err, artifact.ErrNotSigned) {
		t.Errorf("a store of ops over a policy of finance answered %v", err)
	}
	if sent := s.requests()[before:]; len(sent) != 0 {
		t.Errorf("keys the policy does not cover were sent: %q", sent)
	}

	// The prefix a runner is told is not what the store checks: the store checks what it signed.
	claimed := s.policy(t)
	claimed.KeyPrefix = artifact.Prefix("ops")
	err = objects(t, granted.Options{Uploads: claimed}).Put(t.Context(), artifact.Key("ops", digest), strings.NewReader(content))
	if !errors.Is(err, artifact.ErrNotSigned) {
		t.Errorf("posting under a prefix the policy was not signed for answered %v", err)
	}
	if s.holds(t, artifact.Key("ops", digest)) {
		t.Error("a post under a prefix the policy was not signed for stored the object")
	}
}

// A task reads what its redemption named and nothing else, and asking for anything else sends
// nothing: the API resolved what the task may read, and a runner that asked for more would be
// reaching past it.
func TestAKeyWithNoURLIsRefusedWithoutARequest(t *testing.T) {
	s := serve(t, agk.Limits{})
	const content = "the whole of an invoice"
	key := artifact.Key("finance", digestOf(content))
	if err := s.signed.Store(t.Context(), key, strings.NewReader(content)); err != nil {
		t.Fatal(err)
	}

	o := objects(t, granted.Options{Uploads: s.policy(t)})
	_, err := o.Open(t.Context(), key)
	if !errors.Is(err, granted.ErrNotGranted) {
		t.Errorf("reading a key with no URL answered %v", err)
	}
	// Not absence: the store holds it, and a caller reading absence would take it as gone.
	if errors.Is(err, fs.ErrNotExist) {
		t.Error("a key with no URL reads as an object the store does not hold")
	}
	st, err := artifact.New(o, "finance", agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	f := agk.File{Name: "invoice.pdf", URI: agk.URI{Run: run, Step: "invoice", Port: "out", Name: "invoice.pdf"}, MediaType: "application/pdf", Size: int64(len(content)), SHA256: digestOf(content)}
	if _, err := st.Open(t.Context(), f); !errors.Is(err, granted.ErrNotGranted) {
		t.Errorf("a store reading a file with no URL answered %v", err)
	}
	if sent := s.requests(); len(sent) != 0 {
		t.Errorf("reading keys with no URL sent %q", sent)
	}
}

// What a fetch through a URL the redemption named can be answered, and what each reads as.
func TestWhatAFetchIsAnswered(t *testing.T) {
	s := serve(t, agk.Limits{})
	absent := artifact.Key("finance", digestOf("an invoice nobody stored"))
	const content = "the whole of an invoice"
	held := artifact.Key("finance", digestOf(content))
	if err := s.signed.Store(t.Context(), held, strings.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	o := objects(t, granted.Options{
		Get: map[string]string{
			absent: s.get(t, absent, time.Now().UTC().Add(time.Hour)),
			held:   s.get(t, held, time.Now().UTC().Add(-time.Minute)),
		},
		Uploads: s.policy(t),
	})

	// An object the store does not hold is absent, which is how a caller tells an artifact
	// that is gone from a store that is broken.
	if _, err := o.Open(t.Context(), absent); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("fetching an absent object answered %v", err)
	}
	// A URL that expired is not signed for anything any more, whatever the store holds.
	if _, err := o.Open(t.Context(), held); !errors.Is(err, artifact.ErrNotSigned) {
		t.Errorf("fetching through an expired URL answered %v", err)
	}
}

// artifact_max_bytes is the store's to hold as well as the runner's. Store.Put holds the bytes to
// the runner's limit before anything is sent, so a 413 is a store holding a lower one.
func TestAnObjectAboveTheStoresLimitIsRefusedAsTooLarge(t *testing.T) {
	limits := agk.DefaultLimits()
	limits.ArtifactMaxBytes = 1 << 10
	s := serve(t, limits)
	st, err := artifact.New(objects(t, granted.Options{Uploads: s.policy(t)}), "finance", agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	larger := strings.Repeat("x", 2<<10)
	_, err = st.Put(t.Context(), agk.URI{Run: run, Step: "invoice", Port: "out", Name: "ledger.csv"}, "text/csv", strings.NewReader(larger))
	if !errors.Is(err, artifact.ErrTooLarge) {
		t.Fatalf("posting above the store's artifact_max_bytes answered %v", err)
	}
	if s.holds(t, artifact.Key("finance", digestOf(larger))) {
		t.Error("a post refused as too large left something behind")
	}
}

// A form is the policy's fields, then the key, then the file, last: a store honouring a POST
// policy reads what it checks before the bytes it stores, and stops reading at the file.
func TestAFormIsTheFieldsThenTheKeyThenTheFile(t *testing.T) {
	var mu sync.Mutex
	var parts []string
	var file []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Error(err)
		}
		form := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := form.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Error(err)
				break
			}
			value, _ := io.ReadAll(part)
			parts = append(parts, part.FormName()+"="+string(value))
			if part.FormName() == "file" {
				file = value
			}
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	const content = "the whole of an invoice"
	key := artifact.Key("finance", digestOf(content))
	o := objects(t, granted.Options{Uploads: artifact.Policy{
		URL:       srv.URL + "/objects/finance",
		Fields:    map[string]string{"signature": "9d4b71e0", "run": string(run), "expires": "1789023069"},
		KeyPrefix: artifact.Prefix("finance"),
	}})
	if err := o.Put(t.Context(), key, strings.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	want := []string{"expires=1789023069", "run=" + string(run), "signature=9d4b71e0", "key=" + key, "file=" + content}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(parts, want) {
		t.Errorf("the form was %q, where it is %q", parts, want)
	}
	if !bytes.Equal(file, []byte(content)) {
		t.Errorf("the file part carried %q", file)
	}
}

// A form says how long it is before it is read. MinIO refuses one sent chunked before it looks at
// the policy, so a runner posting chunked would lose every output on a store honouring POST
// policies. Both readers a runner posts can say how long they are, the file Store.Put stages an
// artifact in and the bytes of an envelope, and so can one somebody has already read part of.
func TestAFormSaysHowLongItIs(t *testing.T) {
	type post struct {
		length, received int64
		chunked          bool
	}
	var mu sync.Mutex
	var posts []post
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ := io.Copy(io.Discard, r.Body)
		mu.Lock()
		posts = append(posts, post{length: r.ContentLength, received: received, chunked: slices.Contains(r.TransferEncoding, "chunked")})
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	o := objects(t, granted.Options{Uploads: artifact.Policy{
		URL:       srv.URL + "/objects/finance",
		Fields:    map[string]string{"signature": "9d4b71e0"},
		KeyPrefix: artifact.Prefix("finance"),
	}})
	st, err := artifact.New(o, "finance", agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	const content = "the whole of an invoice"
	if _, err := st.Put(t.Context(), agk.URI{Run: run, Step: "invoice", Port: "out", Name: "invoice.pdf"}, "application/pdf", strings.NewReader(content)); err != nil {
		t.Fatalf("storing an artifact: %s", err)
	}
	if _, _, err := st.PutEnvelope(t.Context(), agk.Empty(run, "invoice", "out", 1, time.Date(2026, 9, 10, 6, 0, 0, 0, time.UTC))); err != nil {
		t.Fatalf("storing an envelope: %s", err)
	}
	partway := strings.NewReader("the invoice " + content)
	io.CopyN(io.Discard, partway, int64(len("the invoice ")))
	if err := o.Put(t.Context(), artifact.Key("finance", digestOf(content)), partway); err != nil {
		t.Fatalf("posting what is left of a reader: %s", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(posts) != 3 {
		t.Fatalf("%d forms were posted, where three were", len(posts))
	}
	for i, p := range posts {
		if p.chunked || p.length != p.received {
			t.Errorf("form %d said it was %d bytes long, chunked %t, and carried %d", i+1, p.length, p.chunked, p.received)
		}
	}
}

// A reader that cannot say how long it is still goes out, chunked, which the built-in store takes.
// A pipe is a file that cannot be asked where it is, and is posted the same way.
func TestAReaderThatCannotSayHowLongItIsIsStillPosted(t *testing.T) {
	s := serve(t, agk.Limits{})
	o := objects(t, granted.Options{Uploads: s.policy(t)})

	const content = "the whole of an invoice"
	key := artifact.Key("finance", digestOf(content))
	if err := o.Put(t.Context(), key, io.MultiReader(strings.NewReader(content))); err != nil {
		t.Fatalf("posting a reader of no known length: %s", err)
	}
	if !s.holds(t, key) {
		t.Error("a reader of no known length stored nothing")
	}

	const piped = "the whole of a ledger"
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	go func() {
		io.WriteString(pw, piped)
		pw.Close()
	}()
	key = artifact.Key("finance", digestOf(piped))
	if err := o.Put(t.Context(), key, pr); err != nil {
		t.Fatalf("posting a pipe: %s", err)
	}
	if !s.holds(t, key) {
		t.Error("a pipe stored nothing")
	}
}

// Whatever else a store answers is its own trouble and none of the refusals, and an error never
// carries the URL it failed on: the URL is the authorisation, and the error goes on to a log.
func TestAStoreInTroubleIsNoneOfTheRefusalsAndNamesNoURL(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()

	const content = "the whole of an invoice"
	key := artifact.Key("finance", digestOf(content))
	const signature = "signature=9d4b71e0"
	for _, base := range []string{failing.URL, gone.URL} {
		o := objects(t, granted.Options{
			Get: map[string]string{key: base + "/objects/" + key + "?run=" + string(run) + "&expires=1789023069&" + signature},
			Uploads: artifact.Policy{
				URL:       base + "/objects/finance",
				Fields:    map[string]string{"signature": "9d4b71e0"},
				KeyPrefix: artifact.Prefix("finance"),
			},
		})
		_, opened := o.Open(t.Context(), key)
		put := o.Put(t.Context(), key, strings.NewReader(content))
		for _, err := range []error{opened, put} {
			if err == nil {
				t.Fatalf("%s answered nothing wrong", base)
			}
			for _, refusal := range []error{artifact.ErrNotSigned, artifact.ErrWrongDigest, artifact.ErrTooLarge, fs.ErrNotExist, granted.ErrNotGranted} {
				if errors.Is(err, refusal) {
					t.Errorf("%s: %v reads as %v", base, err, refusal)
				}
			}
			if strings.Contains(err.Error(), signature) {
				t.Errorf("%s: the error carries the URL: %v", base, err)
			}
		}
	}
}

// A store has stored an object when it answers 201, which is the answer the task message sets out,
// and at no other. S3 answers 204 unless the policy's fields ask it for 201, and a runner taking
// 204 as stored would settle for the presigner of MinIO and S3 what only its fields should say.
func TestOnlyA201IsAnObjectStored(t *testing.T) {
	const content = "the whole of an invoice"
	key := artifact.Key("finance", digestOf(content))
	for _, status := range []int{http.StatusOK, http.StatusNoContent} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			w.WriteHeader(status)
		}))
		o := objects(t, granted.Options{Uploads: artifact.Policy{URL: srv.URL + "/objects/finance", KeyPrefix: artifact.Prefix("finance")}})
		err := o.Put(t.Context(), key, strings.NewReader(content))
		srv.Close()
		if err == nil {
			t.Errorf("a store answering %d was taken to have stored the object", status)
			continue
		}
		for _, refusal := range []error{artifact.ErrNotSigned, artifact.ErrWrongDigest, artifact.ErrTooLarge} {
			if errors.Is(err, refusal) {
				t.Errorf("a store answering %d reads as %v", status, refusal)
			}
		}
	}
}

// Has asks nothing, and still answers a context that is over as one that is over, as every byte
// layer does.
func TestHasAnswersAContextThatIsOver(t *testing.T) {
	o := objects(t, granted.Options{Uploads: artifact.Policy{URL: "https://agentiik.example.com/objects/finance", KeyPrefix: artifact.Prefix("finance")}})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if held, err := o.Has(ctx, artifact.Key("finance", digestOf("the whole of an invoice"))); held || !errors.Is(err, context.Canceled) {
		t.Errorf("asking with a cancelled context answered %t, %v", held, err)
	}
}

// A presigned URL is its own authorisation, and following a redirect would send it on to the next
// host in the Referer header, so a redirect is an answer like any other.
func TestARedirectIsNotFollowed(t *testing.T) {
	var mu sync.Mutex
	followed := false
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mu.Lock()
		followed = true
		mu.Unlock()
	}))
	defer elsewhere.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/objects", http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()

	key := artifact.Key("finance", digestOf("the whole of an invoice"))
	o := objects(t, granted.Options{
		Get:     map[string]string{key: redirecting.URL + "/objects/" + key + "?signature=9d4b71e0"},
		Uploads: artifact.Policy{URL: redirecting.URL + "/objects/finance", KeyPrefix: artifact.Prefix("finance")},
	})
	if _, err := o.Open(t.Context(), key); err == nil {
		t.Error("a redirect was read as the object")
	}
	mu.Lock()
	defer mu.Unlock()
	if followed {
		t.Error("the redirect was followed")
	}
}

// A policy a runner could not post with is refused when the task's objects are built, before any
// container runs, rather than once its outputs have nowhere to go.
func TestNewRefusesAPolicyNothingCouldBePostedWith(t *testing.T) {
	good := artifact.Policy{URL: "https://agentiik.example.com/objects/finance", KeyPrefix: artifact.Prefix("finance")}
	for _, c := range []struct {
		name   string
		policy artifact.Policy
	}{
		{"no policy at all", artifact.Policy{}},
		{"a policy with no URL", artifact.Policy{KeyPrefix: good.KeyPrefix}},
		{"a policy with no key prefix", artifact.Policy{URL: good.URL}},
		{"a policy posted somewhere that is not HTTP", artifact.Policy{URL: "ftp://agentiik.example.com/objects/finance", KeyPrefix: good.KeyPrefix}},
		{"a policy posted to a path alone", artifact.Policy{URL: "/objects/finance", KeyPrefix: good.KeyPrefix}},
		{"a policy posted to no host", artifact.Policy{URL: "https:///objects/finance", KeyPrefix: good.KeyPrefix}},
		{"a policy carrying the key", artifact.Policy{URL: good.URL, KeyPrefix: good.KeyPrefix, Fields: map[string]string{"key": "finance/sha256/"}}},
		{"a policy carrying the file", artifact.Policy{URL: good.URL, KeyPrefix: good.KeyPrefix, Fields: map[string]string{"file": ""}}},
	} {
		if _, err := granted.New(granted.Options{Uploads: c.policy}); err == nil {
			t.Errorf("%s was taken", c.name)
		}
	}
	if _, err := granted.New(granted.Options{Uploads: good}); err != nil {
		t.Errorf("a policy with a URL and a key prefix was refused: %s", err)
	}
}
