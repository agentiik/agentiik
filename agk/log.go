package agk

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// logPrefix is the scheme and the second kind it names. An artifact URI says run before it
// says which one, and a log URI says log, so the two are told apart by reading forwards.
const logPrefix = "agk://log/"

// logForm is the shape, written out for the errors that refuse a departure from it.
const logForm = "agk://log/<run>/<task>"

// LogURI is where one task's log is addressed from.
//
// A log belongs to one task and not to one port, which is why it is not addressed like an
// artifact. A step fanned out over eight shards has eight logs and no port to tell them
// apart, and a task identifier tells them apart exactly, that being what a task identifier
// is for.
//
// The run is named as well as the task, though a task identifier already begins with its
// run. The repetition is for the reader and for the store: a log is prefixed by its run the
// way every other object is prefixed by its namespace, so the logs of a run are one prefix
// and a retention sweep over them is one listing.
type LogURI struct {
	Run  RunID  `json:"-"`
	Task TaskID `json:"-"`
}

// String writes the logical form.
//
// The task identifier carries slashes, so it is escaped: run/step/attempt/index/of inside a
// path segment would otherwise read as five more segments and a parser would have no way to
// tell where the task began.
func (l LogURI) String() string {
	return logPrefix + string(l.Run) + "/" + url.PathEscape(string(l.Task))
}

// IsZero says there is no log. A task that wrote nothing has no log to address, which is not
// the same as a log at an address that happens to be empty.
func (l LogURI) IsZero() bool { return l.Run == "" && l.Task == "" }

// MarshalJSON writes the URI as the single string a result message carries, and null where there
// is no log.
//
// Null rather than the empty string, and rather than the shape the fields would compose to. The
// zero value would otherwise travel as agk://log// and come back refused, which is a task with
// no log being a task whose result cannot be read: the one case that has to work is the one
// where nothing happened.
func (l LogURI) MarshalJSON() ([]byte, error) {
	if l.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(l.String())
}

// UnmarshalJSON reads one and refuses anything else, an artifact URI included: the two kinds
// are separate so that a reader can tell what it has without asking what it points at. Null and
// an absent field are both a task with no log.
func (l *LogURI) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*l = LogURI{}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return reject("", "a log URI is a string, written as %s, or null where there is no log", logForm)
	}
	p, err := ParseLogURI(s)
	if err != nil {
		return reject("", "%s", err)
	}
	*l = p
	return nil
}

// ParseLogURI reads the logical form.
func ParseLogURI(s string) (LogURI, error) {
	rest, ok := strings.CutPrefix(s, logPrefix)
	if !ok {
		return LogURI{}, fmt.Errorf("%q is not a log URI: one is written %s", s, logForm)
	}
	run, escaped, ok := strings.Cut(rest, "/")
	if !ok || escaped == "" {
		return LogURI{}, fmt.Errorf("%q is not a log URI: one is written %s, and names a run and the task whose log it is", s, logForm)
	}
	task, err := url.PathUnescape(escaped)
	if err != nil {
		return LogURI{}, fmt.Errorf("%q is not a log URI: the task is not escaped as a path segment: %w", s, err)
	}

	l := LogURI{Run: RunID(run), Task: TaskID(task)}
	if err := l.Run.Validate(); err != nil {
		return LogURI{}, fmt.Errorf("%q is not a log URI: %w", s, err)
	}
	if err := l.Task.Validate(); err != nil {
		return LogURI{}, fmt.Errorf("%q is not a log URI: %w", s, err)
	}
	// The task's own run has to be the run the URI names, or the URI addresses one run's
	// prefix and another run's task, and a sweep over a run's logs would miss it.
	if theRun, _, _, _, err := ParseTaskID(string(l.Task)); err == nil && theRun != l.Run {
		return LogURI{}, fmt.Errorf("%q is not a log URI: it names run %s and a task of run %s", s, l.Run, theRun)
	}
	return l, nil
}

// NewLogURI addresses the log of one task.
func NewLogURI(task TaskID) (LogURI, error) {
	run, _, _, _, err := ParseTaskID(string(task))
	if err != nil {
		return LogURI{}, fmt.Errorf("the log of %q: %w", task, err)
	}
	return LogURI{Run: run, Task: task}, nil
}
