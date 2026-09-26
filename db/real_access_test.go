package db

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// The typed paths over grants, namespaces, the authentication policy and the bootstrap state.

func TestTheGrantsThatApplyAreTheOnesAskedFor(t *testing.T) {
	pool := identity(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, u := range []string{"alice", "bob"} {
			if err := w.CreateUser(ctx, User{Login: u, DisplayName: u}); err != nil {
				return err
			}
		}
		return w.CreateGroup(ctx, "team-finance")
	})
	in := func(namespace string, fn func(context.Context, *NS) error) {
		t.Helper()
		if err := pool.In(ctx, namespace, fn); err != nil {
			t.Fatal(err)
		}
	}
	in("finance", func(ctx context.Context, n *NS) error {
		for _, g := range []AccessGrant{
			{ID: "01JQ3M8A", Principal: "group:team-finance", Role: "viewer", GrantedBy: "operator"},
			{ID: "01JQ3M8B", Workflow: "monthly-invoicing", Principal: "alice", Role: "operator", GrantedBy: "bob"},
			{ID: "01JQ3M8C", Workflow: "monthly-invoicing", Principal: "alice", Deny: "run:read_data", GrantedBy: "bob"},
			{ID: "01JQ3M8D", Principal: "alice", Role: "editor", GrantedBy: "bob", ExpiresAt: now},
			{ID: "01JQ3M8E", Principal: "bob", Role: "owner", GrantedBy: "operator"},
		} {
			if err := n.GrantAccess(ctx, g); err != nil {
				return err
			}
		}
		return nil
	})
	in("team-ops", func(ctx context.Context, n *NS) error {
		return n.GrantAccess(ctx, AccessGrant{ID: "01JQ3M8F", Principal: "alice", Role: "viewer", GrantedBy: "operator"})
	})

	ids := func(grants []AccessGrant) []string {
		var out []string
		for _, g := range grants {
			out = append(out, g.ID)
		}
		slices.Sort(out)
		return out
	}
	alice := []string{"alice", "group:team-finance"}
	in("finance", func(ctx context.Context, n *NS) error {
		onWorkflow, err := n.AccessGrantsFor(ctx, alice, "monthly-invoicing", now)
		if err != nil {
			return err
		}
		if got := ids(onWorkflow); !slices.Equal(got, []string{"01JQ3M8A", "01JQ3M8B", "01JQ3M8C"}) {
			t.Errorf("on the workflow, alice holds %v: the group's, her own on it, not bob's and not the one expired", got)
		}
		onNamespace, err := n.AccessGrantsFor(ctx, alice, "", now)
		if err != nil {
			return err
		}
		if got := ids(onNamespace); !slices.Equal(got, []string{"01JQ3M8A"}) {
			t.Errorf("on the namespace, alice holds %v", got)
		}
		all, err := n.AccessGrants(ctx)
		if err != nil {
			return err
		}
		if len(all) != 5 {
			t.Errorf("finance lists %d grants, and wrote 5", len(all))
		}
		for _, g := range all {
			if g.ID == "01JQ3M8C" && (g.Deny != "run:read_data" || g.Role != "" || g.Workflow != "monthly-invoicing" || g.Namespace != "finance") {
				t.Errorf("the deny reads as %+v", g)
			}
		}
		return nil
	})
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		across, err := w.AccessGrantsAcross(ctx, alice, now)
		if err != nil {
			return err
		}
		if got := ids(across); !slices.Equal(got, []string{"01JQ3M8A", "01JQ3M8B", "01JQ3M8C", "01JQ3M8F"}) {
			t.Errorf("across the namespaces, alice holds %v", got)
		}
		return nil
	})

	for _, c := range []struct {
		what  string
		grant AccessGrant
		want  error
	}{
		{"nobody", AccessGrant{ID: "01JQ3M8G", Principal: "carol", Role: "viewer", GrantedBy: "bob"}, ErrNoPrincipal},
		{"a workflow that is not there", AccessGrant{ID: "01JQ3M8G", Workflow: "nightly", Principal: "alice", Role: "viewer", GrantedBy: "bob"}, ErrNoWorkflow},
	} {
		err := pool.In(ctx, "finance", func(ctx context.Context, n *NS) error { return n.GrantAccess(ctx, c.grant) })
		if !errors.Is(err, c.want) {
			t.Errorf("a grant to %s was answered %v", c.what, err)
		}
	}

	// Revoked in its own namespace, and not from another's handle, which cannot see it.
	err := pool.In(ctx, "team-ops", func(ctx context.Context, n *NS) error { return n.RevokeAccess(ctx, "01JQ3M8E") })
	if !errors.Is(err, ErrNoAccessGrant) {
		t.Errorf("team-ops revoked a grant of finance, answered %v", err)
	}
	in("finance", func(ctx context.Context, n *NS) error { return n.RevokeAccess(ctx, "01JQ3M8E") })
	err = pool.In(ctx, "finance", func(ctx context.Context, n *NS) error { return n.RevokeAccess(ctx, "01JQ3M8E") })
	if !errors.Is(err, ErrNoAccessGrant) {
		t.Errorf("a grant revoked twice was answered %v", err)
	}
}

