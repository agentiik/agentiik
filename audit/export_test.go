package audit

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// log is a chain held in memory, standing in for the database, with the export's cursor.
type log struct {
	mu       sync.Mutex
	entries  []Entry
	through  int64
	hash     []byte
	appended chan struct{}
}

func (l *log) append(actor string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	prev := Genesis
	if n := len(l.entries); n > 0 {
		prev = l.entries[n-1].Hash
	}
	e := Entry{
		Seq: int64(len(l.entries) + 1), At: time.Date(2026, 9, 25, 10, 0, len(l.entries), 123456000, time.UTC),
		Actor: actor, Action: RunCancel, Namespace: "finance", Target: "run", Result: Done, Detail: `{"workflow":"w"}`,
		PrevHash: prev,
	}
	e.Hash = e.Sum()
	l.entries = append(l.entries, e)
}

func (l *log) After(_ context.Context, seq int64, limit int) ([]Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Entry
	for _, e := range l.entries {
		if e.Seq > seq && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (l *log) Exported(context.Context) (int64, []byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hash == nil {
		return 0, Genesis, nil
	}
	return l.through, l.hash, nil
}

func (l *log) MarkExported(_ context.Context, seq int64, hash []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if seq > l.through {
		l.through, l.hash = seq, hash
	}
	return nil
}

// sink is a receiver outside the installation, over TLS, writing down every line it is sent, and
// failing while it is told to.
type sink struct {
	mu      sync.Mutex
	lines   []string
	auth    []string
	failing bool
	server  *httptest.Server
	got     chan struct{}
}

func newSink(t *testing.T) *sink {
	s := &sink{got: make(chan struct{}, 100)}
	s.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		if s.failing || r.Header.Get("Content-Type") != "application/x-ndjson" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		body, _ := io.ReadAll(r.Body)
		scanner := bufio.NewScanner(bytes.NewReader(body))
		for scanner.Scan() {
			s.lines = append(s.lines, scanner.Text())
		}
		s.got <- struct{}{}
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *sink) written() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.lines, "\n")
}

func (s *sink) fail(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing = on
}

func exporter(l *log, s *sink) *Exporter {
	return &Exporter{Source: l, URL: s.server.URL, Token: "sink-token", Client: s.server.Client(), Batch: 3}
}

// Every entry reaches the sink, in batches, each once, with the sink's credential, and a receiver
// verifies what it wrote down from the first entry.
func TestTheExportSendsEveryEntryOnce(t *testing.T) {
	l, s := &log{}, newSink(t)
	for i := range 7 {
		l.append(fmt.Sprintf("principal-%d", i))
	}
	x := exporter(l, s)
	for _, want := range []int{3, 3, 1, 0} {
		if sent, err := x.Once(t.Context()); err != nil || sent != want {
			t.Fatalf("an export sent %d entries, want %d: %v", sent, want, err)
		}
	}
	v, err := VerifyExport(strings.NewReader(s.written()))
	if err != nil || v.First != 1 || v.Last != 7 {
		t.Fatalf("the sink holds entries %d to %d: %v", v.First, v.Last, err)
	}
	if strings.Count(s.written(), "\n")+1 != 7 {
		t.Fatalf("the sink was sent %s", s.written())
	}
	for _, auth := range s.auth {
		if auth != "Bearer sink-token" {
			t.Fatalf("the sink was sent the credential %q", auth)
		}
	}
}

// A sink that is down loses nothing: the cursor stays where it was, and what it missed is sent once
// it is back.
func TestASinkThatWasDownIsSentWhatItMissed(t *testing.T) {
	l, s := &log{}, newSink(t)
	l.append("alice")
	x := exporter(l, s)
	s.fail(true)
	if _, err := x.Once(t.Context()); !errors.Is(err, ErrNotAccepted) {
		t.Fatalf("an export to a sink that refused it answered %v", err)
	}
	if through, _, _ := l.Exported(t.Context()); through != 0 {
		t.Fatalf("the cursor moved to %d on a refusal", through)
	}
	s.fail(false)
	l.append("bob")
	if sent, err := x.Once(t.Context()); err != nil || sent != 2 {
		t.Fatalf("the sink back sent %d entries: %v", sent, err)
	}
	if v, err := VerifyExport(strings.NewReader(s.written())); err != nil || v.Last != 2 {
		t.Fatalf("the sink holds up to %d: %v", v.Last, err)
	}
}

// An entry changed in the database after it was written is not exported, nor anything after it:
// the export sends what comes before the break and stops there.
func TestNothingIsExportedPastABreak(t *testing.T) {
	l, s := &log{}, newSink(t)
	for i := range 4 {
		l.append(fmt.Sprintf("principal-%d", i))
	}
	l.entries[2].Actor = "somebody else"
	x := exporter(l, s)
	x.Batch = 10
	sent, err := x.Once(t.Context())
	var broke *Break
	if !errors.As(err, &broke) || broke.Seq != 3 || sent != 2 {
		t.Fatalf("an export over a break at 3 sent %d and answered %v", sent, err)
	}
	if through, _, _ := l.Exported(t.Context()); through != 2 {
		t.Fatalf("the cursor is at %d", through)
	}
	if sent, err := x.Once(t.Context()); sent != 0 || !errors.As(err, &broke) {
		t.Fatalf("the export went on past the break: %d, %v", sent, err)
	}
}

