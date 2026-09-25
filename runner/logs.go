package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/driver"
)

// Shipping a task's log to the API while its container runs.
//
// "A task's log is its container's standard error, masked on the runner and shipped to the API in
// chunks by POST /api/v1/tasks/logs while the container runs." The driver writes the log, masked,
// timestamped, indexed and capped, into the sink it is given, and on a server runner that sink is
// the task's shipment: it keeps the lines of standard error, ships them a chunk at a time while the
// container runs, and ships the closing chunk once the container has exited and before the result
// is published, "so a finished task's log is whole by the time its step is judged". Masking happens
// in the driver, before the sink, so no value the task redeemed reaches a chunk.
//
// The result's log is the API's last answer and never this runner's own count: "taking all three
// from this answer is what keeps the result and the store saying the same thing about one log".
//
// A chunk is shipped again, as it was, until it is answered, since the key and the seq together name
// one chunk and a chunk the API already holds is answered again rather than appended twice. One
// chunk is on its way at a time: the API takes chunks in order and only in order, so a second one
// sent before the first is answered would only be kept nowhere and sent again.

// logsPath is the route, as the page and the router spell it.
const logsPath = "/api/v1/tasks/logs"

// grantHeader is where a shipment carries its task's grant: "with the task's grant in the
// Agentiik-Grant header and never in the body", since a token written into a body is a token that
// ends up in the very log the body carries.
const grantHeader = "Agentiik-Grant"

// The most one shipment carries, as the API takes it: a mebibyte and 4,096 lines. A chunk leaves
// chunkFraming of its bytes to what surrounds its lines, the key, the seq, the first line and the
// flag, which is a few hundred bytes at most.
const (
	chunkMaxBytes = 1 << 20
	chunkMaxLines = 4096
	chunkFraming  = 1 << 10
)

// shipEvery is how often the lines a running container wrote are shipped. A second is what makes a
// step readable while it runs without a request a line: a chatty container fills a chunk well
// within it, and a full chunk goes at once.
const shipEvery = time.Second

// closeWithin is how long the closing chunk is shipped again for, once the container has exited and
// the API does not answer.
//
// The result waits on the closing chunk, and the result is what the run waits on, while "a log is a
// diagnostic and not a payload". So the wait is bounded, and the bound is the silence after which
// the controller declares a runner's tasks lost: an API that has answered nothing for that long has
// answered no heartbeat either, and a result held longer for a log would be the result of a dispatch
// the controller has already given up on. Past it the result is published with the log the API last
// said it holds, reported truncated, since the store holds less than the container wrote.
const closeWithin = 3 * HeartbeatInterval

// probeSeq is the seq a shipment asks where a log stands with, which is a chunk past any gap: "a
// runner that lost the answer to a shipment ships that chunk again and reads where it stands from
// here, which is why resuming needs no separate route to ask". It is the highest the API's index
// holds but one, which the closing chunk takes where the log was already cut.
const probeSeq = math.MaxInt32 - 2

// LogShipment is one chunk of a task's log, $defs/logShipment/request.
type LogShipment struct {
	IdempotencyKey string    `json:"idempotency_key"`
	Seq            int       `json:"seq"`
	FirstLine      int       `json:"first_line"`
	Final          bool      `json:"final"`
	Lines          []LogLine `json:"lines"`
}

// LogLine is one line of a chunk: when the runner read it, and what it said once masked.
type LogLine struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// LogShipped is what a shipment is answered, $defs/logShipment/response.
type LogShipped struct {
	URI       string `json:"uri"`
	Accepted  int    `json:"accepted"`
	NextSeq   int    `json:"next_seq"`
	Lines     int    `json:"lines"`
	Truncated bool   `json:"truncated"`
}

// LogShipper ships one chunk of a task's log, which is Client.
type LogShipper interface {
	ShipLog(ctx context.Context, grant string, s LogShipment) (LogShipped, error)
}