func TestANamespaceCarriesItsKindOwnerAndQuotas(t *testing.T) {
	pool := identity(t)
	ctx := t.Context()
	var before, after, kept Namespace
	var listed []Namespace
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.CreateGroup(ctx, "finance-leads"); err != nil {
			return err
		}
		var err error
		if before, err = w.NamespaceNamed(ctx, "finance"); err != nil {
			return err
		}
		if err := w.SetOwner(ctx, "finance", "group:finance-leads"); err != nil {
			return err
		}
		if err := w.SetQuotas(ctx, "finance", Quotas{MaxConcurrentTasks: 7, MaxRetentionDays: 180, MaxRunsPerHour: 500,
			MaxArtifactBytes: 500 << 30, MaxRunDuration: "4h", AllowedRunnerPools: []string{"default", "dmz"}}); err != nil {
			return err
		}
		if after, err = w.NamespaceNamed(ctx, "finance"); err != nil {
			return err
		}
		if err := w.SetQuotas(ctx, "finance", Quotas{}); err != nil {
			return err
		}
		if kept, err = w.NamespaceNamed(ctx, "finance"); err != nil {
			return err
		}
		listed, err = w.Namespaces(ctx)
		return err
	})
	same := func(a, b Quotas) bool {
		return slices.Equal(a.AllowedRunnerPools, b.AllowedRunnerPools) &&
			a.MaxConcurrentTasks == b.MaxConcurrentTasks && a.MaxRetentionDays == b.MaxRetentionDays &&
			a.MaxRunsPerHour == b.MaxRunsPerHour && a.MaxArtifactBytes == b.MaxArtifactBytes &&
			a.MaxRunDuration == b.MaxRunDuration
	}
	if before.Kind != NamespaceShared || before.Owner != "" || !same(before.Quotas, Quotas{MaxConcurrentTasks: 20, MaxRetentionDays: 90}) {
		t.Errorf("a namespace nobody set anything on reads as %+v", before)
	}
	want := Quotas{MaxConcurrentTasks: 7, MaxRetentionDays: 180, MaxRunsPerHour: 500, MaxArtifactBytes: 500 << 30,
		MaxRunDuration: "4h", AllowedRunnerPools: []string{"default", "dmz"}}
	if after.Owner != "group:finance-leads" || !same(after.Quotas, want) {
		t.Errorf("the namespace reads as %+v", after)
	}
	// The two a namespace always has are kept, and the others bound nothing again.
	if !same(kept.Quotas, Quotas{MaxConcurrentTasks: 7, MaxRetentionDays: 180}) {
		t.Errorf("quotas written as nothing read as %+v", kept.Quotas)
	}
	if len(listed) != 2 || listed[0].Name != "finance" || listed[1].Name != "team-ops" {
		t.Errorf("the namespaces are listed as %+v", listed)
	}

	for what, fn := range map[string]func(context.Context, *Wide) error{
		"an owner nobody is": func(ctx context.Context, w *Wide) error { return w.SetOwner(ctx, "finance", "carol") },
		"a namespace nobody created": func(ctx context.Context, w *Wide) error {
			_, err := w.NamespaceNamed(ctx, "nowhere")
			return err
		},
	} {
		err := pool.Installation(ctx, NamespaceAdministration, fn)
		if !errors.Is(err, ErrNoPrincipal) && !errors.Is(err, ErrNoNamespace) {
			t.Errorf("%s was answered %v", what, err)
		}
	}
}