// Run is continuous: an entry appended while it runs reaches the sink without anything asking, and a
// failure is said and tried again.
func TestTheExportIsContinuous(t *testing.T) {
	l, s := &log{}, newSink(t)
	x := exporter(l, s)
	x.Every = 10 * time.Millisecond
	troubled := make(chan error, 10)
	x.Trouble = func(err error) { troubled <- err }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- x.Run(ctx) }()

	l.append("alice")
	select {
	case <-s.got:
	case <-time.After(5 * time.Second):
		t.Fatal("an entry appended while the export ran never reached the sink")
	}
	s.fail(true)
	l.append("bob")
	select {
	case <-troubled:
	case <-time.After(5 * time.Second):
		t.Fatal("a sink refusing the export was never said")
	}
	s.fail(false)
	select {
	case <-s.got:
	case <-time.After(5 * time.Second):
		t.Fatal("the export was not tried again once the sink was back")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run answered %v when it was stopped", err)
	}
	if v, err := VerifyExport(strings.NewReader(s.written())); err != nil || v.Last != 2 {
		t.Fatalf("the sink holds up to %d: %v", v.Last, err)
	}
}

// A redirect is not followed: the sink is where the configuration says, and a request sent on
// somewhere else, over plaintext perhaps, is not an export.
func TestTheExportFollowsNoRedirect(t *testing.T) {
	l, s := &log{}, newSink(t)
	elsewhere := httptest.NewTLSServer(http.RedirectHandler(s.server.URL, http.StatusPermanentRedirect))
	defer elsewhere.Close()
	l.append("alice")
	x := &Exporter{Source: l, URL: elsewhere.URL}
	x.client().Transport.(*http.Transport).TLSClientConfig.RootCAs = trusted(elsewhere)
	if _, err := x.Once(t.Context()); !errors.Is(err, ErrNotAccepted) {
		t.Fatalf("a redirect answered %v", err)
	}
	if s.written() != "" {
		t.Fatal("the export followed a redirect")
	}
}

// What a receiver wrote down verifies with entries repeated and out of order, and any edit, a
// deletion or two entries under one number is a break.
func TestAnExportIsVerifiedAsAReceiverWroteIt(t *testing.T) {
	l := &log{}
	for i := range 5 {
		l.append(fmt.Sprintf("principal-%d", i))
	}
	line := func(i int) string {
		b, err := l.entries[i].MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	whole := []string{line(0), line(1), line(2), line(3), line(4)}
	if v, err := VerifyExport(strings.NewReader(strings.Join([]string{line(0), line(1), line(2), line(1), line(2), line(4), line(3)}, "\n"))); err != nil || v.First != 1 || v.Last != 5 {
		t.Fatalf("an export sent twice in part verifies as %+v, %v", v, err)
	}
	if v, err := VerifyExport(strings.NewReader(strings.Join(whole[2:], "\n"))); err != nil || v.First != 3 || v.Last != 5 {
		t.Fatalf("the later part of an export verifies as %+v, %v", v, err)
	}
	for name, lines := range map[string][]string{
		"an actor edited":         {whole[0], strings.Replace(whole[1], "principal-1", "principal-9", 1), whole[2]},
		"a detail edited":         {whole[0], strings.Replace(whole[1], `"w\"`, `"x\"`, 1), whole[2]},
		"an entry removed":        {whole[0], whole[2], whole[3]},
		"two entries of a number": {whole[0], whole[1], strings.Replace(whole[1], "principal-1", "principal-9", 1)},
		"the first entry dropped": {whole[1], whole[2]},
	} {
		v, err := VerifyExport(strings.NewReader(strings.Join(lines, "\n")))
		var broke *Break
		if name == "the first entry dropped" {
			// Not a break, but a proof that starts later, which Verified says.
			if err != nil || v.First != 2 {
				t.Errorf("%s verifies as %+v, %v", name, v, err)
			}
			continue
		}
		if !errors.As(err, &broke) {
			t.Errorf("an export with %s verifies as %+v, %v", name, v, err)
		}
	}
}

// trusted is the authority a test server's certificate is signed by.
func trusted(s *httptest.Server) *x509.CertPool {
	return s.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
}

// The export's own client is made once, and every request goes over the one connection it keeps,
// rather than each leaving a transport and an open connection behind.
func TestTheExportKeepsOneConnectionToTheSink(t *testing.T) {
	l := &log{}
	var mu sync.Mutex
	opened := 0
	sink := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	sink.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			mu.Lock()
			opened++
			mu.Unlock()
		}
	}
	sink.StartTLS()
	defer sink.Close()
	x := &Exporter{Source: l, URL: sink.URL, Batch: 1}
	x.client().Transport.(*http.Transport).TLSClientConfig.RootCAs = trusted(sink)
	for i := range 20 {
		l.append(fmt.Sprintf("principal-%d", i))
		if sent, err := x.Once(t.Context()); err != nil || sent != 1 {
			t.Fatalf("export %d sent %d: %v", i, sent, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if opened != 1 {
		t.Fatalf("twenty exports opened %d connections to the sink", opened)
	}
}
