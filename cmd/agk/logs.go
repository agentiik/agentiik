package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
)

// agk logs: "Follows the logs of a run."
//
// A run's log is the logs of its steps, and the API serves each step's as its own stream, "SSE
// log stream, history then live": GET /api/v1/runs/{id}/steps/{step}/logs. So this reads every
// step's stream at once, or those of the steps named after the run, and writes each line on
// standard output behind the dispatch it comes from, until every stream has said its step's log is
// over. A step that has not started yet is waited for, and a finished run is its history.
//
// # Resuming
//
// A stream cut off before it said the log was over is asked again with Last-Event-ID naming the
// last event it gave, which the API resumes after: "open log streams reconnect elsewhere and
// resume from their last position". Nothing is printed twice: the one event the API sends again
// on such a resume, the end of a dispatch it had already let go of, is recognised and dropped.
// A connection that stays silent past three of the API's keep-alive intervals is taken for one
// that died without saying so, which a proxy or a laptop changing networks leaves behind, and
// asked again the same way.
//
// Asking again waits longer each time, until an event arrives. A stream that could not be
// reopened after logGiveUpAfter attempts in a row, each answered by nothing or by a failure that
// may pass, is given up on, with exit 4: whatever the log still holds is the installation's to
// say, and nothing here can. A stream that opened is never counted towards that, even one cut
// again before it said anything, since a step waiting on another sends nothing but keep-alives
// and a proxy that cuts connections after thirty seconds would otherwise end a healthy follow. A
// refusal, a run or a step that is not there or a credential not accepted, is not asked again.
//
// # What goes where
//
// The lines are the command's answer and go to standard output, each written whole even while
// several streams are read at once. What is said about a log rather than in it, lines that cannot
// be read back, a log the runner cut, a stream being asked again, goes to standard error.

// The pace of asking again: the first wait, the longest, how many attempts in a row that open
// nothing are made before giving up, and how long a connection may stay silent before it is taken
// for dead.
var (
	logRetryFirst  = time.Second
	logRetryAtMost = 30 * time.Second
	logGiveUpAfter = 8
	logSilence     = 45 * time.Second
)

// logEventMaxBytes bounds one event of a stream. A line is at most what one shipment carried, a
// mebibyte, and its text is escaped in JSON, which at worst multiplies it by six.
const logEventMaxBytes = 8 << 20

func logs(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk logs", "agk logs <run> [<step>...] [--server <url>]")
	server := fs.String("server", "", "The installation the run is on. Defaults to "+serverVariable+".")
	named, code, ok := positional(fs, args)
	if !ok {
		return code
	}
	if len(named) == 0 {
		fmt.Fprintln(e.Err, "agk logs names the run whose logs to follow, and after it any of its steps")
		return exitUsage
	}
	at, ok := reach(e, *server)
	if !ok {
		return exitUsage
	}
	run, steps := named[0], named[1:]

	if len(steps) == 0 {
		var d db.RunDetail
		if err := at.getJSON(ctx, "/api/v1/runs/"+url.PathEscape(run), &d); err != nil {
			fmt.Fprintf(e.Err, "%s\n", aboutRun(run, err))
			if errors.Is(err, errUnreachable) {
				return exitNoOutcome
			}
			return exitRefused
		}
		for _, s := range d.Steps {
			steps = append(steps, string(s.Step))
		}
	}

	out, errs := &serial{w: e.Out}, &serial{w: e.Err}
	codes := make([]int, len(steps))
	var wg sync.WaitGroup
	for i, step := range steps {
		wg.Go(func() {
			s := &stepLog{at: at, run: run, step: step, out: out, errs: errs}
			codes[i] = s.follow(ctx)
		})
	}
	wg.Wait()
	// An interrupt is how a person stops following, and a stream it stopped leaves with 0; one
	// already refused or given up on by then still says so.
	worst := exitSucceeded
	for _, c := range codes {
		// No outcome outweighs a refusal, which outweighs a log followed to its end: the
		// first is the one that says the installation may hold more than was printed.
		if c == exitNoOutcome || (c == exitRefused && worst == exitSucceeded) {
			worst = c
		}
	}
	return worst
}