// ShipLog ships one chunk of a task's log, the grant in its header, and answers where the log
// stands.
//
// An answer is taken as the API's only once it addresses the log of the key the chunk names and
// says where the log goes on, which nothing but the API that read the grant can say; anything else
// answered 200, a proxy's page or a body cut short, is an error of no class, which the shipment
// ships again as it would a chunk that got no answer.
func (c *Client) ShipLog(ctx context.Context, grant string, s LogShipment) (LogShipped, error) {
	var a LogShipped
	if err := c.do(ctx, http.MethodPost, logsPath, http.Header{grantHeader: {grant}}, s, &a); err != nil {
		return LogShipped{}, err
	}
	uri, err := agk.ParseLogURI(a.URI)
	switch {
	case err != nil:
		return LogShipped{}, fmt.Errorf("runner: POST %s: the answer addresses no log: %w", logsPath, err)
	case string(uri.Task) != s.IdempotencyKey:
		return LogShipped{}, fmt.Errorf("runner: POST %s: the answer addresses the log of %s, and the chunk was one of %s", logsPath, uri.Task, s.IdempotencyKey)
	case a.NextSeq < 1 || a.Lines < 0 || a.Accepted < 0:
		return LogShipped{}, fmt.Errorf("runner: POST %s: the answer says the log goes on at chunk %d holding %d lines, which is no place a log stands", logsPath, a.NextSeq, a.Lines)
	}
	return a, nil
}

// TaskLogs is the driver.Logs of a server runner. A task's sink is the shipment its carrier opened
// for it, which comes with the context the task is run in, as its sources do: the grant that scopes
// a shipment is the task's own, and two tasks the runner holds at once ship two logs.
type TaskLogs struct{}

// OpenLog answers with the task's shipment and starts shipping it.
func (TaskLogs) OpenLog(ctx context.Context, task agk.TaskID) (io.WriteCloser, error) {
	s, _ := ctx.Value(shipmentKey{}).(*shipment)
	if s == nil || s.key != string(task) {
		return nil, fmt.Errorf("runner: task %s was run with no shipment for its log, which its carrier opens before the driver runs it", task)
	}
	if err := s.open(); err != nil {
		return nil, err
	}
	return s, nil
}

// shipmentKey is where a task's shipment rides in the context the driver runs it with.
type shipmentKey struct{}

// withShipment is ctx carrying s, for TaskLogs to find.
func withShipment(ctx context.Context, s *shipment) context.Context {
	return context.WithValue(ctx, shipmentKey{}, s)
}

// shipment is one delivery's log of one task, on its way to the API.
type shipment struct {
	to    LogShipper
	key   string
	grant string
	say   func(string)
	every time.Duration
	// within is closeWithin, and retry the first wait before a chunk that got no answer is shipped
	// again, retryFirst, both of which a test shortens.
	within, retry time.Duration

	// ctx is what every request is made under, ended by abandon. It does not end with the task's
	// own context, since the closing chunk is shipped once the task is over.
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	partial []byte
	pending []pendingLine
	// seq and line are where the next chunk goes: its seq, and the index of its first line.
	seq, line int
	// sent is the chunk on its way, which is shipped again as it is until it is answered.
	sent *LogShipment
	// answer is the API's last answer, which the result's log is.
	answer *LogShipped
	// known says the API has answered a chunk of this delivery, so a conflict is not the log of an
	// earlier delivery of the task, and probed that it was already asked where the log stands.
	known, probed bool
	// cut is the API saying the cap was reached: nothing more is shipped but the closing chunk.
	cut bool
	// closed is the closing chunk answered, and refused a refusal no chunk can get past.
	closed, refused bool
	// resumed is a log this delivery went on with past lines it did not ship: what the container
	// wrote while no agent read it is not in the store, or, where the driver read the log back
	// from the daemon, is there twice.
	resumed bool
	// ended is what finish answered, which it answers again.
	ended          *bus.Log
	finished       bool
	opened, sealed bool
	giveUpAt       time.Time

	closing, quit       chan struct{}
	done                chan struct{}
	closeOnce, quitOnce sync.Once
}

