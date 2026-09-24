package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
)

// A step's log, as GET /api/v1/runs/{id}/steps/{step}/logs serves it: "SSE log stream, history
// then live".
//
// A step's log is the logs of its dispatches, since "logs are indexed by run, step, attempt and
// shard" and one key can be handed out again after a loss. The stream sends them one after the
// other, in the order they were made, each whole: what it holds already, then what its runner
// ships while the stream waits, until its log is closed or can no longer be counted on, and then
// the next. One after the other rather than interleaved, because a position in the stream is then
// one place in one log, the dispatch, the chunk and the line, which is what Last-Event-ID carries
// and what "open log streams reconnect elsewhere and resume from their last position" needs;
// interleaved, a position would have to be one per dispatch still open. The cost is that the
// shards of a fan-out read one at a time, the first holding back the others while it runs.
//
// The events, each data line one JSON object:
//
//	dispatch      a dispatch's log begins: task_id, idempotency_key, attempt, shard, requeue
//	line          one line of it: task_id, line, at, text
//	dispatch_end  the stream lets go of it: task_id, lines, truncated, final
//	end           the step's log is over: verdict
//
// attempt and requeue are what tell two dispatches apart for a reader: a new attempt is a retry
// after a failure, under a key of its own, and requeue counts the times one attempt was handed out
// again after a loss, under the same key and a new task_id. final is false where the stream let
// go of a log its runner never closed: lost, or stopped and silent past logSettling.
//
// A line and a dispatch event carry an id, <task_id>/<seq>/<line>, the dispatch event 0 and 0,
// which is everything a reconnect has to name. dispatch_end and end carry none, and a reconnect
// between a dispatch_end and what follows it is sent that dispatch_end again.
//
// Nothing asks the database on a clock per stream. A shipment notifies in the transaction that
// records its chunk (db.LogChannel), one connection per API listens for every stream it serves,
// and a stream reads only when its step was named, at most once per logPace, or when the listener
// sweeps, every logSweep: that is what tells it of what no shipment announces, a dispatch that
// ended or a step that reached its verdict, and what makes a notification it missed cost latency
// rather than lines.

// streamTiming is how a stream spends its time, an argument so that a test has short ones.
type streamTiming struct {
	// sweep is how often every stream reads again with no notification, pace the least time
	// between two reads of one stream however often its step is named, keepAlive how long a
	// stream goes without a byte before it sends a comment, reauthorise how often it asks again
	// whether its caller may read it, and settling how long a dispatch the control plane stopped
	// is waited for, from its ending, for the chunk that closes its log.
	sweep, pace, keepAlive, reauthorise, settling time.Duration
}

// The defaults.
//
// logSweep bounds how late a stream hears that its step is over. Five seconds is half a heartbeat
// interval, and one read of a few rows every five seconds per open stream is a load the database
// does not notice until streams are counted in thousands.
//
// logPace bounds what a busy fan-out costs a stream: a thousand shards each shipping every second
// name the step a thousand times a second, and the stream reads four times.
//
// logKeepAlive is below the idle timeout of every proxy an API is likely to stand behind, sixty
// seconds for nginx and an AWS load balancer, with room for one to be missed.
//
// logReauthorise is how long a caller who lost run:read keeps reading a stream they opened before:
// "revokes one grant, from the next request", and a stream held for an hour would otherwise be a
// request that never ends.
//
// logSettling is how long a dispatch cancelled or stopped at its deadline is waited for: its
// runner sends the container SIGTERM, then SIGKILL after stop_grace, thirty seconds by default,
// and only then ships the chunk that closes the log. Twice that covers a container that takes its
// whole grace and a runner that ships after it.
const (
	logSweep       = 5 * time.Second
	logPace        = 250 * time.Millisecond
	logKeepAlive   = 15 * time.Second
	logReauthorise = 30 * time.Second
	logSettling    = time.Minute
)

