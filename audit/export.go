package audit

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"
)

// The export: every entry, in order, to a sink outside the installation, as soon as it is written.
//
// "It is exported continuously outside the installation, since the incident's own host may be
// unreadable." The sink is an HTTPS endpoint the entries are POSTed to as newline-delimited JSON,
// one entry per line, which is the smallest thing that is both continuous and outside: every log
// collector and SIEM takes an HTTP input of that shape, it needs nothing but the standard library
// here, and it leaves the installation over TLS rather than landing on a disk of the host whose
// integrity is in question. A file would be a copy on that host, and syslog carries no delivery
// answer to move a cursor on.
//
// Delivery is at least once. The cursor moves only once the sink has answered 2xx, so a sink that
// was down is sent everything it missed once it is back, and a controller that stops between the
// answer and the cursor sends a batch again. Each entry carries its number and its hash, so a
// receiver keeps one of each and verifies the chain, as VerifyExport does.
//
// Nothing is exported past a break. The export verifies each batch against the last entry the sink
// accepted before sending it, so an entry changed in the database after it was written, or one
// removed, stops the export at the entry before it, and the copy outside keeps what was true.

// Source is the log as the export reads it: the entries after one, and how far the sink has
// accepted them.
type Source interface {
	After(ctx context.Context, seq int64, limit int) ([]Entry, error)
	Exported(ctx context.Context) (int64, []byte, error)
	MarkExported(ctx context.Context, seq int64, hash []byte) error
}

// Default pacing of the export.
const (
	// DefaultEvery is how often the export looks for new entries when it has sent everything,
	// which is how far behind the database the copy outside is at worst.
	DefaultEvery = time.Second
	// DefaultBatch is how many entries go in one request.
	DefaultBatch = 500
	// retryAtMost is the longest the export waits before trying a sink that failed again: the
	// wait doubles from Every, so that a sink that is down is not hammered and one that is back
	// is sent to within a minute.
	retryAtMost = time.Minute
	// sendWithin bounds one request, so that a sink that accepts the connection and never answers
	// does not stop the export.
	sendWithin = 30 * time.Second
)

// Exporter sends the audit log to a sink outside the installation.
type Exporter struct {
	Source Source

	// URL is the sink, an https URL the entries are POSTed to, and Token, where it is set, is
	// sent as a bearer credential.
	URL   string
	Token string

	// Client sends the requests, and is one that speaks TLS 1.2 at least and follows no
	// redirect where nil: a sink that has moved is configured again, rather than followed
	// somewhere its operator did not name.
	Client *http.Client

	Every time.Duration
	Batch int

	// Trouble is told each time an export fails, and is not given the sink's answer, which could
	// be anything the sink chose to say.
	Trouble func(err error)
}

// ErrNotAccepted is a sink that answered, and did not answer 2xx.
var ErrNotAccepted = errors.New("audit: the sink did not accept the entries")

