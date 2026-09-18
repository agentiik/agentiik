package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The pool, which exists before any machine does.

func TestAPoolCarriesItsPolicyAndCountsWhatIsInIt(t *testing.T) {
	pool, _ := joining(t)
	now := time.Now().UTC()

	var listed []RunnerPool
	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		if err := w.CreateRunnerPool(ctx, RunnerPool{
			Name: "sandboxed", Labels: []string{"runtime=runsc"},
			AcceptedNamespaces: []string{"finance"},
			MaxCPU:             4, MaxMemoryBytes: 1 << 33, MaxDiskBytes: 1 << 36,
			CreatedBy: "admin",
		}); err != nil {
			return err
		}
		var err error
		listed, err = w.RunnerPools(ctx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string]RunnerPool{}
	for _, p := range listed {
		byName[p.Name] = p
	}
	made, ok := byName["sandboxed"]
	switch {
	case !ok:
		t.Fatalf("the listing holds %v", byName)
	case made.MaxCPU != 4 || made.MaxMemoryBytes != 1<<33 || made.MaxDiskBytes != 1<<36:
		t.Errorf("the ceilings came back as %d, %d, %d", made.MaxCPU, made.MaxMemoryBytes, made.MaxDiskBytes)
	case made.Runners != 0 || made.Ready != 0:
		t.Errorf("a pool nothing has joined holds %d runners", made.Runners)
	case made.CreatedBy != "admin" || made.CreatedAt.IsZero():
		t.Errorf("the pool says it was created by %q at %s", made.CreatedBy, made.CreatedAt)
	}

	// A pool with no list of its own accepts every namespace, which is what an installation
	// with one pool has and should not have to write down.
	if !byName["dmz"].Accepts("finance") {
		t.Error("a pool that names no namespace refuses one")
	}
	if made.Accepts("team-ops") {
		t.Error("a pool that names finance accepts team-ops")
	}

	// What it holds is counted from the runners, and a state change moves the count without
	// anything having to remember to write it.
	var runner string
	err = pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		issued, err := w.IssueJoinToken(ctx, "dmz", []string{"zone=dmz"}, "admin", now.Add(time.Hour))
		if err != nil {
			return err
		}
		joined, err := w.Join(ctx, Joining{
			Token: issued.Clear, Labels: []string{"zone=dmz"},
			CPU: 8, MemoryBytes: 1 << 34, DiskBytes: 1 << 38,
			Architecture: "amd64", AgentVersion: "0.2.0",
		}, 30*24*time.Hour, now)
		runner = joined.Runner
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, step := range []struct {
		name           string
		do             func(context.Context, *Wide) error
		runners, ready int
	}{
		{name: "joined", do: func(context.Context, *Wide) error { return nil }, runners: 1, ready: 1},
		{name: "draining", do: func(ctx context.Context, w *Wide) error {
			return w.Drain(ctx, runner, "the host is being retired")
		}, runners: 1, ready: 0},
		{name: "revoked", do: func(ctx context.Context, w *Wide) error {
			return w.Revoke(ctx, runner, "the credential leaked")
		}, runners: 0, ready: 0},
	} {
		var dmz RunnerPool
		err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
			if err := step.do(ctx, w); err != nil {
				return err
			}
			all, err := w.RunnerPools(ctx)
			for _, p := range all {
				if p.Name == "dmz" {
					dmz = p
				}
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if dmz.Runners != step.runners || dmz.Ready != step.ready {
			t.Errorf("%s: the pool holds %d runners, %d ready", step.name, dmz.Runners, dmz.Ready)
		}
	}
}

// "Labels are not self-asserted" is a chain, and this is its first link: a token draws from the
// pool, a machine draws from the token, so a label reaches a machine only where an administrator
// wrote it on a pool first.
func TestATokenCannotPermitALabelItsPoolDoesNotCarry(t *testing.T) {
	pool, _ := joining(t)
	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		_, err := w.IssueJoinToken(ctx, "dmz", []string{"zone=dmz", "zone=lan"}, "admin",
			time.Now().UTC().Add(time.Hour))
		return err
	})
	if err == nil {
		t.Fatal("a token was issued permitting a label its pool does not carry")
	}
}

func TestAJoinTokenForAPoolNobodyCreated(t *testing.T) {
	pool, _ := joining(t)
	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		_, err := w.IssueJoinToken(ctx, "imaginary", nil, "admin", time.Now().UTC().Add(time.Hour))
		return err
	})
	if !errors.Is(err, ErrNoRunnerPool) {
		t.Fatalf("a token for a pool nobody created answered %v", err)
	}
}