func TestThePolicyAndTheBootstrapTokenAreTheInstallations(t *testing.T) {
	pool := identity(t)
	ctx := t.Context()
	yes := true
	now := time.Now().UTC().Truncate(time.Microsecond)
	var defaults, forbidden, tightened, loosened AuthPolicy
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		var err error
		if defaults, err = w.InstallationPolicy(ctx); err != nil {
			return err
		}
		written := defaults
		written.Password = "forbidden"
		if err := w.SetInstallationPolicy(ctx, written, now); err != nil {
			return err
		}
		if forbidden, err = w.InstallationPolicy(ctx); err != nil {
			return err
		}
		if err := w.SetNamespacePolicy(ctx, "finance", AuthPolicy{DeviceBoundOnly: &yes}, now); err != nil {
			return err
		}
		if tightened, err = w.NamespacePolicy(ctx, "finance"); err != nil {
			return err
		}
		if err := w.SetNamespacePolicy(ctx, "finance", AuthPolicy{}, now); err != nil {
			return err
		}
		loosened, err = w.NamespacePolicy(ctx, "finance")
		return err
	})
	no := false
	if defaults.Password != "allowed" || defaults.Passkey != "required" || defaults.UserVerification != "required" ||
		defaults.DeviceBoundOnly == nil || *defaults.DeviceBoundOnly || defaults.MinPasskeys != 2 {
		t.Errorf("the installation's policy starts as %+v", defaults)
	}
	if forbidden.Password != "forbidden" || forbidden.MinPasskeys != 2 {
		t.Errorf("the installation's policy reads as %+v once passwords are forbidden", forbidden)
	}
	if tightened.DeviceBoundOnly == nil || !*tightened.DeviceBoundOnly || tightened.Password != "" || tightened.MinPasskeys != 0 {
		t.Errorf("a namespace requiring device-bound passkeys reads as %+v", tightened)
	}
	if loosened != (AuthPolicy{}) {
		t.Errorf("a namespace that tightens nothing reads as %+v", loosened)
	}
	err := pool.Installation(ctx, Identity, func(ctx context.Context, w *Wide) error {
		return w.SetInstallationPolicy(ctx, AuthPolicy{Password: "allowed", Passkey: "required", DeviceBoundOnly: &no, MinPasskeys: 2}, now)
	})
	if err == nil {
		t.Error("an installation policy with no user_verification was written")
	}

	// The token is kept by its hash until the first administrator enrols, and then never again.
	var start, ended Bootstrap
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		var err error
		if start, err = w.Bootstrap(ctx); err != nil {
			return err
		}
		for i, c := range []struct {
			value   string
			changed bool
		}{{"agk_op_one", true}, {"agk_op_one", false}, {"agk_op_two", true}} {
			if changed, err := w.SetBootstrapToken(ctx, valueHash(c.value)); err != nil || changed != c.changed {
				t.Errorf("writing the token's hash, time %d, was answered %v, %v", i+1, changed, err)
			}
		}
		if first, err := w.EndBootstrap(ctx, now); err != nil || !first {
			t.Errorf("the first enrolment ended the token as %v, %v", first, err)
		}
		if first, err := w.EndBootstrap(ctx, now.Add(time.Hour)); err != nil || first {
			t.Errorf("a second enrolment ended the token as %v, %v", first, err)
		}
		if _, err := w.SetBootstrapToken(ctx, valueHash("agk_op_two")); !errors.Is(err, ErrBootstrapEnded) {
			t.Errorf("the token written again after it ended was answered %v", err)
		}
		ended, err = w.Bootstrap(ctx)
		return err
	})
	if start.Ended() || start.TokenHash != nil {
		t.Errorf("the bootstrap state starts as %+v", start)
	}
	if !ended.Ended() || !ended.EnrolledAt.Equal(now) || ended.TokenHash != nil {
		t.Errorf("the bootstrap state reads as %+v once the first administrator enrolled", ended)
	}
}