// pendingLine is a line not yet in a chunk, and how many bytes it takes in one.
type pendingLine struct {
	LogLine
	size int
}

// newShipment is the shipment of the log of the task message m names, shipped through to.
func newShipment(ctx context.Context, to LogShipper, m bus.TaskMessage, say func(string), every, within time.Duration) *shipment {
	if every <= 0 {
		every = shipEvery
	}
	if within <= 0 {
		within = closeWithin
	}
	if say == nil {
		say = func(string) {}
	}
	s := &shipment{
		to: to, key: m.IdempotencyKey, grant: m.Grant, say: say, every: every, within: within, retry: retryFirst,
		seq: 1, line: 1,
		closing: make(chan struct{}), quit: make(chan struct{}), done: make(chan struct{}),
	}
	s.ctx, s.cancel = context.WithCancel(context.WithoutCancel(ctx))
	return s
}

// open starts shipping, once: a run opens its task's log once.
func (s *shipment) open() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.opened {
		return fmt.Errorf("runner: the log of task %s was opened twice by one delivery, and one delivery ships one log", s.key)
	}
	s.opened = true
	go s.run()
	return nil
}

// Write takes what the driver wrote, one JSON line a line of the log, and keeps the lines of
// standard error to ship. It never blocks on the API: a container whose output nobody reads blocks
// on a full pipe, and the lines kept here are bounded by the runner's own caps, which the driver
// applied before them.
func (s *shipment) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed || s.cut || s.refused {
		return len(p), nil
	}
	s.partial = append(s.partial, p...)
	rest := s.partial
	for {
		n := bytes.IndexByte(rest, '\n')
		if n < 0 {
			break
		}
		s.keep(rest[:n])
		rest = rest[n+1:]
	}
	s.partial = append(s.partial[:0], rest...)
	return len(p), nil
}

// Close is the driver done with the log: the container has exited and its streams were read to
// the end. The closing chunk is the carrier's to order, since only it knows whether the task ended
// or the agent stopped under it, and a log closed then would refuse what a restarted agent ships of
// the same container.
func (s *shipment) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.partial) > 0 && !s.cut && !s.refused {
		s.keep(s.partial)
	}
	s.partial = nil
	s.sealed = true
	return nil
}

// keep adds one line the driver wrote to what is to be shipped.
//
// Only standard error is shipped. The driver writes both of the container's streams into one log,
// so that a person running it locally reads what the container said in the order it said it, and
// the wire takes one of them: "Standard error is the log because standard output belongs to the
// result: a brick may write its out envelope there, and a stream that is sometimes a payload cannot
// also be the diagnostic record." The driver's own notes are written on standard error.
func (s *shipment) keep(raw []byte) {
	var l driver.Line
	if err := json.Unmarshal(raw, &l); err != nil {
		s.say(fmt.Sprintf("runner: a line the driver wrote into the log of task %s is not a line of a log, and is not shipped: %s", s.key, err))
		return
	}
	if l.Stream != driver.Stderr {
		return
	}
	line := LogLine{At: l.At.UTC(), Text: l.Text}
	size := encodedSize(line)
	for size > chunkMaxBytes-chunkFraming {
		// No line the driver writes is this long, since it cuts a line that never ends at
		// 64 KiB, but one that were would never be taken, and would hold back every line
		// after it. It is cut at a character, as the byte cap cuts one.
		n := len(line.Text) / 2
		for n > 0 && !utf8.RuneStart(line.Text[n]) {
			n--
		}
		line.Text = line.Text[:n]
		size = encodedSize(line)
	}
	s.pending = append(s.pending, pendingLine{LogLine: line, size: size})
}

