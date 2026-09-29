package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
)

// Memoisation: "With cache: true, a hit republishes the same envelopes without starting a
// container. It never crosses a namespace boundary."
//
// The evaluator makes the key of what it knows, the image, the step's script, the resolved
// parameters and the items on each input port. What it cannot know is the tree the step is
// handed, and a script that reads a file of the repository does something else once the file
// changes, whatever else is the same: a key without the tree would republish what the old file
// made. So the controller adds the files the step's selectors place, each by its path, where it
// goes, its mode and its digest, which is also what keeps a step whose files a commit left alone
// finding its entry across commits.
//
// A task is looked up once the plan is decided and before anything of it is counted against the
// quota or written, since a hit holds no slot and starts nothing. A hit is recorded as the
// task's success, with the envelopes the entry names read back out of the store and stamped as
// this run's, since a step's shards are concatenated into what it publishes and one batch names
// one run: the items are the same, which is what republishing them is. What the items' files
// name stays the run that made them, and lives as long as that run keeps them.

// memoised records a hit for each task of the plan the cache holds an entry for, and answers the
// plan the evaluator makes once they are recorded, and whether it recorded any. An entry naming
// something no longer there is removed, and the task goes on to be handed out.
func (co *Core) memoised(ctx context.Context, ev *graph.Evaluator, namespace string, plan graph.Plan, trees map[string][]db.TreeFile, now time.Time) (graph.Plan, bool, error) {
	var hits []graph.Result
	for _, t := range plan.Start {
		if t.CacheKey == "" {
			continue
		}
		r, ok, err := co.hit(ctx, ev, namespace, t, trees, now)
		if err != nil {
			return plan, false, err
		}
		if ok {
			hits = append(hits, r)
		}
	}
	if len(hits) == 0 {
		return plan, false, nil
	}
	for _, r := range hits {
		if err := ev.Record(r, now); err != nil {
			return plan, false, fmt.Errorf("the hit of %s could not be recorded: %w", r.Task, err)
		}
	}
	next, err := ev.Next(now)
	if err != nil {
		return plan, false, err
	}
	// The stops of the pass that found the hits go out with the plan that follows them, since
	// the evaluator names a stop only in the pass that ends its task.
	for _, s := range plan.Stop {
		if !slices.Contains(next.Stop, s) {
			next.Stop = append(next.Stop, s)
		}
	}
	return next, true, nil
}

// hit answers the success a cache entry makes of one task, where the cache holds one under its key
// and everything the entry names is still there.
func (co *Core) hit(ctx context.Context, ev *graph.Evaluator, namespace string, t graph.Task, trees map[string][]db.TreeFile, now time.Time) (graph.Result, bool, error) {
	tree, held := trees[t.Commit]
	if !held {
		if err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
			var err error
			tree, err = w.Tree(ctx, namespace, t.Workflow, t.Commit)
			return err
		}); err != nil {
			if errors.Is(err, db.ErrNoVersion) || errors.Is(err, db.ErrNoTree) {
				// No tree to key it by is no entry to find, and the task goes out
				// to meet the refusal its redemption answers.
				return graph.Result{}, false, nil
			}
			return graph.Result{}, false, fmt.Errorf("the tree of %s@%s could not be read: %w", t.Workflow, t.Commit, err)
		}
		trees[t.Commit] = tree
	}
	key, err := memoKey(namespace, t, tree)
	if err != nil {
		// A tree the step's files cannot be laid out over is a task the redemption
		// refuses, and nothing a cache could hold.
		return graph.Result{}, false, nil
	}

	var m db.Memo
	err = co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		m, err = w.Memo(ctx, namespace, key)
		return err
	})
	if errors.Is(err, db.ErrNoMemo) {
		return graph.Result{}, false, nil
	}
	if err != nil {
		return graph.Result{}, false, err
	}

	outputs, files, ok := co.memoOutputs(ctx, namespace, t, m)
	if ok {
		err = co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
			var err error
			ok, err = w.MemoAlive(ctx, namespace, m, files)
			return err
		})
		if err != nil {
			return graph.Result{}, false, err
		}
	}
	if !ok {
		if err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
			return w.Forget(ctx, namespace, key)
		}); err != nil {
			return graph.Result{}, false, err
		}
		return graph.Result{}, false, nil
	}

	// The shard's dispatch, which a terminal result has to name to be heard.
	requeue := 0
	for _, sh := range ev.State().Steps[t.Step].Shards {
		if sh.Shard == t.Shard {
			requeue = sh.Requeue
		}
	}
	for port, e := range outputs {
		e.Meta.RunID, e.Meta.Step, e.Meta.Port, e.Meta.Attempt, e.Meta.ProducedAt = t.Run, t.Step, port, t.Attempt, now.UTC()
		outputs[port] = e
	}
	return graph.Result{
		Task: t.ID, State: agk.TaskSucceeded, Requeue: requeue, FinishedAt: now,
		Outputs: outputs, MemoisedFrom: m.Run,
	}, true, nil
}

// memoOutputs reads back the envelopes an entry names, and the files their items name, and says
// whether they are the ports the task publishes: an entry that names others, or an envelope
// gone from the store or not the one its digest names, is no hit.
func (co *Core) memoOutputs(ctx context.Context, namespace string, t graph.Task, m db.Memo) (map[agk.Port]agk.Envelope, []agk.URI, bool) {
	if len(m.Ports) != len(t.Outputs) {
		return nil, nil, false
	}
	outputs := make(map[agk.Port]agk.Envelope, len(m.Ports))
	var files []agk.URI
	for _, p := range m.Ports {
		if !slices.Contains(t.Outputs, p.Port) {
			return nil, nil, false
		}
		e, err := get(ctx, namespace, co.objects, EnvelopeRef{Digest: strings.TrimPrefix(p.Digest, "sha256:")}, co.limits)
		if err != nil || e.Meta.Count != p.Items {
			return nil, nil, false
		}
		for _, item := range e.Items {
			for _, f := range item.Files {
				files = append(files, f.URI)
			}
		}
		outputs[p.Port] = e
	}
	return outputs, files, true
}

// memoKey is the key a cache entry of a task is kept under: the evaluator's key, and the files of
// the tree the step's selectors place, each by its path, where it goes, its mode and its digest,
// prefixed by the namespace as the evaluator's is.
func memoKey(namespace string, t graph.Task, tree []db.TreeFile) (string, error) {
	paths := make([]string, 0, len(tree))
	byPath := make(map[string]db.TreeFile, len(tree))
	for _, f := range tree {
		paths = append(paths, f.Path)
		byPath[f.Path] = f
	}
	placed, err := graph.Place(t.Files, paths)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(t.CacheKey))
	h.Write([]byte{0})
	for _, p := range placed {
		f := byPath[p.Path]
		mode := f.Mode
		if p.Mode != "" {
			mode = p.Mode
		}
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00", p.Path, p.To, mode, f.SHA256)
	}
	return namespace + "/sha256/" + hex.EncodeToString(h.Sum(nil)), nil
}

// memoOf is the cache entry a cached task's success makes: what it published, under the key it
// was handed out with and the tree it was handed.
func memoOf(key string, run agk.RunID, step agk.Step, outputs []Output) db.Memo {
	m := db.Memo{Key: key, Run: run, Step: step}
	for _, o := range outputs {
		m.Ports = append(m.Ports, db.MemoPort{Port: o.Port, Digest: "sha256:" + o.Digest, Items: o.Items})
	}
	return m
}
