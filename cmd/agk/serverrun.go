package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/cmd/agk/internal/local"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/schema"
)

// agk run on an installation: the workflow a pushed commit holds, started there and followed
// until it ends, on the evaluator agk run --local already uses.
//
// The controller decides a server run through graph, the package that decides a local one, so
// what changes here is who runs the loop and where the containers are, and nothing about what
// a step, a port or a verdict is. The narration is the local run's own, print.go's narration fed
// the transitions GET /api/v1/runs/{id} shows between two readings, and so is the report: a
// workflow read on a laptop and on an installation reads the same.
//
// # What it names
//
// A run is of a commit, since "a version is a commit", and until the installation hosts the
// repository it holds no branch to resolve one from, so the commit is read here as agk push reads
// it: HEAD, or the hash, branch or tag --commit names. A commit nobody pushed is refused by the
// installation as a version it does not have. --ref, a ref the installation resolves, arrives
// with git hosting.
//
// The inputs are bound here, through package schema against the declared inputs the commit holds,
// as a local run binds them: "already held to their declared schemas with required and default
// applied" is what a run's inputs are when they are written, and the API writes them as they
// arrive. So the workflow is read out of the commit, and never off the working copy, which may
// hold another.
//
// # Following it
//
// The run is read again after every interval until it ends, since the API has no stream of a
// run's state, only of a step's log. The interval starts short and lengthens while nothing
// changes, so a run of seconds reads as it happens and one of hours costs a request every few
// seconds.
//
// An interrupt stops the following and not the run. A server run is the installation's, other
// people may be reading it, and a terminal closed on a laptop is not a decision to call off
// work somebody may be waiting for: cancelling is POST /api/v1/runs/{id}/cancel, under
// workflow:run. So the process leaves with exit 4, no outcome determined, and says how to read
// the run again.

// The pace of following a run: the first interval, the longest one, and how long an installation
// that cannot be read is asked again before the command gives up.
var (
	followEvery  = 500 * time.Millisecond
	followAtMost = 5 * time.Second
	followGiveUp = 2 * time.Minute
)

// serverRun is what agk run on an installation was asked.
type serverRun struct {
	entry, namespace, server, commit string

	inputs, inputFiles pairs
	document           string

	json, verbose bool
}

func runOnServer(ctx context.Context, e Env, o serverRun) int {
	at, ok := reach(e, o.server)
	if !ok {
		return exitUsage
	}

	// 1. The commit, and the workflow it holds, read out of git's objects as agk push reads
	// them, so that the inputs are bound against the declaration that version was pushed with.
	path, err := entryOf(e, o.entry)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}
	repo, err := repositoryAt(ctx, path)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}
	sha, err := commitOf(ctx, repo.top, o.commit)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}
	if err := repo.holds(ctx, sha, path); err != nil {
		refusal(e.Err, err)
		return exitRefused
	}
	// Said rather than refused: nothing is registered, and the run is of what was pushed
	// whatever the working copy holds, but somebody with an edit open most likely believes it
	// is what runs.
	if changed, err := dirtyTree(ctx, repo.top); err == nil && len(changed) > 0 {
		fmt.Fprintf(e.Err, "the working tree has uncommitted changes in %s, and what runs is %s as it was committed and pushed, without them\n",
			counted(len(changed), "file", "files"), short(sha))
	}
	files, err := repositoryOf(ctx, repo, sha)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}
	tree := committed(files)
	wf, err := loadCommitted(tree, filepath.Base(path), filepath.Dir(path), sha)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}

	// 2. The inputs, exactly as a local run binds them.
	declared, err := declaredInputs(wf, tree)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}
	supplied, err := suppliedInputs(o.inputs, e.paths(o.inputFiles), e.path(o.document))
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}
	bound, err := schema.Bind(declared, supplied)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}

	// 3. The run.
	workflow := string(wf.Metadata.Name)
	run, err := start(ctx, at, o.namespace, workflow, sha, bound)
	switch {
	case errors.Is(err, errUnreachable):
		fmt.Fprintf(e.Err, "%s\n", err)
		return exitNoOutcome
	case err != nil:
		fmt.Fprintf(e.Err, "%s\n", err)
		return exitRefused
	}
	fmt.Fprintf(e.Err, "run %s of %s/%s@%s started at %s\n", run, o.namespace, workflow, short(sha), at.base)

	// 4. Following it until it ends.
	f := &following{e: e, at: at, run: run, n: &narration{w: e.Err, verbose: o.verbose}}
	detail, code := f.follow(ctx)
	if code != exitSucceeded {
		return code
	}

	report := e.Out
	if o.json {
		report = e.Err
	}
	if detail.State != agk.Succeeded {
		reportEnded(e.Err, detail)
		return exitNotSucceeded
	}
	reportSucceeded(report, at, detail)
	if o.json {
		outputs, err := outputsOf(ctx, at, detail)
		if err != nil {
			fmt.Fprintf(e.Err, "run %s succeeded and its outputs could not be read: %s\n", run, err)
			return exitNoOutcome
		}
		if err := writeOutputs(e.Out, outputs); err != nil {
			refusal(e.Err, err)
			return exitNoOutcome
		}
	}
	return exitSucceeded
}

