package driver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/agentiik/agentiik/agk"
)

// Stream is which of a container's two output streams a line came from.
//
// The contract collects standard error as the log and keeps standard output for the
// shorthand, and both are written into the same log so that a person reading it sees
// what the container said in the order it said it. Which stream a line came from is
// therefore a member of the line rather than a choice of sink.
type Stream int

const (
	// Stdout is the stream the single out port's shorthand is read from.
	Stdout Stream = iota + 1

	// Stderr is the stream the contract names as the captured log.
	Stderr
)

// streams spell the two streams as a line carries them.
var streams = [...]string{
	Stdout: "stdout",
	Stderr: "stderr",
}

// String names the stream. The zero value is not one of the two on purpose: a line whose
// stream was never set would otherwise read as standard output, and a diagnostic
// attributed to the payload stream is worse than one attributed to neither.
func (s Stream) String() string {
	if s < 0 || int(s) >= len(streams) || streams[s] == "" {
		return fmt.Sprintf("stream %d", int(s))
	}
	return streams[s]
}

// MarshalText writes the stream as the log line carries it.
func (s Stream) MarshalText() ([]byte, error) {
	if s < 0 || int(s) >= len(streams) || streams[s] == "" {
		return nil, fmt.Errorf("%d is not one of the two streams a container writes on, stdout and stderr", int(s))
	}
	return []byte(streams[s]), nil
}

// UnmarshalText reads one back, so that an archived log is read by the type that wrote
// it rather than by a second reading of the same two words.
func (s *Stream) UnmarshalText(b []byte) error {
	for i, name := range streams {
		if name != "" && name == string(b) {
			*s = Stream(i)
			return nil
		}
	}
	return fmt.Errorf("%q is not one of the two streams a container writes on, stdout and stderr", string(b))
}

// index is the stream's slot in the per-stream line buffers. An unknown stream shares
// the buffer of standard error, which is where a line of unknown provenance belongs.
func (s Stream) index() int {
	if s == Stdout {
		return 0
	}
	return 1
}

// Line is one line of a task's log: timestamped, indexed and attributed to the stream it
// was written on, which is the shape the contract asks of the captured error stream.
//
// The run, the step, the attempt and the shard are not members. A log is opened for one
// task and the task identifier carries all four, so repeating them on every line would
// be the same four values written a thousand times over.
type Line struct {
	At     time.Time `json:"at"`
	Index  int       `json:"index"`
	Stream Stream    `json:"stream"`
	Text   string    `json:"text"`
}

// LogRef says where a task's log went, how much of it there is, and whether a cap cut it
// short. It is what leaves through the observer, since graph.Result has nowhere to carry
// a log.
//
// URI is filled in by whoever opened the sink, because the log is written through an
// io.Writer that knows where it lands and this does not.
type LogRef struct {
	URI       agk.URI `json:"uri,omitzero"`
	Lines     int     `json:"lines"`
	Truncated bool    `json:"truncated,omitempty"`
}

// notePrefix begins a line the driver wrote itself, so that a reader tells the runner's
// words from the container's inside one stream. It is the package's name, which is the
// prefix every error in this module already carries.
const notePrefix = "driver: "

// maxLineBytes is where a line that never ends is written out in pieces.
//
// A line is the unit masking works on, so the whole of one is held in memory before
// anything is written, and a container emitting a gigabyte without a newline would
// otherwise hold a gigabyte here. The pieces are cut where no value can be split across
// the cut, which is what masker.hold is for.
const maxLineBytes = 64 << 10