// Run exports until ctx is done, and answers nil then. A failure is told to Trouble and tried again
// after a wait that doubles, and never ends the export.
func (x *Exporter) Run(ctx context.Context) error {
	every := x.Every
	if every <= 0 {
		every = DefaultEvery
	}
	wait := every
	for {
		sent, err := x.Once(ctx)
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			if x.Trouble != nil {
				x.Trouble(err)
			}
			wait = min(wait*2, retryAtMost)
		case sent == x.batch():
			// A full batch: there may be more behind it, so the next goes at once.
			wait = every
			continue
		default:
			wait = every
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

func (x *Exporter) batch() int {
	if x.Batch <= 0 {
		return DefaultBatch
	}
	return x.Batch
}

// Once sends the entries after the last one the sink accepted, one batch of them, and answers how
// many it sent. Entries before a break are sent, and the break is answered as a *Break.
func (x *Exporter) Once(ctx context.Context) (int, error) {
	seq, hash, err := x.Source.Exported(ctx)
	if err != nil {
		return 0, err
	}
	entries, err := x.Source.After(ctx, seq, x.batch())
	if err != nil {
		return 0, err
	}
	chain := From(seq, hash)
	var broke error
	for i, e := range entries {
		if err := chain.Next(e); err != nil {
			entries, broke = entries[:i], err
			break
		}
	}
	if len(entries) == 0 {
		return 0, broke
	}
	if err := x.send(ctx, entries); err != nil {
		return 0, err
	}
	last, lastHash := chain.Last()
	if err := x.Source.MarkExported(ctx, last, lastHash); err != nil {
		return 0, err
	}
	return len(entries), broke
}

func (x *Exporter) send(ctx context.Context, entries []Entry) error {
	var body bytes.Buffer
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		body.Write(line)
		body.WriteByte('\n')
	}
	ctx, cancel := context.WithTimeout(ctx, sendWithin)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, x.URL, &body)
	if err != nil {
		return fmt.Errorf("audit: the sink's URL cannot be sent to: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	if x.Token != "" {
		req.Header.Set("Authorization", "Bearer "+x.Token)
	}
	answer, err := x.client().Do(req)
	if err != nil {
		return fmt.Errorf("audit: entries %d to %d could not be sent to the sink: %w", entries[0].Seq, entries[len(entries)-1].Seq, err)
	}
	defer answer.Body.Close()
	io.Copy(io.Discard, io.LimitReader(answer.Body, 64<<10))
	if answer.StatusCode/100 != 2 {
		return fmt.Errorf("%w: entries %d to %d were answered %d, and are sent again", ErrNotAccepted, entries[0].Seq, entries[len(entries)-1].Seq, answer.StatusCode)
	}
	return nil
}

func (x *Exporter) client() *http.Client {
	if x.Client != nil {
		return x.Client
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Verified is what VerifyExport found: the entries it verified, from First to Last.
type Verified struct {
	First, Last int64
}

// exportLineMax is the longest line VerifyExport reads, far more than any entry.
const exportLineMax = 1 << 20

// VerifyExport checks an export as a receiver wrote it down: one entry per line, perhaps some of
// them twice, since delivery is at least once, and perhaps out of order, since a batch sent again
// follows the ones after it. An entry repeated identically counts once; one number carried by two
// different entries is a break.
//
// It verifies from the first entry it holds. Where that is entry 1, the chain is proved from its
// start; where the receiver kept only a later part, the first entry's claim about the one before it
// is taken as given, and Verified.First says where the proof starts.
func VerifyExport(r io.Reader) (Verified, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(nil, exportLineMax)
	bySeq := map[int64]Entry{}
	line := 0
	for scanner.Scan() {
		line++
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(text, &e); err != nil {
			return Verified{}, fmt.Errorf("audit: line %d is not an entry: %w", line, err)
		}
		if seen, ok := bySeq[e.Seq]; ok {
			if !bytes.Equal(seen.Hash, e.Hash) || !bytes.Equal(seen.Sum(), e.Sum()) {
				return Verified{}, &Break{Seq: e.Seq, Why: fmt.Sprintf("line %d carries an entry of this number that differs from one before it", line)}
			}
			continue
		}
		bySeq[e.Seq] = e
	}
	if err := scanner.Err(); err != nil {
		return Verified{}, fmt.Errorf("audit: the export could not be read: %w", err)
	}
	if len(bySeq) == 0 {
		return Verified{}, errors.New("audit: the export holds no entry")
	}
	numbers := make([]int64, 0, len(bySeq))
	for seq := range bySeq {
		numbers = append(numbers, seq)
	}
	slices.Sort(numbers)
	first := bySeq[numbers[0]]
	chain := From(first.Seq-1, first.PrevHash)
	if first.Seq == 1 {
		chain = From(0, Genesis)
	}
	for _, seq := range numbers {
		if err := chain.Next(bySeq[seq]); err != nil {
			return Verified{}, err
		}
	}
	last, _ := chain.Last()
	return Verified{First: first.Seq, Last: last}, nil
}
