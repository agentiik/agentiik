package driver

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// logClock is a clock that moves one second per line, so that a test can say which line
// was written when without depending on how long it took to write.
func logClock() func() time.Time {
	at := time.Date(2026, 9, 10, 6, 0, 12, 0, time.UTC)
	return func() time.Time {
		at = at.Add(time.Second)
		return at
	}
}

// logLines reads back what was written to the sink, which is one JSON object per line.
func logLines(t *testing.T, sink *bytes.Buffer) []Line {
	t.Helper()
	var out []Line
	for _, raw := range strings.Split(strings.TrimSuffix(sink.String(), "\n"), "\n") {
		if raw == "" {
			continue
		}
		var l Line
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("the log is not one JSON object per line: %v: %s", err, raw)
		}
		out = append(out, l)
	}
	return out
}

// texts is the text of each line, which is what most of these assert on.
func texts(lines []Line) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Text
	}
	return out
}

// The three words the contract uses of the captured error stream: timestamped, indexed
// and capped. The first two are here and the cap is below.
func TestTheLogIsTimestampedAndIndexedFromOne(t *testing.T) {
	var sink bytes.Buffer
	l := newLog(&sink, nil, logClock(), 0, 0)

	l.write(Stderr, []byte("first\nsecond\n"))
	if _, err := l.finish(); err != nil {
		t.Fatal(err)
	}

	lines := logLines(t, &sink)
	if len(lines) != 2 {
		t.Fatalf("wrote %d lines, want 2: %v", len(lines), texts(lines))
	}
	for i, want := range []string{"first", "second"} {
		if lines[i].Text != want {
			t.Errorf("line %d is %q, want %q", i, lines[i].Text, want)
		}
		if lines[i].Index != i+1 {
			t.Errorf("line %d carries index %d, want %d: a log is indexed from one", i, lines[i].Index, i+1)
		}
		if lines[i].At.IsZero() {
			t.Errorf("line %d carries no timestamp", i)
		}
	}
	if !lines[1].At.After(lines[0].At) {
		t.Errorf("the second line is not after the first: %s then %s", lines[0].At, lines[1].At)
	}
}

// Both streams travel in one log, each line saying which one it came from, so that a
// person reading it sees what the container said in the order it said it.
func TestTheLogSaysWhichStreamALineCameFrom(t *testing.T) {
	var sink bytes.Buffer
	l := newLog(&sink, nil, logClock(), 0, 0)

	l.write(Stdout, []byte("payload\n"))
	l.write(Stderr, []byte("diagnostic\n"))
	if _, err := l.finish(); err != nil {
		t.Fatal(err)
	}

	lines := logLines(t, &sink)
	if len(lines) != 2 {
		t.Fatalf("wrote %d lines, want 2", len(lines))
	}
	if lines[0].Stream != Stdout || lines[1].Stream != Stderr {
		t.Errorf("streams are %s and %s, want stdout then stderr", lines[0].Stream, lines[1].Stream)
	}
	if !strings.Contains(sink.String(), `"stream":"stdout"`) {
		t.Errorf("the stream is not spelled as it travels: %s", sink.String())
	}
}

// A line that is half written is not a line yet. The whole of one is held before the
// match runs, which is what catches a value split across two reads from the socket.
func TestAValueSplitAcrossTwoReadsIsStillMasked(t *testing.T) {
	var sink bytes.Buffer
	l := newLog(&sink, newMasker([]byte("s3cr3t-value")), logClock(), 0, 0)

	l.write(Stderr, []byte("token=s3cr3t"))
	l.write(Stderr, []byte("-value done\n"))
	if _, err := l.finish(); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(sink.String(), "s3cr3t") {
		t.Fatalf("the value was written in the clear: %s", sink.String())
	}
	if got := texts(logLines(t, &sink)); len(got) != 1 || got[0] != "token="+maskToken+" done" {
		t.Errorf("wrote %q", got)
	}
}