// taskLog turns what a container wrote into the log the contract collects: masked, then
// timestamped, indexed and capped.
//
// The order matters and is the reason this is one type rather than three wrappers.
// Masking is line buffered and runs before the timestamp, the index and the cap, so
// nothing unmasked reaches the sink, the truncation marker included. A value split
// across two reads from the socket is still caught, because the line is whole before the
// match runs; a value split across two lines is not, which is what the documentation
// already says of literal matching.
//
// It is guarded, because two goroutines reach it: the one demultiplexing the container's
// streams, and the one that writes the driver's own lines about a deadline that fired or
// an exit code charged to the runtime.
type taskLog struct {
	mu       sync.Mutex
	w        io.Writer
	mask     *masker
	now      func() time.Time
	maxBytes int64
	maxLines int

	pending   [2][]byte
	lines     int
	written   int64
	truncated bool
	stopped   bool
	sealed    bool
	err       error
}

// newLog opens the log of one task over w, which is the sink the runner opened for it.
//
// A nil w is a task whose runner keeps no log: the lines are still counted and capped, so
// that what the observer is told does not depend on whether anybody was listening.
// maxBytes and maxLines of zero or less are caps deliberately turned off, which is the
// reading agk already takes of a limit that is not positive.
func newLog(w io.Writer, m *masker, now func() time.Time, maxBytes int64, maxLines int) *taskLog {
	if now == nil {
		now = time.Now
	}
	return &taskLog{w: w, mask: m, now: now, maxBytes: maxBytes, maxLines: maxLines}
}

// write takes what was read off one of the container's streams. It never returns an
// error: a sink that failed is the runner's trouble and not the container's, and a task
// is not failed for it. The error is kept and comes back from finish.
func (l *taskLog) write(s Stream, b []byte) {
	if l == nil || len(b) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sealed {
		return
	}
	l.append(s, b)
}

// stream gives one of the two streams as an io.Writer, for a copy loop to write into.
func (l *taskLog) stream(s Stream) io.Writer { return streamWriter{log: l, stream: s} }

// streamWriter is one stream of a log, seen as an io.Writer.
type streamWriter struct {
	log    *taskLog
	stream Stream
}

func (w streamWriter) Write(p []byte) (int, error) {
	w.log.write(w.stream, p)
	return len(p), nil
}

// note writes one line of the driver's own into the log the container is writing into.
//
// It goes on standard error because that is the stream the contract collects and the one
// a person opens a log to read: a line saying that an exit code was charged to the
// runtime belongs beside the container's own last words, in order, rather than in a
// second place nobody correlates. The text is masked like any other, because a note
// quotes what the container did.
func (l *taskLog) note(format string, args ...any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sealed {
		return
	}
	// What the container left half written goes first, so the note does not land in
	// the middle of a line the container had not finished.
	l.flush(Stderr)
	for _, piece := range strings.Split(l.mask.maskString(fmt.Sprintf(format, args...)), "\n") {
		l.emit(Stderr, notePrefix+strings.TrimSuffix(piece, "\r"))
	}
}

