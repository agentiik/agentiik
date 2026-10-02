package db

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/access"
)

// The typed paths over grants, namespaces, the authentication policy and the bootstrap state.

func TestTheGrantsThatApplyAreTheOnesAskedFor(t *testing.T) {
	pool := identity(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, u := range []string{"alice", "bob"} {
			if err := w.CreateUser(ctx, User{Login: u}); err != nil {
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
	finance := access.Scope{Namespace: "finance"}
	invoicing := access.Scope{Namespace: "finance", Workflow: "monthly-invoicing"}
	in("finance", func(ctx context.Context, n *NS) error {
		for _, g := range []access.Grant{
			{ID: "01JQ3M8A", Scope: finance, Principal: "group:team-finance", Role: access.Viewer, GrantedBy: "operator"},
			{ID: "01JQ3M8B", Scope: invoicing, Principal: "alice", Role: access.Operator, GrantedBy: "bob"},
			{ID: "01JQ3M8C", Scope: invoicing, Principal: "alice", Deny: access.RunReadData, GrantedBy: "bob"},
			{ID: "01JQ3M8D", Scope: finance, Principal: "alice", Role: access.Editor, GrantedBy: "bob", ExpiresAt: &now},
			{ID: "01JQ3M8E", Scope: finance, Principal: "bob", Role: access.Owner, GrantedBy: "operator"},
		} {
			if err := n.GrantAccess(ctx, g); err != nil {
				return err
			}
		}
		return nil
	})
	in("team-ops", func(ctx context.Context, n *NS) error {
		return n.GrantAccess(ctx, access.Grant{ID: "01JQ3M8F", Scope: access.Scope{Namespace: "team-ops"}, Principal: "alice",
			Role: access.Viewer, GrantedBy: "operator"})
	})

	ids := func(grants []access.Grant) []string {
		var out []string
		for _, g := range grants {
			out = append(out, g.ID)
		}
		slices.Sort(out)
		return out
	}
	alice := access.Principal{Ref: "alice", Groups: []string{"team-finance"}}
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
			switch {
			case g.ID == "01JQ3M8C" && (g.Deny != access.RunReadData || g.Role != "" || g.Scope != invoicing || g.ExpiresAt != nil):
				t.Errorf("the deny reads as %+v", g)
			case g.ID == "01JQ3M8D" && (g.ExpiresAt == nil || !g.ExpiresAt.Equal(now)):
				t.Errorf("the grant that expired reads as %+v", g)
			}
		}
		// What is read resolves as it was written: alice runs the workflow and reads it through
		// her group, and never reads its data.
		held, err := access.Resolve(alice, onWorkflow, invoicing, now)
		if err != nil {
			return err
		}
		if !held.Has(access.WorkflowRun) || !held.Has(access.WorkflowRead) || held.Has(access.RunReadData) {
			t.Errorf("alice holds %s on the workflow", held)
		}
		return nil
	})
	if err := pool.Installation(ctx, Authorisation, func(ctx context.Context, w *Wide) error {
		across, err := w.AccessGrantsAcross(ctx, alice, now)
		if err != nil {
			return err
		}
		if got := ids(across); !slices.Equal(got, []string{"01JQ3M8A", "01JQ3M8B", "01JQ3M8C", "01JQ3M8F"}) {
			t.Errorf("across the namespaces, alice holds %v", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		what  string
		grant access.Grant
		want  error
	}{
		{"nobody", access.Grant{ID: "01JQ3M8G", Scope: finance, Principal: "carol", Role: access.Viewer, GrantedBy: "bob"}, ErrNoPrincipal},
		{"a workflow that is not there", access.Grant{ID: "01JQ3M8G", Scope: access.Scope{Namespace: "finance", Workflow: "nightly"},
			Principal: "alice", Role: access.Viewer, GrantedBy: "bob"}, ErrNoWorkflow},
	} {
		err := pool.In(ctx, "finance", func(ctx context.Context, n *NS) error { return n.GrantAccess(ctx, c.grant) })
		if !errors.Is(err, c.want) {
			t.Errorf("a grant to %s was answered %v", c.what, err)
		}
	}
	// And one on another namespace, or one access refuses, is not written at all, and is refused
	// in its words before the table is asked.
	for what, c := range map[string]struct {
		grant access.Grant
		says  string
	}{
		"on team-ops":     {access.Grant{ID: "01JQ3M8H", Scope: access.Scope{Namespace: "team-ops"}, Principal: "alice", Role: access.Owner, GrantedBy: "bob"}, "a namespace writes only its own"},
		"denying a role":  {access.Grant{ID: "01JQ3M8H", Scope: finance, Principal: "alice", Deny: "owner", GrantedBy: "bob"}, "owner is a role"},
		"with no granter": {access.Grant{ID: "01JQ3M8H", Scope: finance, Principal: "alice", Role: access.Viewer}, "somebody who granted it"},
	} {
		err := pool.In(ctx, "finance", func(ctx context.Context, n *NS) error { return n.GrantAccess(ctx, c.grant) })
		if err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("a grant %s was answered %v", what, err)
		}
	}

	// Revoked in its own namespace, and not from another's handle, which cannot see it; at the
	// scope it was written at, and not at the other.
	revoke := func(namespace, workflow, id string) (access.Grant, error) {
		var revoked access.Grant
		err := pool.In(ctx, namespace, func(ctx context.Context, n *NS) error {
			var err error
			revoked, err = n.RevokeAccess(ctx, workflow, id)
			return err
		})
		return revoked, err
	}
	for what, c := range map[string]struct{ namespace, workflow, id string }{
		"from team-ops":                         {"team-ops", "", "01JQ3M8E"},
		"a namespace's grant on a workflow":     {"finance", "monthly-invoicing", "01JQ3M8E"},
		"a workflow's grant on its namespace":   {"finance", "", "01JQ3M8B"},
		"a workflow's grant on another of them": {"finance", "payroll", "01JQ3M8B"},
	} {
		if _, err := revoke(c.namespace, c.workflow, c.id); !errors.Is(err, ErrNoAccessGrant) {
			t.Errorf("revoking %s was answered %v", what, err)
		}
	}
	if revoked, err := revoke("finance", "", "01JQ3M8E"); err != nil || revoked.Principal != "bob" || revoked.Role != access.Owner || revoked.Scope != finance {
		t.Errorf("revoking bob's grant answered %+v, %v", revoked, err)
	}
	if revoked, err := revoke("finance", "monthly-invoicing", "01JQ3M8C"); err != nil || revoked.Deny != access.RunReadData || revoked.Scope != invoicing {
		t.Errorf("revoking alice's deny answered %+v, %v", revoked, err)
	}
	if _, err := revoke("finance", "", "01JQ3M8E"); !errors.Is(err, ErrNoAccessGrant) {
		t.Errorf("a grant revoked twice was answered %v", err)
	}

	// Listed at a scope, each with the scope it was written at and none expired, the namespace's
	// first; a namespace or a workflow that is not there told from one holding nothing.
	in("finance", func(ctx context.Context, n *NS) error {
		at, err := n.AccessGrantsAt(ctx, "monthly-invoicing", now)
		if err != nil {
			return err
		}
		var got []string
		for _, g := range at {
			got = append(got, g.ID+" "+g.Scope.String())
		}
		if want := []string{"01JQ3M8A finance", "01JQ3M8B finance/monthly-invoicing"}; !slices.Equal(got, want) {
			t.Errorf("the grants that apply to monthly-invoicing are %q, want %q", got, want)
		}
		if at, err := n.AccessGrantsAt(ctx, "", now); err != nil || len(at) != 1 || at[0].ID != "01JQ3M8A" {
			t.Errorf("the grants on finance are %+v, %v", at, err)
		}
		if _, err := n.AccessGrantsAt(ctx, "nightly", now); !errors.Is(err, ErrNoWorkflow) {
			t.Errorf("the grants on a workflow that is not there were answered %v", err)
		}
		return nil
	})
	err := pool.In(ctx, "nowhere", func(ctx context.Context, n *NS) error {
		_, err := n.AccessGrantsAt(ctx, "", now)
		return err
	})
	if !errors.Is(err, ErrNoNamespace) {
		t.Errorf("the grants of a namespace that is not there were answered %v", err)
	}
}

func TestANamespaceCarriesItsKindOwnerAndQuotas(t *testing.T) {
	pool := identity(t)
	ctx := t.Context()
	var before, after, kept Namespace
	var listed []Namespace
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		return w.CreateGroup(ctx, "finance-leads")
	})
	err := pool.Installation(ctx, NamespaceAdministration, func(ctx context.Context, w *Wide) error {
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
	if err != nil {
		t.Fatal(err)
	}
	same := func(a, b Quotas) bool {
		return slices.Equal(a.AllowedRunnerPools, b.AllowedRunnerPools) &&
			a.MaxConcurrentTasks == b.MaxConcurrentTasks && a.MaxRetentionDays == b.MaxRetentionDays &&
			a.MaxRunsPerHour == b.MaxRunsPerHour && a.MaxArtifactBytes == b.MaxArtifactBytes &&
			a.MaxRunDuration == b.MaxRunDuration
	}
	if before.Kind != NamespaceShared || before.Owner != "" || !same(before.Quotas, Quotas{MaxConcurrentTasks: 20}) {
		t.Errorf("a namespace nobody set anything on reads as %+v", before)
	}
	want := Quotas{MaxConcurrentTasks: 7, MaxRetentionDays: 180, MaxRunsPerHour: 500, MaxArtifactBytes: 500 << 30,
		MaxRunDuration: "4h", AllowedRunnerPools: []string{"default", "dmz"}}
	if after.Owner != "group:finance-leads" || !same(after.Quotas, want) {
		t.Errorf("the namespace reads as %+v", after)
	}
	// The one a namespace always has is kept, and the others bound nothing again.
	if !same(kept.Quotas, Quotas{MaxConcurrentTasks: 7}) {
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
		// A v0.2 hash is imported where none is kept, and never over one kept, which a token
		// set replaces as before.
		if imported, err := w.ImportBootstrapToken(ctx, valueHash("agk_op_v02")); err != nil || !imported {
			t.Errorf("the v0.2 hash was imported into no hash as %v, %v", imported, err)
		}
		if imported, err := w.ImportBootstrapToken(ctx, valueHash("agk_op_v02_again")); err != nil || imported {
			t.Errorf("a v0.2 hash was imported over the one kept as %v, %v", imported, err)
		}
		if kept, err := w.Bootstrap(ctx); err != nil || !bytes.Equal(kept.TokenHash, valueHash("agk_op_v02")) {
			t.Errorf("the imported hash reads as %x, %v", kept.TokenHash, err)
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
		if imported, err := w.ImportBootstrapToken(ctx, valueHash("agk_op_v02")); err != nil || imported {
			t.Errorf("a v0.2 hash imported after the token ended was answered %v, %v", imported, err)
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
