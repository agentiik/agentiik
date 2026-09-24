package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"time"
	"unicode/utf8"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
)

// A task's log, taken from the runner holding the task.
//
// "The log travels as an outbound request from the host holding the container, because a runner
// opens no listening port and is never connected to; it goes to the API rather than to the object
// store, since the API is what indexes a log, applies the retention the workflow declared within
// the namespace's ceiling and serves it over SSE." It is shipped in chunks while the container
// runs, so that a step running for an hour is readable while it runs, and a last chunk closes it.
//
// Each chunk the API writes is one object in the store, under the namespace, the run and the task
// the log's URI names, and the database keeps the index to them and never a line. Chunks are taken
// in order and only in order: one that arrives past a gap is answered with where the gap is and
// kept nowhere, so that what the store holds is always the log from its first line, and the
// runner, which keeps a chunk until it is told it is held, ships from there again. Kept past a gap,
// a chunk would be counted and capped before the lines ahead of it were known, and the cap could
// fall in the middle of a log whose end was already written.

// GrantHeader is where a shipment carries its task's grant.
//
// "The runner credential authenticates the call and the per-task grant scopes it to one task, and
// neither is a member of either half, because a token written into a body is a token that ends up
// in the very log this message carries." The credential is the Authorization header, as on every
// runner route, and the grant has one of its own: the body is lines of the log and nothing else.
const GrantHeader = "Agentiik-Grant"

// shipMaxBytes is how large one shipment may be.
//
// A chunk is what a runner read of a container's standard error since the last one, and the
// driver writes out a line that never ends in pieces of 64 KiB, so a mebibyte holds fifteen of the
// longest lines it produces with their framing, and far more of the usual ones, while five
// shipments reach the whole of what the log may hold.
const shipMaxBytes = 1 << 20

// shipMaxLines is how many lines one shipment may carry. The count is what bounds reading one,
// since a line costs its two strings and a timestamp however short it is, and the shortest a line
// is written on the wire is about forty bytes: at shipMaxBytes that is some twenty-six thousand of
// them, and 4,096 is more than a container writes between two shipments of a runner that ships
// while the container runs.
const shipMaxLines = 4096

// The caps a log is held to where it is written, "so that one component keeps one account of how
// much of a task's log exists". They are the runner's own defaults, log_max_bytes and
// log_max_lines, exactly, and the API is the backstop for a runner that raised them: "a log is a
// diagnostic and not a payload".
//
// Exactly and not with room to spare, because truncated is what the result reports as
// log.truncated and the API has no other way to learn that the runner cut the log. A driver that
// reaches its cap writes one line more to say so, and at these caps that line is the one dropped
// here, so a log the runner cut is truncated in the answer, and one that came to the cap and no
// further is not. The line saying why is lost; the flag saying that is not, and it is the one
// "somebody chasing a failure has to know".
const (
	DefaultLogMaxBytes = 4 << 20
	DefaultLogMaxLines = 50000
)

// LogShipment is one chunk of a task's log, in the shape wire.schema.json gives it:
// $defs/logShipment/request.
type LogShipment struct {
	IdempotencyKey agk.TaskID `json:"idempotency_key"`
	Seq            int64      `json:"seq"`
	FirstLine      int64      `json:"first_line"`
	Final          bool       `json:"final"`
	Lines          []LogLine  `json:"lines"`

	// saidFinal is whether the body wrote final, which the wire requires on every chunk: "the
	// closing chunk is what a result message is allowed to report a line count from, and that is
	// too load-bearing to rest on a missing field".
	saidFinal bool
}