// Masking runs before the timestamp, the index and the cap, so that nothing unmasked
// reaches the sink whatever else happens to the line afterwards.
func TestNothingUnmaskedReachesTheSinkWhenTheCapCutsALine(t *testing.T) {
	var sink bytes.Buffer
	l := newLog(&sink, newMasker([]byte("s3cr3t")), logClock(), 12, 0)

	l.write(Stderr, []byte("s3cr3t s3cr3t s3cr3t\n"))
	if _, err := l.finish(); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(sink.String(), "s3cr3t") {
		t.Fatalf("the value reached the sink through the cut: %s", sink.String())
	}
}

// A container writing CRLF leaves a carriage return at the end of every line, and a
// carriage return is not part of what it said.
func TestACarriageReturnAtTheEndOfALineIsNotPartOfIt(t *testing.T) {
	var sink bytes.Buffer
	l := newLog(&sink, nil, logClock(), 0, 0)

	l.write(Stderr, []byte("windows\r\n"))
	if _, err := l.finish(); err != nil {
		t.Fatal(err)
	}

	if got := texts(logLines(t, &sink)); len(got) != 1 || got[0] != "windows" {
		t.Errorf("wrote %q, want the line without its carriage return", got)
	}
}

// A container that exits mid-line still said something, and it is usually the line that
// says why it exited.
func TestWhatTheContainerLeftWithoutANewlineIsWrittenAtTheEnd(t *testing.T) {
	var sink bytes.Buffer
	l := newLog(&sink, nil, logClock(), 0, 0)

	l.write(Stdout, []byte("half a line"))
	l.write(Stderr, []byte("panic: no such file"))
	ref, err := l.finish()
	if err != nil {
		t.Fatal(err)
	}

	got := texts(logLines(t, &sink))
	if len(got) != 2 || got[0] != "half a line" || got[1] != "panic: no such file" {
		t.Fatalf("wrote %q, want what each stream left behind", got)
	}
	if ref.Lines != 2 {
		t.Errorf("the reference says %d lines, want 2", ref.Lines)
	}
	if ref.Truncated {
		t.Errorf("the reference says the log was truncated, and no cap was set")
	}
}

// The cap is what keeps one runaway task from filling the store, and the marker is what
// says the rest was dropped rather than never written.
func TestTheLineCapEndsTheLogAndSaysSo(t *testing.T) {
	var sink bytes.Buffer
	l := newLog(&sink, nil, logClock(), 0, 2)

	l.write(Stderr, []byte("one\ntwo\nthree\nfour\n"))
	ref, err := l.finish()
	if err != nil {
		t.Fatal(err)
	}

	lines := logLines(t, &sink)
	if len(lines) != 3 {
		t.Fatalf("wrote %d lines, want the two the cap allows and the marker: %q", len(lines), texts(lines))
	}
	if lines[0].Text != "one" || lines[1].Text != "two" {
		t.Errorf("wrote %q, want the first two lines", texts(lines))
	}
	if !strings.HasPrefix(lines[2].Text, notePrefix) || !strings.Contains(lines[2].Text, "2 lines") {
		t.Errorf("the marker does not say what was dropped: %q", lines[2].Text)
	}
	if !ref.Truncated || ref.Lines != 3 {
		t.Errorf("the reference says truncated=%v lines=%d, want true and 3", ref.Truncated, ref.Lines)
	}
	if strings.Contains(sink.String(), "three") {
		t.Errorf("a line past the cap was written: %s", sink.String())
	}
}

// The byte cap is counted in bytes, so the line it falls on is cut at the byte rather
// than dropped whole: what a container said up to there is still what it said.
func TestTheByteCapCutsTheLineItFallsOn(t *testing.T) {
	var sink bytes.Buffer
	l := newLog(&sink, nil, logClock(), 8, 0)

	l.write(Stderr, []byte("abcdefghijkl\nnever\n"))
	ref, err := l.finish()
	if err != nil {
		t.Fatal(err)
	}

	lines := logLines(t, &sink)
	if len(lines) != 2 {
		t.Fatalf("wrote %d lines, want the cut line and the marker: %q", len(lines), texts(lines))
	}
	if lines[0].Text != "abcdefgh" {
		t.Errorf("the cut line is %q, want the eight bytes the cap allows", lines[0].Text)
	}
	if !strings.Contains(lines[1].Text, "8 bytes") {
		t.Errorf("the marker does not say what was dropped: %q", lines[1].Text)
	}
	if !ref.Truncated {
		t.Errorf("the reference does not say the log was truncated")
	}
}