var defaultStreamTiming = streamTiming{
	sweep: logSweep, pace: logPace, keepAlive: logKeepAlive, reauthorise: logReauthorise, settling: logSettling,
}

// logChunkBatch is how many chunks of a log one read takes, which bounds what a stream holds in
// memory at once: a chunk is at most what one shipment carried, a mebibyte.
const logChunkBatch = 16

// streamedDispatch is a dispatch event's data.
type streamedDispatch struct {
	TaskID         string     `json:"task_id"`
	IdempotencyKey agk.TaskID `json:"idempotency_key"`
	Attempt        int        `json:"attempt"`
	Shard          *agk.Shard `json:"shard,omitempty"`
	Requeue        int        `json:"requeue"`
}

// streamedLine is a line event's data: the line as it is kept, where it sits in its dispatch's log.
type streamedLine struct {
	TaskID string    `json:"task_id"`
	Line   int       `json:"line"`
	At     time.Time `json:"at"`
	Text   string    `json:"text"`
}

// streamedEnd is a dispatch_end event's data: what the API holds of the log.
type streamedEnd struct {
	TaskID    string `json:"task_id"`
	Lines     int    `json:"lines"`
	Truncated bool   `json:"truncated"`
	Final     bool   `json:"final"`
}

// stepLog answers GET /api/v1/runs/{run}/steps/{step}/logs.
//
// Guarded by run:read, which is "see run state, per-step state, timings and log lines", and by
// nothing more: a log is diagnostics, masked before anything was written, and "reading them needs
// run:read; the payload viewer needs run:read_data".
func (s *Server) stepLog(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	run, step := agk.RunID(r.PathValue("run")), agk.Step(r.PathValue("step"))
	if step.Validate() != nil {
		fail(w, http.StatusNotFound, "the run has no step of that name")
		return
	}
	from, err := resumeFrom(r.Header.Get("Last-Event-ID"))
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.objects == nil {
		fail(w, http.StatusServiceUnavailable, "this installation has no object store attached, and a log is read from nowhere else")
		return
	}

	// Followed before the first read, so that a chunk landing between the two wakes the stream
	// rather than falling in the gap.
	wake, leave := s.logs.follow(run, step)
	defer leave()

	var state db.StepLog
	err = s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		state, err = ns.StepLog(ctx, run, step)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoRun):
		fail(w, http.StatusNotFound, "no such thing, or not yours")
		return
	case errors.Is(err, db.ErrNoStep):
		fail(w, http.StatusNotFound, "the run has no step of that name")
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the log could not be read")
		return
	case state.Expired:
		// 410 rather than 404, as an envelope past its retention answers: the log existed, and
		// the run still shows how many lines it held.
		fail(w, http.StatusGone, "the logs of that run are past the retention its workflow declared, and are deleted with it")
		return
	}
	f := &follower{server: s, namespace: over.Namespace, run: run, step: step, done: map[string]bool{}, stopped: map[string]time.Time{}}
	if from.row != "" && !f.resume(state, from) {
		fail(w, http.StatusBadRequest, "Last-Event-ID names no dispatch of this step, and a stream resumes from a position it gave")
		return
	}

	// "text/event-stream", and nothing in between allowed to keep it: no cache, and no proxy
	// buffering, which nginx does to a response unless told otherwise and which would hold back a
	// live log until the buffer filled.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	// A server that bounds how long an answer may take would cut every stream at that bound.
	// None of the API's does, and one that cannot lift it answers ErrNotSupported, which leaves
	// it as it was.
	rc.SetWriteDeadline(time.Time{})
	w.WriteHeader(http.StatusOK)
	out := &eventStream{w: w, rc: rc}
	if out.flush() != nil {
		return
	}
	f.stream(r.Context(), out, wake, Still(r))
}

// position is where a reader stands in a step's log: the dispatch, the chunk and the line it had
// last, 0 and 0 where it had that dispatch's event and none of its lines.
type position struct {
	row       string
	seq, line int
}