// finish writes out what each stream left without a final newline, seals the log and says
// where it got to.
//
// A container that exits mid-line still said something, and dropping it would lose the
// one line that most often says why the container exited.
//
// Sealing is the other half, and it is about the sink rather than about the log. The
// caller closes the sink the moment this returns, and one thing is still able to write
// after that: the goroutine a stop was issued on, which notes a stop the daemon refused
// and which outlives the task it was issued for by as long as the grace. A note landing
// then would be a Write on a writer its owner has already closed, and would make the line
// count this just answered with untrue. So after this there is nothing more to write and
// nothing more to count, and every later call is dropped.
func (l *taskLog) finish() (LogRef, error) {
	if l == nil {
		return LogRef{}, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flush(Stdout)
	l.flush(Stderr)
	l.sealed = true
	return LogRef{Lines: l.lines, Truncated: l.truncated}, l.err
}

// append adds bytes to one stream's pending line and writes out every line they
// completed.
func (l *taskLog) append(s Stream, b []byte) {
	i := s.index()
	buf := append(l.pending[i], b...)
	for {
		n := bytes.IndexByte(buf, '\n')
		if n < 0 {
			break
		}
		l.emit(s, l.text(buf[:n]))
		buf = buf[n+1:]
	}
	if len(buf) > maxLineBytes {
		// A line long enough to be a denial of service is written out in pieces.
		// The whole of what is held is masked first and only then cut, and the cut
		// leaves behind everything that could still be the start of a value, so a
		// secret straddling it is masked when the next piece completes it.
		masked := l.mask.mask(buf)
		keep := min(l.mask.hold(), len(masked))
		if piece := masked[:len(masked)-keep]; len(piece) > 0 {
			l.emit(s, string(piece))
		}
		buf = buf[:copy(buf, masked[len(masked)-keep:])]
	}
	// Moved to the front of the buffer rather than kept as the window it is: what is
	// left of a line is a few bytes at the end of an array that grew by appending, and
	// letting the window creep forward through that array is what makes a stream of
	// long lines keep reallocating one.
	l.pending[i] = append(l.pending[i][:0], buf...)
}

// flush writes out what one stream left without a final newline.
func (l *taskLog) flush(s Stream) {
	i := s.index()
	if len(l.pending[i]) == 0 {
		return
	}
	text := l.text(l.pending[i])
	l.pending[i] = l.pending[i][:0]
	l.emit(s, text)
}

// text makes one line out of the bytes of one: masked first, so that nothing unmasked
// reaches the cap, the index or the sink, and then relieved of the carriage return a
// container writing CRLF leaves at the end of every line.
func (l *taskLog) text(b []byte) string {
	return string(bytes.TrimSuffix(l.mask.mask(b), []byte("\r")))
}

// emit writes one line, unless a cap has already ended the log.
func (l *taskLog) emit(s Stream, text string) {
	if l.stopped {
		return
	}
	if l.maxLines > 0 && l.lines >= l.maxLines {
		l.stop("the log reached the %d lines the runner policy allows, and the rest of it was dropped", l.maxLines)
		return
	}
	whole := true
	if l.maxBytes > 0 {
		room := l.maxBytes - l.written
		if room <= 0 {
			l.stop("the log reached the %d bytes the runner policy allows, and the rest of it was dropped", l.maxBytes)
			return
		}
		if int64(len(text)) > room {
			// The byte cap is counted in bytes, so the line is cut at the byte it
			// falls on rather than dropped whole. It is cut back to a rune
			// boundary, because half a character is not text.
			text = cut(text, int(room))
			whole = false
		}
	}
	l.put(Line{At: l.now().UTC(), Index: l.lines + 1, Stream: s, Text: text})
	l.written += int64(len(text))
	if !whole {
		l.stop("the log reached the %d bytes the runner policy allows, and the rest of it was dropped", l.maxBytes)
	}
}

// stop records a cap being reached and writes the marker, once.
//
// The marker is a line of the log like any other and is counted as one, so that the
// number the observer is given is the number of lines a reader of the sink will find.
func (l *taskLog) stop(format string, args ...any) {
	if l.stopped {
		return
	}
	l.stopped = true
	l.truncated = true
	l.put(Line{At: l.now().UTC(), Index: l.lines + 1, Stream: Stderr, Text: notePrefix + fmt.Sprintf(format, args...)})
}

// put writes one line to the sink as one JSON object, and counts it whether or not there
// is a sink to write it to.
//
// The first error stops the writing. A sink that failed once fails again, and a log that
// keeps trying turns one broken writer into one broken writer per line of output.
func (l *taskLog) put(line Line) {
	l.lines++
	if l.w == nil || l.err != nil {
		return
	}
	b, err := json.Marshal(line)
	if err != nil {
		l.err = fmt.Errorf("driver: the log line could not be written: %w", err)
		return
	}
	if _, err := l.w.Write(append(b, '\n')); err != nil {
		l.err = fmt.Errorf("driver: the log could not be written: %w", err)
	}
}

// cut shortens text to at most n bytes, at a rune boundary.
func cut(text string, n int) string {
	if n >= len(text) {
		return text
	}
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	return text[:n]
}