// A cut falls on a byte and a rune is several of them. Half a character is not text, and
// a log line is read by a person.
func TestTheByteCapCutsAtARuneBoundary(t *testing.T) {
	var sink bytes.Buffer
	// Two bytes, which is the first byte of the second rune: the cut falls inside it
	// and backs off to where the rune began.
	l := newLog(&sink, nil, logClock(), 2, 0)

	l.write(Stderr, []byte("héllo\n"))
	if _, err := l.finish(); err != nil {
		t.Fatal(err)
	}

	lines := logLines(t, &sink)
	if len(lines) == 0 {
		t.Fatal("nothing was written")
	}
	if lines[0].Text != "h" {
		t.Errorf("the cut line is %q, want the whole runes that fit", lines[0].Text)
	}
}

// A line the driver wrote itself carries the package's name, so that the runner's words
// are told from the container's inside one stream, and it is masked like any other.
func TestTheDriversOwnLineIsMarkedAndMasked(t *testing.T) {
	var sink bytes.Buffer
	l := newLog(&sink, newMasker([]byte("s3cr3t")), logClock(), 0, 0)

	l.write(Stderr, []byte("half"))
	l.note("the container exited 137 with s3cr3t in the argument")
	if _, err := l.finish(); err != nil {
		t.Fatal(err)
	}

	lines := logLines(t, &sink)
	if len(lines) != 2 {
		t.Fatalf("wrote %d lines, want the half line and the note: %q", len(lines), texts(lines))
	}
	if lines[0].Text != "half" {
		t.Errorf("the note landed inside the line the container had not finished: %q", texts(lines))
	}
	if !strings.HasPrefix(lines[1].Text, notePrefix) {
		t.Errorf("the note carries no mark: %q", lines[1].Text)
	}
	if strings.Contains(lines[1].Text, "s3cr3t") {
		t.Errorf("the note was written unmasked: %q", lines[1].Text)
	}
	if lines[1].Stream != Stderr {
		t.Errorf("the note is on %s, want the stream the contract collects", lines[1].Stream)
	}
}

// A line that never ends is written out in pieces, because the whole of one is held in
// memory before anything is written. The cut leaves behind everything that could still
// be the beginning of a value, so a value straddling it is masked when the next piece
// completes it.
func TestALineTooLongToHoldIsWrittenInPiecesWithoutSplittingAValue(t *testing.T) {
	var sink bytes.Buffer
	secret := "supersecretvalue"
	l := newLog(&sink, newMasker([]byte(secret)), logClock(), 0, 0)

	l.write(Stderr, append(bytes.Repeat([]byte("a"), maxLineBytes), []byte(secret[:6])...))
	l.write(Stderr, append([]byte(secret[6:]), '\n'))
	if _, err := l.finish(); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(sink.String(), secret[:6]) {
		t.Fatalf("the first bytes of the value were written in the clear on the cut")
	}
	lines := logLines(t, &sink)
	if len(lines) != 2 {
		t.Fatalf("wrote %d lines, want the piece and the rest", len(lines))
	}
	if !strings.HasSuffix(lines[1].Text, maskToken) {
		t.Errorf("the value was not masked once the line completed it: %q", lines[1].Text)
	}
}

// A sink that failed is the runner's trouble and not the container's, so nothing throws
// it back at the caller in the middle of reading a container's output. It comes back
// once, at the end.
func TestASinkThatFailedIsReportedOnceAtTheEnd(t *testing.T) {
	sink := brokenSink{}
	l := newLog(sink, nil, logClock(), 0, 0)

	l.write(Stderr, []byte("one\ntwo\n"))
	ref, err := l.finish()
	if err == nil {
		t.Fatal("a sink that failed was not reported")
	}
	if !errors.Is(err, errBrokenSink) {
		t.Errorf("the error lost what went wrong: %v", err)
	}
	if ref.Lines != 2 {
		t.Errorf("the reference says %d lines, and the lines were still counted", ref.Lines)
	}
}