// encodedSize is how many bytes a line takes in a chunk, its comma included.
func encodedSize(l LogLine) int {
	b, err := json.Marshal(l)
	if err != nil {
		return 0
	}
	return len(b) + 1
}

// run ships the log until it is closed, refused, given up on at the close, or abandoned.
func (s *shipment) run() {
	defer close(s.done)
	wait := s.every
	backoff := s.retry
	for {
		if !s.pause(wait) {
			return
		}
		c := s.next()
		if c == nil {
			if s.over() {
				return
			}
			wait = s.every
			continue
		}
		answer, err := s.to.ShipLog(s.ctx, s.grant, *c)
		switch s.answered(c, answer, err) {
		case shipAgain:
			s.say(fmt.Sprintf("runner: chunk %d of the log of task %s got no answer, and is shipped again in %s: %s", c.Seq, s.key, backoff, err))
			wait = backoff
			backoff = min(2*backoff, retryMost)
		case shipNext:
			backoff = s.retry
			wait = s.every
			if s.ready() {
				wait = 0
			}
		case shipNoMore:
			return
		}
	}
}

// What an answer leaves a shipment to do.
const (
	shipNext = iota
	shipAgain
	shipNoMore
)

// pause waits d, cut short by the close being ordered, and answers false where the shipment is to
// stop: abandoned, or closing and past the time the close is waited for.
func (s *shipment) pause(d time.Duration) bool {
	wake := s.closing
	if s.isClosing() {
		left := time.Until(s.giveUp())
		if left <= 0 {
			return false
		}
		d, wake = min(d, left), nil
	}
	if d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-wake:
		case <-s.quit:
			return false
		}
	} else {
		select {
		case <-s.quit:
			return false
		default:
		}
	}
	return !s.isClosing() || time.Until(s.giveUp()) > 0
}

// next is the chunk to ship: the one on its way, or one made of the lines kept since, the last one
// once the close is ordered and every line is in it. It is nil where there is nothing to ship yet,
// or nothing more at all.
func (s *shipment) next() *LogShipment {
	s.mu.Lock()
	defer s.mu.Unlock()
	closing := s.isClosing()
	switch {
	case s.closed || s.refused:
		return nil
	case s.sent != nil:
		return s.sent
	case len(s.pending) == 0 && !closing:
		return nil
	}
	n := s.fits()
	c := &LogShipment{IdempotencyKey: s.key, Seq: s.seq, FirstLine: s.line, Lines: make([]LogLine, n)}
	for i, l := range s.pending[:n] {
		c.Lines[i] = l.LogLine
	}
	c.Final = closing && n == len(s.pending)
	s.pending = append(s.pending[:0], s.pending[n:]...)
	s.seq++
	s.line += n
	s.sent = c
	return c
}

// fits is how many of the lines kept go in one chunk.
func (s *shipment) fits() int {
	room := chunkMaxBytes - chunkFraming
	for i, l := range s.pending {
		if i == chunkMaxLines || l.size > room {
			return i
		}
		room -= l.size
	}
	return len(s.pending)
}

// ready says a whole chunk is waiting, or the close is ordered, so the next one goes at once.
func (s *shipment) ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isClosing() || s.sent != nil || s.fits() < len(s.pending) || len(s.pending) == chunkMaxLines
}