// resumeID is how a position is written as an event id.
var resumeID = regexp.MustCompile(`^([0-9A-HJKMNP-TV-Z]{1,64})/([0-9]{1,10})/([0-9]{1,10})$`)

// resumeFrom reads a Last-Event-ID, which is empty on a first connection.
func resumeFrom(id string) (position, error) {
	if id == "" {
		return position{}, nil
	}
	m := resumeID.FindStringSubmatch(id)
	if m == nil {
		return position{}, fmt.Errorf("Last-Event-ID is %.100q, and a stream resumes from an id it gave, written task_id/seq/line", id)
	}
	seq, errSeq := strconv.Atoi(m[2])
	line, errLine := strconv.Atoi(m[3])
	if errSeq != nil || errLine != nil || seq >= math.MaxInt32 || line >= math.MaxInt32 {
		return position{}, fmt.Errorf("Last-Event-ID is %.100q, whose chunk or line is past any a log holds", id)
	}
	return position{row: m[1], seq: seq, line: line}, nil
}

func (p position) id() string { return fmt.Sprintf("%s/%d/%d", p.row, p.seq, p.line) }

// follower is one stream's place in its step's log.
type follower struct {
	server    *Server
	namespace string
	run       agk.RunID
	step      agk.Step

	// done are the dispatches the stream has let go of, and at the one it follows now: its
	// dispatch event sent or not, the last chunk read whole, and the last line sent.
	done      map[string]bool
	at        string
	announced bool
	after     int
	line      int

	// stopped is when the stream first found a dispatch stopped with its log open, where its row
	// says no moment it ended, which logSettling is counted from.
	stopped map[string]time.Time
}

// resume places the stream where a reconnecting reader stood: every dispatch made before theirs
// let go of, and theirs from the line after the one they had. It answers false for a dispatch the
// step does not have.
func (f *follower) resume(state db.StepLog, from position) bool {
	for _, d := range state.Dispatches {
		if d.Row == from.row {
			f.at, f.announced, f.after, f.line = d.Row, true, max(from.seq-1, 0), from.line
			return true
		}
		f.done[d.Row] = true
	}
	clear(f.done)
	return false
}

// stream sends the step's log until it is over, the reader goes, the API stops, or the reader may
// no longer read it. Only the first ends with an end event: a reader cut off otherwise reconnects,
// elsewhere where the API stopped, and is answered what any request would be.
func (f *follower) stream(ctx context.Context, out *eventStream, wake <-chan struct{}, still func(context.Context) (bool, error)) {
	timing := f.server.streaming
	keepAlive := time.NewTimer(timing.keepAlive)
	defer keepAlive.Stop()
	reauthorise := time.NewTicker(timing.reauthorise)
	defer reauthorise.Stop()

	for {
		over, verdict, err := f.advance(ctx, out)
		if err != nil {
			return
		}
		if over {
			out.send("end", "", map[string]agk.Verdict{"verdict": verdict})
			out.flush()
			return
		}
		if out.flush() != nil {
			return
		}
		if out.sent {
			out.sent = false
			keepAlive.Reset(timing.keepAlive)
		}
		read := time.Now()

	waiting:
		for {
			select {
			case <-ctx.Done():
				return
			case <-f.server.stopping:
				return
			case <-reauthorise.C:
				if allowed, err := still(ctx); err != nil || !allowed {
					return
				}
			case <-keepAlive.C:
				out.comment("keep-alive")
				if out.flush() != nil {
					return
				}
				keepAlive.Reset(timing.keepAlive)
			case <-wake:
				if rest := timing.pace - time.Since(read); rest > 0 {
					paced := time.NewTimer(rest)
					select {
					case <-ctx.Done():
						paced.Stop()
						return
					case <-f.server.stopping:
						paced.Stop()
						return
					case <-paced.C:
					}
				}
				break waiting
			}
		}
	}
}