// positional reads flags and the words between them, in any order, so that agk logs <run> --server
// <url> reads as it is typed: flag stops at the first word that is not a flag.
func positional(fs *flag.FlagSet, args []string) ([]string, int, bool) {
	var words []string
	for {
		if code, ok := parse(fs, args); !ok {
			return nil, code, false
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return words, exitSucceeded, true
		}
		if rest[0] == "--" {
			return append(words, rest[1:]...), exitSucceeded, true
		}
		words = append(words, rest[0])
		args = rest[1:]
	}
}

// stepLog is one step's stream being followed: where it stands, so that a reconnect resumes
// there, and what has been printed, so that nothing is printed twice.
type stepLog struct {
	at   remote
	run  string
	step string

	out, errs io.Writer

	// last is the id of the last event with one, which Last-Event-ID names on a reconnect.
	last string

	// labels names each dispatch as its lines are prefixed, printed the highest line written of
	// each, and ended the dispatches whose end was said.
	labels  map[string]string
	printed map[string]int
	ended   map[string]bool
}

// follow reads the step's stream to its end, asking again where it is cut off, and answers the
// exit code it leaves with.
func (s *stepLog) follow(ctx context.Context) int {
	s.labels, s.printed, s.ended = map[string]string{}, map[string]int{}, map[string]bool{}
	wait, failed := logRetryFirst, 0
	for {
		over, opened, heard, err := s.once(ctx)
		switch {
		case over:
			return exitSucceeded
		case ctx.Err() != nil:
			return exitSucceeded
		case err != nil && !passing(err) && !errors.Is(err, errCutOff):
			fmt.Fprintf(s.errs, "%s: %s\n", s.step, s.refusedWith(err))
			return exitRefused
		}
		if heard {
			wait = logRetryFirst
		}
		if opened {
			failed = 0
		}
		failed++
		if failed > logGiveUpAfter {
			fmt.Fprintf(s.errs, "%s: %s, and the log was asked for %d times in a row without the installation opening it: whatever it still holds is at %s, and agk logs %s %s asks again\n",
				s.step, err, logGiveUpAfter, s.at.base, s.run, s.step)
			return exitNoOutcome
		}
		fmt.Fprintf(s.errs, "%s: %s: asking again in %s, from where it stood\n", s.step, err, wait)
		pause := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			pause.Stop()
			return exitSucceeded
		case <-pause.C:
		}
		wait = min(wait*2, logRetryAtMost)
	}
}

// errCutOff is a stream that stopped before it said the step's log was over.
var errCutOff = errors.New("the log was cut off")

// refusedWith says why the API refused a step's log, in the command line's words.
func (s *stepLog) refusedWith(err error) string {
	switch statusOf(err) {
	case http.StatusUnauthorized:
		return fmt.Sprintf("the installation did not accept the credential in %s", tokenVariable)
	case http.StatusNotFound:
		return fmt.Sprintf("run %s has no such step, or is not there, or not yours", s.run)
	}
	return err.Error()
}