// LogLine is one line of a log: when the runner read it, and what it said once masked. It is the
// shape a line is shipped in and the shape it is kept in, one JSON object a line.
type LogLine struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`

	saidText bool
}

func (s *LogShipment) field(b *body, name string) error {
	switch name {
	case "idempotency_key":
		return text(b, &s.IdempotencyKey)
	case "seq":
		return integer(b, &s.Seq)
	case "first_line":
		return integer(b, &s.FirstLine)
	case "final":
		s.saidFinal = b.d.PeekKind() != jsontext.KindNull
		return flag(b, &s.Final)
	case "lines":
		return s.lines(b)
	}
	return unknown(name)
}

// lines reads the lines of a chunk, each an object of its own, and refuses the one past
// shipMaxLines. Null is no lines, which is refused once the body is read.
func (s *LogShipment) lines(b *body) error {
	t, err := b.d.ReadToken()
	if err != nil {
		return malformed(err)
	}
	switch t.Kind() {
	case jsontext.KindNull:
		return nil
	case jsontext.KindBeginArray:
	default:
		return b.mistyped(t.Kind(), "an array of lines")
	}
	s.Lines = []LogLine{}
	for {
		k := b.d.PeekKind()
		if k == jsontext.KindEndArray {
			break
		}
		if len(s.Lines) == shipMaxLines && k != jsontext.KindInvalid {
			return &tooLarge{reason: fmt.Sprintf("a shipment carries at most %d lines, which is more than a container writes between two of them", shipMaxLines)}
		}
		var line LogLine
		if err := b.fields(&line); err != nil {
			return err
		}
		s.Lines = append(s.Lines, line)
	}
	_, err = b.d.ReadToken()
	return malformed(err)
}

func (l *LogLine) field(b *body, name string) error {
	switch name {
	case "at":
		return instant(b, &l.At)
	case "text":
		l.saidText = b.d.PeekKind() != jsontext.KindNull
		return text(b, &l.Text)
	}
	return unknown(name)
}

// check holds a chunk to what the wire says of one, where the body alone can tell.
func (s LogShipment) check() error {
	switch {
	case s.IdempotencyKey == "":
		return errors.New("the shipment names no idempotency_key: it names the task whose log it is, which the grant has to name too")
	case !keyForm.MatchString(string(s.IdempotencyKey)):
		return fmt.Errorf("%.100q is not an idempotency key: one is run/step/attempt, and run/step/attempt/index/of where a fan-out produced it", string(s.IdempotencyKey))
	case s.Seq < 1 || s.Seq >= math.MaxInt32:
		return fmt.Errorf("the shipment is chunk %d: chunks are counted from 1, once per chunk", s.Seq)
	case s.FirstLine < 1 || s.FirstLine >= math.MaxInt32-int64(len(s.Lines)):
		return fmt.Errorf("the shipment says its first line is %d: lines are counted from 1 across the whole of the log", s.FirstLine)
	case !s.saidFinal:
		return errors.New("the shipment does not say whether it is final: every chunk says so, the last one true")
	case s.Lines == nil:
		return errors.New("the shipment carries no lines: a chunk carries a list of them, empty only where it is the last and the container wrote nothing more")
	case len(s.Lines) == 0 && !s.Final:
		return errors.New("the shipment carries no lines and is not the last: a chunk is shipped because there is something in it, or to close the log")
	case s.Seq == 1 && s.FirstLine != 1:
		return fmt.Errorf("the first chunk says it begins at line %d, and a log begins at line 1", s.FirstLine)
	}
	if err := s.IdempotencyKey.Validate(); err != nil {
		return fmt.Errorf("the shipment names a key that could not have been composed: %w", err)
	}
	for i, l := range s.Lines {
		switch {
		case l.At.IsZero():
			return fmt.Errorf("line %d of the shipment says no at: every line carries when the runner read it", i+1)
		case !l.saidText:
			return fmt.Errorf("line %d of the shipment carries no text: an empty line is written \"text\": \"\"", i+1)
		}
	}
	return nil
}

// LogShipped is what a shipment is answered: where the log lives, what was done with the chunk,
// and where the runner now stands. $defs/logShipment/response.
type LogShipped struct {
	URI       agk.LogURI `json:"uri"`
	Accepted  int        `json:"accepted"`
	NextSeq   int        `json:"next_seq"`
	Lines     int        `json:"lines"`
	Truncated bool       `json:"truncated"`
}

// errChunk is a chunk that cannot be taken where it says it goes: the same seq with other lines, a
// chunk after the last one, or a first line that is not where the chunks before it ended. Each is
// a runner that has lost track of its own log, and is answered 409 with the sentence.
type errChunk struct{ reason string }

func (e errChunk) Error() string { return e.reason }

func (s *RunnerAPI) shipLog(w http.ResponseWriter, r *http.Request, runner Runner) {
	if s.objects == nil {
		fail(w, http.StatusServiceUnavailable, "this installation has no object store attached")
		return
	}
	grant := r.Header.Get(GrantHeader)
	if grant == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, "this shipment carries no grant: it goes in the "+GrantHeader+" header, never in the body")
		return
	}
	var ship LogShipment
	if err := readAtMost(r, &ship, shipMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	if err := ship.check(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	// Two transactions. The first decides where the chunk would be written and records the key,
	// and commits, so that the object can be named by the purge whatever happens next. The second
	// holds the log from its first read to its last write, decides again, writes the chunk to the
	// store and indexes it. Two shipments of one log are taken one after the other there, which
	// they have to be, since what the second may write depends on what the first did. The store
	// is written before the index, so that the index never names an object that is not there, and
	// a transaction that fails after the write leaves an object the first transaction recorded.
	// One that finds a key nobody recorded, because another chunk was taken in between, goes back
	// to the first, which then records it.
	var answer LogShipped
	var err error
	for attempt := 1; ; attempt++ {
		err = s.pool.Installation(r.Context(), db.LogShipment, func(ctx context.Context, wide *db.Wide) error {
			held, err := wide.ShippingLog(ctx, grant, ship.IdempotencyKey, runner.ID)
			if err != nil {
				return err
			}
			_, key, err := s.take(ctx, wide, held, ship, true)
			if err != nil || key == "" {
				return err
			}
			return wide.RecordLogObject(ctx, held, key)
		})
		if err != nil {
			break
		}
		if s.betweenShip != nil {
			s.betweenShip()
		}
		err = s.pool.Installation(r.Context(), db.LogShipment, func(ctx context.Context, wide *db.Wide) error {
			held, err := wide.ShippingLog(ctx, grant, ship.IdempotencyKey, runner.ID)
			if err != nil {
				return err
			}
			answer, _, err = s.take(ctx, wide, held, ship, false)
			return err
		})
		if !errors.Is(err, errNotRecorded) || attempt == 3 {
			break
		}
	}
	var conflict errChunk
	switch {
	case err == nil:
		write(w, http.StatusOK, answer)
	case errors.Is(err, db.ErrNoGrant):
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, "that grant opens no task's log")
	case errors.Is(err, db.ErrTaskHeld):
		// The task was never redeemed by this runner: another holds it, or nobody does. A runner
		// ships the log of what it runs, and it runs only what its redemption bound to it.
		fail(w, http.StatusConflict, "that task is not this runner's, and a runner ships the log of a task it redeemed")
	case errors.Is(err, db.ErrLogExpired):
		fail(w, http.StatusConflict, "the logs of that task's run are past their retention, and nothing is added to them")
	case errors.As(err, &conflict):
		fail(w, http.StatusConflict, conflict.reason)
	default:
		s.report(fmt.Errorf("api: a chunk of the log of %s from runner %s was not taken: %w", ship.IdempotencyKey, runner.ID, err))
		fail(w, http.StatusInternalServerError, "the chunk could not be written")
	}
}

// errNotRecorded is a chunk to be written at a key nobody recorded, which is written only once it
// has been.
var errNotRecorded = errors.New("api: the object a chunk is written to was not recorded first")

// take decides what one chunk does to a log, writes what it keeps, and answers where the log
// stands. Dry, it writes nothing and answers only the key it would write the chunk at, or none.
func (s *RunnerAPI) take(ctx context.Context, wide *db.Wide, was db.TaskLog, ship LogShipment, dry bool) (LogShipped, string, error) {
	seq := int(ship.Seq)
	shipped, err := chunkOf(ship.FirstLine, ship.Final, ship.Lines)
	if err != nil {
		return LogShipped{}, "", err
	}
	digest := sha256.Sum256(shipped)
	shippedDigest := hex.EncodeToString(digest[:])

	switch {
	case seq < was.NextSeq && dry:
		return LogShipped{}, "", nil

	case seq < was.NextSeq:
		// A chunk already taken, shipped again because its answer was lost: answered again and
		// not appended twice. It has to be the same chunk, since the key and the seq together
		// name one, and one that says otherwise is refused rather than answered as held. A chunk
		// taken once the cap was reached has no row to compare with, and nothing of it was kept
		// either way.
		held, found, err := wide.ShippedChunk(ctx, was, seq)
		if err != nil {
			return LogShipped{}, "", err
		}
		if found && held.ShippedDigest != shippedDigest {
			return LogShipped{}, "", errChunk{fmt.Sprintf("chunk %d of that log was shipped before with other lines, and a chunk is shipped again as it was the first time", seq)}
		}
		return answerOf(was, 0)

	case was.FinalSeq != 0:
		return LogShipped{}, "", errChunk{fmt.Sprintf("the log of that task was closed by chunk %d, and nothing follows the last chunk", was.FinalSeq)}

	case was.Truncated:
		// Nothing more is written, so the chunk moves the log past itself and is kept nowhere,
		// the last one closing it: "a runner reading true stops shipping", and one still sending
		// what was already on its way is told where it stands without the index growing a row a
		// request.
		if dry {
			return LogShipped{}, "", nil
		}
		now := was
		now.NextSeq = seq + 1
		if ship.Final {
			now.FinalSeq = seq
		}
		if err := wide.ShipChunk(ctx, was, now, nil); err != nil {
			return LogShipped{}, "", err
		}
		return answerOf(now, 0)

	case seq > was.NextSeq:
		// Past a gap: next_seq "never runs past a gap: a runner whose chunk five never arrived is
		// told five and not eight", and the runner ships from there.
		if dry {
			return LogShipped{}, "", nil
		}
		return answerOf(was, 0)
	}

	if int(ship.FirstLine) != was.ShippedLines+1 {
		return LogShipped{}, "", errChunk{fmt.Sprintf("chunk %d says it begins at line %d, and the chunks before it end at line %d", seq, ship.FirstLine, was.ShippedLines)}
	}
	kept, cut := capped(ship.Lines, was.Lines, was.Bytes, s.logMaxLines, s.logMaxBytes)
	c := &db.LogChunk{Seq: seq, FirstLine: int(ship.FirstLine), Shipped: len(ship.Lines), ShippedDigest: shippedDigest, Lines: len(kept)}
	for _, l := range kept {
		c.Bytes += int64(len(l.Text))
	}
	if len(kept) > 0 {
		object, err := chunkOf(0, false, kept)
		if err != nil {
			return LogShipped{}, "", err
		}
		sum := sha256.Sum256(object)
		c.Digest = hex.EncodeToString(sum[:])
		if c.Key, err = logKey(was, seq, shippedDigest); err != nil {
			return LogShipped{}, "", err
		}
		if dry {
			return LogShipped{}, c.Key, nil
		}
		recorded, err := wide.LogObjectRecorded(ctx, was, c.Key)
		if err != nil {
			return LogShipped{}, "", err
		}
		if !recorded {
			return LogShipped{}, "", errNotRecorded
		}
		if err := s.objects.Put(ctx, c.Key, bytes.NewReader(object)); err != nil {
			return LogShipped{}, "", fmt.Errorf("the chunk could not be written to the store: %w", err)
		}
	}
	if dry {
		return LogShipped{}, "", nil
	}

	now := was
	now.NextSeq = seq + 1
	now.ShippedLines += len(ship.Lines)
	now.Lines += c.Lines
	now.Bytes += c.Bytes
	now.Truncated = cut
	if ship.Final {
		now.FinalSeq = seq
	}
	if err := wide.ShipChunk(ctx, was, now, c); err != nil {
		return LogShipped{}, "", err
	}
	return answerOf(now, c.Lines)
}

// answerOf is the answer a log standing where it does gives, with how many lines of this chunk were
// written.
func answerOf(l db.TaskLog, accepted int) (LogShipped, string, error) {
	uri, err := l.URI()
	if err != nil {
		return LogShipped{}, "", err
	}
	return LogShipped{URI: uri, Accepted: accepted, NextSeq: l.NextSeq, Lines: l.Lines, Truncated: l.Truncated}, "", nil
}

// capped is what of a chunk the caps leave, and whether they cut it: a log already holding lines
// lines and bytes bytes of text takes lines up to maxLines and text up to maxBytes, the line the
// byte cap falls inside cut back to the last whole character before it, as the driver cuts it. A
// cap of zero or less is no cap.
func capped(lines []LogLine, held int, heldBytes int64, maxLines int, maxBytes int64) ([]LogLine, bool) {
	kept := make([]LogLine, 0, len(lines))
	for _, l := range lines {
		if maxLines > 0 && held >= maxLines {
			return kept, true
		}
		if maxBytes > 0 {
			room := maxBytes - heldBytes
			if room <= 0 {
				return kept, true
			}
			if int64(len(l.Text)) > room {
				n := int(room)
				for n > 0 && !utf8.RuneStart(l.Text[n]) {
					n--
				}
				return append(kept, LogLine{At: l.At, Text: l.Text[:n]}), true
			}
		}
		kept = append(kept, l)
		held++
		heldBytes += int64(len(l.Text))
	}
	return kept, false
}

// chunkOf writes lines as a chunk is kept: one JSON object a line, each at in UTC. Written with a
// first line and a flag before them, it is what a shipment is compared on when it arrives again.
func chunkOf(firstLine int64, final bool, lines []LogLine) ([]byte, error) {
	var out bytes.Buffer
	if firstLine > 0 {
		fmt.Fprintf(&out, "%d %t\n", firstLine, final)
	}
	enc := json.NewEncoder(&out)
	for _, l := range lines {
		if err := enc.Encode(LogLine{At: l.At.UTC(), Text: l.Text}); err != nil {
			return nil, err
		}
	}
	return out.Bytes(), nil
}

// logKey is where one chunk of a log is kept: under its namespace, as every object is, then under
// the run and the task the log's URI names, "so the logs of a run are one prefix", then under the
// dispatch, since a requeue keeps its key and ships a log of its own, and last the seq, padded so
// that a listing is in order, and the digest of what was shipped.
//
// The shipped digest rather than the digest of what is kept, because the key has to be known, and
// recorded, before the chunk is decided under the log's lock. It names one content all the same:
// what a chunk keeps is decided by the chunks before it, which are fixed once it can be taken, so a
// chunk shipped again with the same lines is kept the same way, and one with other lines is
// another key.
func logKey(l db.TaskLog, seq int, shippedDigest string) (string, error) {
	uri, err := l.URI()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/logs/%s/%s/%s/%010d-%s",
		l.Namespace, uri.Run, url.PathEscape(string(uri.Task)), l.Row, seq, shippedDigest), nil
}

// ReadLogChunk reads back the lines of one chunk of a log, as the index names it, and refuses bytes
// that are not the ones the chunk was written with.
//
// It is what a reader of a log follows the index with, db.NS.TaskLog giving the chunks in order and
// this the lines of each, which is how GET /api/v1/runs/{id}/steps/{step}/logs is to read a log's
// history once it is built.
func ReadLogChunk(ctx context.Context, objects artifact.Objects, c db.LogChunk) ([]LogLine, error) {
	r, err := objects.Open(ctx, c.Key)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != c.Digest {
		return nil, fmt.Errorf("api: the chunk of a log at %s does not hold the bytes it was written with", c.Key)
	}
	var lines []LogLine
	dec := json.NewDecoder(bytes.NewReader(raw))
	for dec.More() {
		var l LogLine
		if err := dec.Decode(&l); err != nil {
			return nil, fmt.Errorf("api: the chunk of a log at %s: %w", c.Key, err)
		}
		lines = append(lines, l)
	}
	return lines, nil
}