// start asks the installation for a run of a commit, and answers its identifier.
func start(ctx context.Context, at remote, namespace, workflow, sha string, inputs map[string]any) (string, error) {
	body, err := json.Marshal(api.Start{Commit: sha, Inputs: inputs})
	if err != nil {
		return "", fmt.Errorf("the inputs could not be written: %w", err)
	}
	path := fmt.Sprintf("/api/v1/%s/workflows/%s/runs", namespace, workflow)
	req, err := at.request(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	answer, err := client(answerTimeout).Do(req)
	if err != nil {
		// Sent and never answered is a run that may exist, which nothing here can tell.
		return "", fmt.Errorf("%w at %s: %v, and whether a run was started cannot be said", errUnreachable, at.base, err)
	}
	defer answer.Body.Close()
	if answer.StatusCode != http.StatusAccepted {
		r := refusedBy(answer)
		if passing(r) {
			// A gateway that timed out in front of an API that had already written the run
			// answers this too, so it is no outcome rather than a refusal: a person told that
			// nothing ran would start a second run.
			return "", fmt.Errorf("%w: %s answered %s, and whether a run was started cannot be said: agk status reads runs by identifier, and GET /api/v1/%s/runs lists them", errUnreachable, at.base, r.said, namespace)
		}
		switch r.status {
		case http.StatusUnauthorized:
			return "", fmt.Errorf("the installation did not accept the credential in %s", tokenVariable)
		case http.StatusNotFound:
			// The same answer an inaccessible workflow gets, and a commit never pushed.
			return "", fmt.Errorf("%s/%s@%s is not there, or not yours: a server runs a commit agk push registered, so push it first", namespace, workflow, short(sha))
		}
		return "", fmt.Errorf("the installation refused the run: %s", r.said)
	}
	var started struct {
		Run string `json:"run"`
	}
	if err := json.NewDecoder(answer.Body).Decode(&started); err != nil || started.Run == "" {
		// The run exists, and the Location it is read at names it too.
		if loc := answer.Header.Get("Location"); loc != "" {
			if i := strings.LastIndex(loc, "/"); i >= 0 && loc[i+1:] != "" {
				return loc[i+1:], nil
			}
		}
		return "", fmt.Errorf("%w: a run was started and its answer naming it could not be read", errUnreachable)
	}
	return started.Run, nil
}

// following is one run being followed: what has been said of it, so that nothing is said twice.
type following struct {
	e   Env
	at  remote
	run string
	n   *narration

	verdicts map[agk.Step]agk.Verdict
	tasks    map[dispatchKey]agk.TaskState
}

// shardKey is one shard of one step, whichever attempt or dispatch of it is current.
type shardKey struct {
	step  agk.Step
	index int
}

// dispatchKey is one row of the tasks table: a shard's attempt, and which dispatch of that attempt
// it is, since a loss hands the same key out again under a new row. The API names no row, so the
// dispatch is counted: the rows of one attempt come in the order they were made.
type dispatchKey struct {
	shardKey
	attempt, dispatch int
}

// follow reads the run until it ends, narrating what changed between two readings, and answers
// it as it ended, or the exit code of a following that stopped first.
func (f *following) follow(ctx context.Context) (db.RunDetail, int) {
	every := followEvery
	var failing time.Time
	for {
		var d db.RunDetail
		err := f.at.getJSON(ctx, "/api/v1/runs/"+f.run, &d)
		switch {
		case ctx.Err() != nil:
			f.detached()
			return d, exitNoOutcome
		case err != nil && passing(err):
			if failing.IsZero() {
				failing = f.e.now()
			}
			if f.e.now().Sub(failing) >= followGiveUp {
				fmt.Fprintf(f.e.Err, "%s, and has not answered for %s: run %s goes on there, and agk status %s reads it once it answers\n", err, followGiveUp, f.run, f.run)
				return d, exitNoOutcome
			}
		case err != nil:
			// The run was started, so a refusal now is not the workflow's: a credential
			// revoked meanwhile, or a run deleted with its workflow.
			fmt.Fprintf(f.e.Err, "run %s could not be read: %s\n", f.run, aboutRun(f.run, err))
			return d, exitNoOutcome
		default:
			failing = time.Time{}
			if f.narrate(d) {
				every = followEvery
			} else {
				every = min(every*2, followAtMost)
			}
			if d.State.Terminal() {
				return d, exitSucceeded
			}
		}
		pause := time.NewTimer(every)
		select {
		case <-ctx.Done():
			pause.Stop()
			f.detached()
			return db.RunDetail{}, exitNoOutcome
		case <-pause.C:
		}
	}
}

// detached says what an interrupt left behind: a run that goes on, and how to read it again.
func (f *following) detached() {
	fmt.Fprintf(f.e.Err, "stopped following run %s, which goes on at %s: agk status %s reads how it stands, and agk logs %s follows its logs\n", f.run, f.at.base, f.run, f.run)
}

// narrate says, in the local run's words, every transition one reading shows that the ones before
// it did not, and answers whether there was any.
//
// Each line is placed at the moment the installation recorded, rather than the moment it was
// read, so that a narration read a second late still says how long into the run each thing
// happened. A transition begun and over between two readings is not seen: running is only a line
// where a reading caught it.
func (f *following) narrate(d db.RunDetail) bool {
	if f.verdicts == nil {
		f.verdicts, f.tasks = map[agk.Step]agk.Verdict{}, map[dispatchKey]agk.TaskState{}
	}
	for _, s := range d.Steps {
		f.n.width = max(f.n.width, len(s.Step))
	}
	since := func(t time.Time) time.Duration {
		if d.StartedAt.IsZero() {
			return 0
		}
		if t.IsZero() {
			t = f.e.now()
		}
		return max(t.Sub(d.StartedAt), 0)
	}

	verdicts := map[agk.Step]agk.Verdict{}
	shards := map[agk.Step]int{}
	for _, s := range d.Steps {
		verdicts[s.Step] = s.Verdict
	}
	// Every row, and not only the current dispatch of each shard: a dispatch lost and requeued,
	// or an attempt failed and retried, between two readings is still news, and it is only
	// ever a row the current one has replaced. The tasks come ordered by step, attempt, shard
	// and requeue, which is what counting the dispatches of an attempt rests on.
	var events []local.Event
	made := map[dispatchKey]int{}
	for _, t := range d.Tasks {
		k := dispatchKey{shardKey: shardKey{step: t.Step}, attempt: t.Attempt}
		if t.Shard != nil {
			k.index = t.Shard.Index
			shards[t.Step] = max(shards[t.Step], t.Shard.Of)
		}
		k.dispatch = made[k]
		made[dispatchKey{shardKey: k.shardKey, attempt: k.attempt}]++
		said, seen := f.tasks[k]
		if seen && said == t.State {
			continue
		}
		f.tasks[k] = t.State
		// A shard only just planned is not news, as in a local run.
		if t.State == agk.TaskPending && !seen {
			continue
		}
		ev := local.Event{
			At: since(t.StartedAt), Step: t.Step, Shards: shards[t.Step], Attempt: t.Attempt,
			State: t.State, Verdict: verdicts[t.Step],
		}
		if t.State.Terminal() {
			ev.At = since(t.FinishedAt)
		}
		if t.Shard != nil {
			ev.Shard = *t.Shard
		}
		if t.ExitCode != nil {
			ev.ExitCode = *t.ExitCode
		}
		events = append(events, ev)
	}
	for _, s := range d.Steps {
		if said, seen := f.verdicts[s.Step]; seen && said == s.Verdict {
			continue
		}
		f.verdicts[s.Step] = s.Verdict
		if s.Verdict == agk.VerdictPending {
			continue
		}
		ev := local.Event{At: since(s.StartedAt), Step: s.Step, Shards: shards[s.Step], Verdict: s.Verdict}
		if s.Verdict.Terminal() {
			ev.At = since(s.FinishedAt)
		}
		if len(s.Ports) > 0 {
			ev.Ports = map[agk.Port]int{}
			for port, envelope := range s.Ports {
				ev.Ports[port] = envelope.Items
			}
		}
		events = append(events, ev)
	}
	// In the order they happened, and a shard before its step where both happened at once, as
	// a local run says them.
	slices.SortStableFunc(events, func(a, b local.Event) int { return int(a.At - b.At) })
	for _, ev := range events {
		f.n.say(ev)
	}
	return len(events) > 0
}

// reportSucceeded is the success report of a server run, in the words of a local one: the
// workflow, how long it took and what it ran, then one line per declared output, then where the
// run is read.
func reportSucceeded(w io.Writer, at remote, d db.RunDetail) {
	fmt.Fprintf(w, "%s %s in %s: %s, %s\n", d.Workflow, d.State,
		elapsed(d.FinishedAt.Sub(d.StartedAt)),
		counted(len(d.Steps), "step", "steps"),
		counted(containers(d), "container", "containers"))
	for _, name := range slices.Sorted(maps.Keys(d.Outputs)) {
		fmt.Fprintf(w, "%s: %s\n", name, counted(itemsOf(d.Outputs[name]), "item", "items"))
	}
	fmt.Fprintf(w, "run %s: %s\n", d.Run, at.url(fmt.Sprintf("/api/v1/%s/runs/%s", d.Namespace, d.Run)))
	if fs := failuresOf(d); len(fs) > 0 {
		fmt.Fprintf(w, "%s under continue_on_error:\n", counted(len(fs), "failure tolerated", "failures tolerated"))
		for _, f := range fs {
			failure(w, f)
		}
		fmt.Fprintf(w, "agk logs %s shows what they wrote\n", d.Run)
	}
}

// reportEnded says what became of a server run that did not succeed, as reportFailure says it of a
// local one.
func reportEnded(w io.Writer, d db.RunDetail) {
	fs := failuresOf(d)
	if len(fs) == 0 {
		line := fmt.Sprintf("%s %s: no step failed", d.Workflow, d.State)
		if stopped := stoppedSteps(d); len(stopped) == 1 {
			line += ", and " + stopped[0] + " was stopped"
		} else if len(stopped) > 1 {
			line += ", and " + strings.Join(stopped, ", ") + " were stopped"
		}
		fmt.Fprintln(w, line)
		return
	}
	fmt.Fprintf(w, "%s %s: %s\n", d.Workflow, d.State, counted(len(fs), "step failed", "steps failed"))
	for _, f := range fs {
		failure(w, f)
	}
	fmt.Fprintf(w, "agk logs %s shows what they wrote\n", d.Run)
}

// failuresOf reads the failures a run ended with out of its tasks: the current dispatch of every
// shard whose state a report names, which is failed, timed_out or lost, in step order. An attempt
// a retry moved past is not one, as in a local run, and the current dispatch is the shard's last
// row, since the tasks come ordered by step, attempt, shard and requeue.
func failuresOf(d db.RunDetail) []local.Failure {
	current := map[shardKey]db.TaskSummary{}
	for _, t := range d.Tasks {
		k := shardKey{step: t.Step}
		if t.Shard != nil {
			k.index = t.Shard.Index
		}
		current[k] = t
	}
	var out []local.Failure
	for _, k := range slices.SortedFunc(maps.Keys(current), func(a, b shardKey) int {
		if a.step != b.step {
			return strings.Compare(string(a.step), string(b.step))
		}
		return a.index - b.index
	}) {
		t := current[k]
		switch t.State {
		case agk.TaskFailed, agk.TaskTimedOut, agk.TaskLost:
		default:
			continue
		}
		f := local.Failure{Step: t.Step, Attempt: t.Attempt, State: t.State}
		if t.Shard != nil {
			f.Shard = *t.Shard
		}
		// A code is named for a failure alone, as a local run names it: a stop's 137 or 143
		// is how the container was stopped and not why the step ended.
		if t.State == agk.TaskFailed && t.ExitCode != nil {
			f.HasExit, f.ExitCode, f.Band = true, *t.ExitCode, agk.Band(*t.ExitCode)
		}
		out = append(out, f)
	}
	return out
}

// stoppedSteps names the steps a stop reached, in name order.
func stoppedSteps(d db.RunDetail) []string {
	var out []string
	for _, t := range d.Tasks {
		if t.State != agk.TaskCancelled && t.State != agk.TaskTimedOut {
			continue
		}
		if !slices.Contains(out, string(t.Step)) {
			out = append(out, string(t.Step))
		}
	}
	slices.Sort(out)
	return out
}

// containers counts the dispatches a runner took, which is the containers the run asked for: a
// task no runner redeemed started none.
func containers(d db.RunDetail) int {
	n := 0
	for _, t := range d.Tasks {
		if t.Runner != "" {
			n++
		}
	}
	return n
}

// itemsOf reads how many items a run records of one output, which it keeps as a count beside the
// step and the port the output is a view of.
func itemsOf(output any) int {
	o, _ := output.(map[string]any)
	n, _ := o["count"].(float64)
	return int(n)
}

// outputsOf reads the envelope of every output a run records, which is what -o json writes, in
// the shape a local run writes it.
func outputsOf(ctx context.Context, at remote, d db.RunDetail) (map[string]agk.Envelope, error) {
	out := make(map[string]agk.Envelope, len(d.Outputs))
	for _, name := range slices.Sorted(maps.Keys(d.Outputs)) {
		req, err := at.request(ctx, http.MethodGet, fmt.Sprintf("/api/v1/runs/%s/outputs/%s", d.Run, name), nil)
		if err != nil {
			return nil, err
		}
		answer, err := client(answerTimeout).Do(req)
		if err != nil {
			return nil, fmt.Errorf("%w at %s: %v", errUnreachable, at.base, err)
		}
		if answer.StatusCode != http.StatusOK {
			r := refusedBy(answer)
			answer.Body.Close()
			return nil, fmt.Errorf("%s: %s", name, r.said)
		}
		e, err := agk.Decode(answer.Body, agk.DefaultLimits())
		answer.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out[name] = e
	}
	return out, nil
}