// advance sends what the step's log holds past where the stream stands, dispatch after dispatch,
// until it reaches one whose log may still grow. It answers whether the step's log is over, with
// the step's verdict, and an error where the stream cannot go on.
func (f *follower) advance(ctx context.Context, out *eventStream) (bool, agk.Verdict, error) {
	for {
		var state db.StepLog
		var chunks []db.LogChunk
		var head *db.Dispatch
		err := f.server.pool.In(ctx, f.namespace, func(ctx context.Context, ns *db.NS) error {
			var err error
			if state, err = ns.StepLog(ctx, f.run, f.step); err != nil {
				return err
			}
			if head = f.next(state); head == nil {
				return nil
			}
			after := 0
			if head.Row == f.at {
				after = f.after
			}
			chunks, err = ns.LogChunks(ctx, head.Row, after, logChunkBatch)
			return err
		})
		switch {
		case err != nil:
			return false, 0, err
		case state.Expired:
			return false, 0, errors.New("api: the logs of the run went past their retention while a stream read them")
		case head == nil:
			return state.Over(), state.Verdict, nil
		}

		if head.Row != f.at {
			f.at, f.announced, f.after, f.line = head.Row, false, 0, 0
		}
		if !f.announced {
			out.send("dispatch", position{row: head.Row}.id(), streamedDispatch{
				TaskID: head.Row, IdempotencyKey: head.Task, Attempt: head.Attempt, Shard: head.Shard, Requeue: head.Requeue,
			})
			f.announced = true
		}
		for _, c := range chunks {
			lines, err := ReadLogChunk(ctx, f.server.objects, c)
			if err != nil {
				return false, 0, err
			}
			for i, l := range lines {
				n := c.FirstLine + i
				if n <= f.line {
					continue
				}
				out.send("line", position{row: head.Row, seq: c.Seq, line: n}.id(), streamedLine{
					TaskID: head.Row, Line: n, At: l.At, Text: l.Text,
				})
				f.line = n
			}
			f.after = c.Seq
		}
		if out.flush() != nil {
			return false, 0, out.err
		}
		if len(chunks) == logChunkBatch {
			continue
		}
		if !f.lettingGo(*head) {
			return false, 0, nil
		}
		out.send("dispatch_end", "", streamedEnd{TaskID: head.Row, Lines: head.Lines, Truncated: head.Truncated, Final: head.FinalSeq != 0})
		f.done[head.Row] = true
	}
}

// next is the first dispatch the stream has not let go of, in the order they were made.
func (f *follower) next(state db.StepLog) *db.Dispatch {
	for i := range state.Dispatches {
		if !f.done[state.Dispatches[i].Row] {
			return &state.Dispatches[i]
		}
	}
	return nil
}

// lettingGo says whether the stream is done with a dispatch whose chunks it has read as far as the
// index went when it last read where the dispatch stands.
//
// A log its runner closed is done once its last chunk was read, which the index held by then,
// since the chunk that closes a log is recorded in the transaction that says so. One still open
// is waited for while the dispatch is going. Once it has ended, what it can still ship depends on
// how:
//
//   - never redeemed, it ran nowhere and nothing ships;
//   - succeeded or failed, its runner closes the log before it reports, so a log still open is
//     one nobody will close, as for an ending a host re-reported from the record of an earlier
//     dispatch of the key;
//   - lost, its host has been silent for three heartbeat intervals. It may only be cut off and
//     ship later, and what it ships then is in the history of the next request, but a stream that
//     waited for it would hold back the requeue that replaced it, which is the one being read;
//   - cancelled or stopped at its deadline, its container is being stopped and its runner ships
//     the last lines once it has, which logSettling waits for.
func (f *follower) lettingGo(d db.Dispatch) bool {
	switch {
	case d.FinalSeq != 0:
		return true
	case !d.State.Terminal():
		return false
	case !d.Bound:
		return true
	case d.State != agk.TaskCancelled && d.State != agk.TaskTimedOut:
		return true
	}
	ended := d.FinishedAt
	if ended.IsZero() {
		if ended = f.stopped[d.Row]; ended.IsZero() {
			ended = f.server.now()
			f.stopped[d.Row] = ended
		}
	}
	return !f.server.now().Before(ended.Add(f.server.streaming.settling))
}