// answered takes the API's answer to chunk c, or its refusal, and says what follows.
func (s *shipment) answered(c *LogShipment, a LogShipped, err error) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		var refusal *APIError
		if !errors.As(err, &refusal) || errors.Is(err, ErrUnavailable) {
			return shipAgain
		}
		if errors.Is(err, ErrConflict) && !s.known && !s.probed {
			// The first chunk of this delivery is refused as one the API holds other lines
			// for: an earlier delivery on this host shipped the start of this log, and the
			// agent restarted under the running container. The API is asked where the log
			// stands, with these lines past any gap, where it keeps them nowhere, and this
			// delivery goes on from there. A conflict for any other reason is refused again.
			s.probed = true
			probe := *c
			probe.Seq = probeSeq
			s.sent = &probe
			return shipNext
		}
		s.refused = true
		s.sent = nil
		s.pending = nil
		what := fmt.Sprintf("chunk %d", c.Seq)
		if c.Seq == probeSeq {
			what = "asking where it stands"
		}
		s.say(fmt.Sprintf("runner: the log of task %s is shipped no further, since %s was refused: %s", s.key, what, err))
		return shipNoMore
	}

	s.answer = &a
	s.known = true
	if a.Truncated {
		// "A runner reading true stops shipping", and goes on reading the container.
		s.cut = true
		s.pending = nil
	}
	if a.NextSeq > c.Seq {
		// Taken, or held already from a shipment whose answer was lost.
		s.sent = nil
		s.seq = a.NextSeq
		if c.Final {
			s.closed = true
			return shipNoMore
		}
		return shipNext
	}
	// Kept nowhere: past a gap, or asked where the log stands. The lines of this chunk go
	// where the API says the log goes on, unless the cap has already ended it, and the log
	// is reported truncated, since what lies between is not what the container wrote.
	s.resumed = true
	s.seq, s.line, s.sent = a.NextSeq, a.Lines+1, nil
	lines := c.Lines
	if s.cut {
		lines = []LogLine{}
	}
	if len(lines) > 0 || c.Final {
		s.sent = &LogShipment{IdempotencyKey: s.key, Seq: s.seq, FirstLine: s.line, Final: c.Final, Lines: lines}
		s.seq++
		s.line += len(lines)
	}
	return shipNext
}

// over says nothing more will be shipped.
func (s *shipment) over() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed || s.refused
}

func (s *shipment) isClosing() bool {
	select {
	case <-s.closing:
		return true
	default:
		return false
	}
}

func (s *shipment) giveUp() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.giveUpAt
}

// finish ships the closing chunk, once the container has exited and the task ended, and answers
// with the log the result reports: the API's last answer, truncated where the driver cut the log,
// where the API did, where this delivery went on with a log past lines it did not ship, or where
// the closing chunk was never answered and the store holds less than the container wrote. It
// answers nil for a log that was never opened, since the task reached nothing that writes one, and
// for one the API never answered, since there is no address to copy.
func (s *shipment) finish(cutByDriver bool) *bus.Log {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if !s.opened {
		s.mu.Unlock()
		return nil
	}
	s.closeOnce.Do(func() {
		s.giveUpAt = time.Now().Add(s.within)
		close(s.closing)
	})
	s.mu.Unlock()
	// A request still on its way when the close has been waited for long enough is given up
	// on, which is what bounds the wait whatever the API is doing.
	timer := time.NewTimer(s.within)
	select {
	case <-s.done:
	case <-timer.C:
		s.cancel()
		<-s.done
	}
	timer.Stop()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return s.ended
	}
	s.finished = true
	if !s.closed {
		s.say(fmt.Sprintf("runner: the log of task %s was not closed, so its result reports it truncated at the lines the API holds", s.key))
	}
	if s.answer != nil {
		s.ended = &bus.Log{URI: s.answer.URI, Lines: s.answer.Lines, Truncated: s.answer.Truncated || cutByDriver || s.resumed || !s.closed}
	}
	return s.ended
}

// abandon stops shipping, whatever is on its way, and waits for the shipping to stop. It is what a
// carrier does with a log once its task is reported or given up with the agent: what was not
// shipped by then is not, and a restarted agent goes on with the log of a container it finds
// running.
func (s *shipment) abandon() {
	if s == nil {
		return
	}
	s.quitOnce.Do(func() {
		close(s.quit)
		s.cancel()
	})
	s.mu.Lock()
	opened := s.opened
	s.mu.Unlock()
	if opened {
		<-s.done
	}
}