// A task whose runner keeps no log still counts and caps its lines, so that what the
// observer is told does not depend on whether anybody was listening.
func TestALogWithNoSinkStillCountsAndCaps(t *testing.T) {
	l := newLog(nil, nil, logClock(), 0, 2)

	l.write(Stderr, []byte("one\ntwo\nthree\n"))
	ref, err := l.finish()
	if err != nil {
		t.Fatal(err)
	}
	if !ref.Truncated || ref.Lines != 3 {
		t.Errorf("the reference says truncated=%v lines=%d, want true and 3", ref.Truncated, ref.Lines)
	}
}

// The copy loop writes into one of the two streams, which is what the demultiplexed
// frames arrive as.
func TestAStreamIsAWriter(t *testing.T) {
	var sink bytes.Buffer
	l := newLog(&sink, nil, logClock(), 0, 0)

	if _, err := l.stream(Stdout).Write([]byte("through the writer\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := l.finish(); err != nil {
		t.Fatal(err)
	}

	lines := logLines(t, &sink)
	if len(lines) != 1 || lines[0].Stream != Stdout || lines[0].Text != "through the writer" {
		t.Errorf("wrote %q", texts(lines))
	}
}

// The zero value is not one of the two streams. A line whose stream was never set would
// otherwise read as standard output, and a diagnostic attributed to the payload stream
// is worse than one attributed to neither.
func TestTheZeroStreamIsNotOne(t *testing.T) {
	var s Stream
	if _, err := s.MarshalText(); err == nil {
		t.Fatal("the zero value was written as a stream")
	}
	if err := s.UnmarshalText([]byte("stdout")); err != nil || s != Stdout {
		t.Fatalf("stdout did not read back: %v %v", s, err)
	}
	if err := s.UnmarshalText([]byte("both")); err == nil {
		t.Fatal("a name that is not one of the two was read as a stream")
	}
}

// errBrokenSink is what a sink that cannot be written to answers with.
var errBrokenSink = errors.New("the log sink is gone")

type brokenSink struct{}

func (brokenSink) Write([]byte) (int, error) { return 0, errBrokenSink }

// closedSink is a sink that refuses a write once it has been closed, which is what the
// runner's own sink is: an open file, an object being shipped, a buffer somebody else
// owns. Writing into one after closing it is not the driver's to do.
type closedSink struct {
	mu     sync.Mutex
	closed bool
	after  int
	buf    bytes.Buffer
}

func (s *closedSink) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		s.after++
		return 0, errors.New("write on a closed sink")
	}
	return s.buf.Write(b)
}

func (s *closedSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// The log is sealed when it is finished, because the caller closes the sink the moment
// finish returns and the goroutine a stop was issued on outlives the task by as long as
// the grace. A note landing after that would be a write on a writer nobody holds open any
// more, and would make the line count finish already answered with untrue.
func TestTheLogIsSealedWhenItIsFinishedSoNothingWritesAfterTheSinkIsClosed(t *testing.T) {
	sink := &closedSink{}
	l := newLog(sink, nil, logClock(), 0, 0)

	l.write(Stderr, []byte("what the container said\n"))
	ref, err := l.finish()
	if err != nil {
		t.Fatal(err)
	}
	sink.Close()

	// Exactly what a stop still in flight does when the daemon refuses it, and what
	// a frame arriving late off a stream that has not noticed the end does.
	l.note("the container could not be stopped: %v", errors.New("the daemon answered 500"))
	l.write(Stderr, []byte("a frame that arrived after the end\n"))

	if sink.after != 0 {
		t.Errorf("%d writes reached the sink after it was closed, and the log was finished before it was", sink.after)
	}
	if ref.Lines != 1 {
		t.Errorf("finish answered with %d lines", ref.Lines)
	}
	if again, _ := l.finish(); again.Lines != ref.Lines {
		t.Errorf("the log counted %d lines after it was sealed, and finish had already answered %d", again.Lines, ref.Lines)
	}
}