// eventStream writes server-sent events, and remembers the first write that failed, after which it
// writes nothing: a reader that went is not written to again.
type eventStream struct {
	w    http.ResponseWriter
	rc   *http.ResponseController
	err  error
	sent bool
}

// send writes one event, its data one line of JSON, which escapes every line break a text can
// carry, so that no line of a log ends the event it is in.
func (e *eventStream) send(event, id string, data any) {
	if e.err != nil {
		return
	}
	body, err := json.Marshal(data)
	if err != nil {
		e.err = err
		return
	}
	var b bytes.Buffer
	if id != "" {
		b.WriteString("id: " + id + "\n")
	}
	b.WriteString("event: " + event + "\ndata: ")
	b.Write(body)
	b.WriteString("\n\n")
	_, e.err = e.w.Write(b.Bytes())
	e.sent = true
}

// comment writes a line no client dispatches, which keeps an idle connection from being taken for
// a dead one by whatever stands between the reader and the API.
func (e *eventStream) comment(text string) {
	if e.err == nil {
		_, e.err = e.w.Write([]byte(": " + text + "\n\n"))
	}
}

func (e *eventStream) flush() error {
	if e.err == nil {
		e.err = e.rc.Flush()
	}
	return e.err
}

// logWatch is what tells the streams one API serves that their step's log moved on: one
// connection listening for all of them, held while at least one is open.
type logWatch struct {
	pool  *db.Pool
	sweep time.Duration

	mu      sync.Mutex
	readers map[stepOf]map[chan struct{}]bool
	stop    context.CancelFunc
}

type stepOf struct {
	run  agk.RunID
	step agk.Step
}

// follow wakes the channel it answers whenever the step's log may have moved on, until the
// function it answers is called.
func (h *logWatch) follow(run agk.RunID, step agk.Step) (<-chan struct{}, func()) {
	wake := make(chan struct{}, 1)
	k := stepOf{run: run, step: step}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.readers == nil {
		h.readers = map[stepOf]map[chan struct{}]bool{}
	}
	if h.readers[k] == nil {
		h.readers[k] = map[chan struct{}]bool{}
	}
	h.readers[k][wake] = true
	if h.stop == nil {
		ctx, cancel := context.WithCancel(context.Background())
		h.stop = cancel
		go h.listen(ctx)
	}
	return wake, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.readers[k], wake)
		if len(h.readers[k]) == 0 {
			delete(h.readers, k)
		}
		if len(h.readers) == 0 && h.stop != nil {
			h.stop()
			h.stop = nil
		}
	}
}

// listen holds the listening connection until ctx is done, and takes another when one fails,
// sweeping in between, since a notification sent while nothing listened is one nobody heard.
func (h *logWatch) listen(ctx context.Context) {
	for {
		h.pool.WatchLogs(ctx, h.sweep, h.wake)
		if ctx.Err() != nil {
			return
		}
		h.wake("")
		pause := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			pause.Stop()
			return
		case <-pause.C:
		}
	}
}

// wake nudges the streams of the step a key names, or every stream for the empty key, which is a
// sweep, or for a payload that names no step, which a sweep finds whatever it meant.
func (h *logWatch) wake(key agk.TaskID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	nudge := func(c chan struct{}) {
		select {
		case c <- struct{}{}:
		default:
		}
	}
	if key != "" {
		if run, step, _, _, err := agk.ParseTaskID(string(key)); err == nil {
			for c := range h.readers[stepOf{run: run, step: step}] {
				nudge(c)
			}
			return
		}
	}
	for _, readers := range h.readers {
		for c := range readers {
			nudge(c)
		}
	}
}