// once reads the stream from where it stands until it ends or is cut off, and answers whether the
// step's log is over, whether the stream opened, whether any event arrived, and why it stopped
// where it is not over.
func (s *stepLog) once(ctx context.Context) (bool, bool, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := s.at.request(ctx, http.MethodGet, fmt.Sprintf("/api/v1/runs/%s/steps/%s/logs", url.PathEscape(s.run), url.PathEscape(s.step)), nil)
	if err != nil {
		return false, false, false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if s.last != "" {
		req.Header.Set("Last-Event-ID", s.last)
	}

	// Silence past logSilence cancels the request, which ends the read below: a connection
	// that died without a word is otherwise read from for ever.
	var hushed atomic.Bool
	silent := time.AfterFunc(logSilence, func() {
		hushed.Store(true)
		cancel()
	})
	defer silent.Stop()
	quiet := fmt.Errorf("%w: nothing was heard for %s", errCutOff, logSilence)

	// No timeout on the client: a stream lasts as long as its step.
	answer, err := client(0).Do(req)
	if err != nil {
		if hushed.Load() {
			return false, false, false, quiet
		}
		return false, false, false, fmt.Errorf("%w at %s: %v", errUnreachable, s.at.base, err)
	}
	defer answer.Body.Close()
	if answer.StatusCode != http.StatusOK {
		return false, false, false, refusedBy(answer)
	}

	heard := false
	read := bufio.NewScanner(answer.Body)
	read.Buffer(make([]byte, 0, 64<<10), logEventMaxBytes)
	var ev sseEvent
	for read.Scan() {
		silent.Reset(logSilence)
		line := read.Text()
		if line != "" {
			ev.field(line)
			continue
		}
		if ev.name == "" && ev.data == "" {
			// A comment, which keeps the connection alive and says nothing.
			ev = sseEvent{}
			continue
		}
		heard = true
		if s.take(ev) {
			return true, true, heard, nil
		}
		ev = sseEvent{}
	}
	switch {
	case hushed.Load():
		return false, true, heard, quiet
	case read.Err() != nil:
		return false, true, heard, fmt.Errorf("%w: %v", errCutOff, read.Err())
	}
	return false, true, heard, errCutOff
}

// sseEvent is one server-sent event as it is read, field by field.
type sseEvent struct {
	id, name, data string
	hasID          bool
}

// field reads one line of an event: "the field name, a colon, an optional space and the value".
func (e *sseEvent) field(line string) {
	if strings.HasPrefix(line, ":") {
		return
	}
	name, value, _ := strings.Cut(line, ":")
	value = strings.TrimPrefix(value, " ")
	switch name {
	case "id":
		e.id, e.hasID = value, true
	case "event":
		e.name = value
	case "data":
		if e.data != "" {
			e.data += "\n"
		}
		e.data += value
	}
}

// take acts on one event, and answers whether it says the step's log is over.
func (s *stepLog) take(ev sseEvent) bool {
	if ev.hasID && ev.id != "" {
		s.last = ev.id
	}
	switch ev.name {
	case "dispatch":
		var d struct {
			TaskID  string     `json:"task_id"`
			Attempt int        `json:"attempt"`
			Shard   *agk.Shard `json:"shard"`
			Requeue int        `json:"requeue"`
		}
		if json.Unmarshal([]byte(ev.data), &d) != nil {
			return false
		}
		label := s.step
		if d.Shard != nil && !d.Shard.IsZero() {
			label += " " + d.Shard.String()
		}
		if d.Attempt > 1 {
			label += fmt.Sprintf(", attempt %d", d.Attempt)
		}
		if d.Requeue > 0 {
			label += fmt.Sprintf(", requeue %d", d.Requeue)
		}
		s.labels[d.TaskID] = label
	case "line":
		var l struct {
			TaskID string `json:"task_id"`
			Line   int    `json:"line"`
			Text   string `json:"text"`
		}
		if json.Unmarshal([]byte(ev.data), &l) != nil || l.Line <= s.printed[l.TaskID] {
			return false
		}
		s.printed[l.TaskID] = l.Line
		fmt.Fprintf(s.out, "%s | %s\n", s.label(l.TaskID), l.Text)
	case "gap":
		var g struct {
			TaskID    string `json:"task_id"`
			FirstLine int    `json:"first_line"`
			Lines     int    `json:"lines"`
			Reason    string `json:"reason"`
		}
		if json.Unmarshal([]byte(ev.data), &g) != nil {
			return false
		}
		s.printed[g.TaskID] = max(s.printed[g.TaskID], g.FirstLine+g.Lines-1)
		fmt.Fprintf(s.errs, "%s: %s from line %d: %s\n", s.label(g.TaskID), counted(g.Lines, "line is missing", "lines are missing"), g.FirstLine, g.Reason)
	case "dispatch_end":
		var d struct {
			TaskID    string `json:"task_id"`
			Lines     int    `json:"lines"`
			Truncated bool   `json:"truncated"`
			Final     bool   `json:"final"`
		}
		if json.Unmarshal([]byte(ev.data), &d) != nil || s.ended[d.TaskID] {
			// Sent again on a resume that followed it, and said once already.
			return false
		}
		s.ended[d.TaskID] = true
		if d.Truncated {
			fmt.Fprintf(s.errs, "%s: the runner cut this log at %s, where its log_max_bytes or log_max_lines stopped it\n", s.label(d.TaskID), counted(d.Lines, "line", "lines"))
		}
		if !d.Final {
			fmt.Fprintf(s.errs, "%s: its runner never closed this log, and the installation stopped waiting for it at %s: the task was lost or stopped, and later lines are read by asking again\n", s.label(d.TaskID), counted(d.Lines, "line", "lines"))
		}
	case "end":
		return true
	}
	return false
}

// label is how a dispatch's lines are prefixed: its step, its shard, and its attempt and requeue
// where either tells it apart from another dispatch of the same shard.
func (s *stepLog) label(task string) string {
	if l, ok := s.labels[task]; ok {
		return l
	}
	return s.step
}
