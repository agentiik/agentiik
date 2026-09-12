package local

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/agentiik/agentiik/agk"
)

// logs is driver.Logs as a file per task: one log, at the path the layout derives from the
// task identifier, which is the same path a failure report names.
//
// A server runner opens a key in the object store and fills in the agk.URI of it. This
// opens a file, because the thing a person does with a failed local run is open the log in
// an editor, and a path they can paste is worth more than a URI nothing here can resolve.
type logs struct{ layout Layout }

// OpenLog creates the file one task's log is written into.
//
// The directories are created here rather than with the run, because the path carries the
// step and a run does not know which of its steps will have run until it has run them. The
// file is truncated: a second attempt has its own file, and a task identifier carries the
// attempt, so an existing file at this path is one a previous delivery of this very task
// wrote and the delivery that is running now is the one that reports.
func (l logs) OpenLog(ctx context.Context, task agk.TaskID) (io.WriteCloser, error) {
	path, err := l.layout.Log(task)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), recordMode); err != nil {
		return nil, fmt.Errorf("local: task %s: the log directory %s could not be prepared: %w", task, filepath.Dir(path), err)
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("local: task %s: the log %s could not be opened: %w", task, path, err)
	}
	return f, nil
}
