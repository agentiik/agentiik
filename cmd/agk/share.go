package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/api"
)

// agk share, agk grants and agk whoami: who may do what on an installation, through
// /api/v1/{ns}/grants, /api/v1/{ns}/workflows/{name}/grants and /api/v1/me.
//
// Each names the namespace or the workflow it is about as its one word, written as a grant's scope
// is, finance or finance/monthly-invoicing, and reaches the installation as every other verb does.
// What the installation answers is printed in its own words, a role, a permission and a principal
// each as the wire writes them, and -o json writes the answer itself. A refusal is the
// installation's own sentence, exit 1; an installation that did not answer is exit 4.

// shareVerb is agk share NS[/WORKFLOW] (--user L | --group G | --service-account NS/N) (--role R |
// --deny PERMISSION) [--expires D], or agk share NS[/WORKFLOW] --revoke ID.
func shareVerb(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk share", "agk share <ns>[/<workflow>] (--user <login> | --group <group> | --service-account <ns>/<name>) (--role <role> | --deny <permission>) [--expires <30d|instant>] [--server <url>] [-o json]\n\tagk share <ns>[/<workflow>] --revoke <id> [--server <url>]")
	user := fs.String("user", "", "The user shared with, by login.")
	group := fs.String("group", "", "The group shared with, by name, group:NAME or NAME.")
	account := fs.String("service-account", "", "The service account shared with, written NS/NAME.")
	role := fs.String("role", "", "The role granted: viewer, operator, editor or owner.")
	deny := fs.String("deny", "", "The one permission taken away, such as run:read_data, instead of a role.")
	expires := fs.String("expires", "", "How long it lasts, in days such as 30d or as a duration such as 12h, or the instant it ends, in RFC 3339. Defaults to until it is revoked.")
	revoke := fs.String("revoke", "", "A grant to revoke instead, by the identifier agk grants prints.")
	server := fs.String("server", "", "The installation. "+serverDefault)
	output := fs.String("o", "", "json writes the installation's answer as it gave it.")
	at, code, ok := oneScope(e, fs, args)
	if !ok {
		return code
	}
	if ok, code := oneFormat(e, *output); !ok {
		return code
	}
	var principals []string
	for _, p := range []string{*user, groupRef(*group), *account} {
		if p != "" {
			principals = append(principals, p)
		}
	}
	if *revoke != "" {
		if len(principals) > 0 || *role != "" || *deny != "" || *expires != "" {
			fmt.Fprintln(e.Err, "--revoke names a grant by its identifier, and nothing else: who it was for and what it gave are the grant's")
			return exitUsage
		}
		return revokeGrant(ctx, e, at, *revoke, *server)
	}
	switch {
	case len(principals) != 1:
		fmt.Fprintln(e.Err, "agk share names one principal: --user, --group or --service-account")
		return exitUsage
	case *account != "" && !strings.Contains(*account, "/"):
		fmt.Fprintf(e.Err, "--service-account is %q: a service account is written NS/NAME, such as finance/nightly-sync\n", *account)
		return exitUsage
	case (*role == "") == (*deny == ""):
		fmt.Fprintln(e.Err, "agk share grants a role with --role or takes one permission away with --deny, one of the two")
		return exitUsage
	}
	q := api.GrantRequest{Principal: principals[0], Role: *role, Deny: *deny}
	if *expires != "" {
		ends, err := expiryOf(*expires, e.now(), "a grant ends after it is written")
		if err != nil {
			fmt.Fprintf(e.Err, "--expires: %s\n", err)
			return exitUsage
		}
		q.ExpiresAt = &ends
	}
	installation, ok := reach(e, *server)
	if !ok {
		return exitUsage
	}
	var raw json.RawMessage
	if _, err := installation.send(ctx, http.MethodPost, grantsPath(at), q, &raw, http.StatusCreated); err != nil {
		return sharingRefused(e, err, at, "", true)
	}
	if *output == "json" {
		return indented(e, raw)
	}
	var g access.Grant
	if err := json.Unmarshal(raw, &g); err != nil {
		fmt.Fprintf(e.Err, "the installation's answer about the grant could not be read: %s\n", err)
		return exitNoOutcome
	}
	did := "granted " + string(g.Role) + " on " + g.Scope.String() + " to " + g.Principal
	if g.Deny != "" {
		did = "denied " + string(g.Deny) + " on " + g.Scope.String() + " to " + g.Principal
	}
	if g.ExpiresAt != nil {
		did += " until " + g.ExpiresAt.UTC().Format(time.RFC3339)
	}
	fmt.Fprintf(e.Out, "%s: grant %s\n", did, g.ID)
	return exitSucceeded
}

// revokeGrant is agk share SCOPE --revoke ID: one grant written at that scope, from the next
// request and the next run creation.
func revokeGrant(ctx context.Context, e Env, at access.Scope, id, server string) int {
	installation, ok := reach(e, server)
	if !ok {
		return exitUsage
	}
	if _, err := installation.send(ctx, http.MethodDelete, grantsPath(at)+"/"+url.PathEscape(id), nil, nil, http.StatusNoContent); err != nil {
		return sharingRefused(e, err, at, id, true)
	}
	fmt.Fprintf(e.Out, "revoked grant %s on %s: it gives nothing from the next request and the next run\n", id, at)
	return exitSucceeded
}

// grantsVerb is agk grants NS[/WORKFLOW]: who can do what there, and from which scope, one grant or
// deny a line.
func grantsVerb(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk grants", "agk grants <ns>[/<workflow>] [--server <url>] [-o json]")
	server := fs.String("server", "", "The installation. "+serverDefault)
	output := fs.String("o", "", "json writes the installation's answer as it gave it.")
	at, code, ok := oneScope(e, fs, args)
	if !ok {
		return code
	}
	if ok, code := oneFormat(e, *output); !ok {
		return code
	}
	installation, ok := reach(e, *server)
	if !ok {
		return exitUsage
	}
	var raw json.RawMessage
	if _, err := installation.send(ctx, http.MethodGet, grantsPath(at), nil, &raw, http.StatusOK); err != nil {
		return sharingRefused(e, err, at, "", false)
	}
	if *output == "json" {
		return indented(e, raw)
	}
	var listed api.GrantList
	if err := json.Unmarshal(raw, &listed); err != nil {
		fmt.Fprintf(e.Err, "the installation's list of grants could not be read: %s\n", err)
		return exitNoOutcome
	}
	if len(listed.Grants) == 0 {
		fmt.Fprintf(e.Err, "no grant on %s\n", at)
		return exitSucceeded
	}
	describeGrants(e.Out, listed.Grants)
	return exitSucceeded
}

// describeGrants writes one grant a line: its identifier, whom it is for, what it gives or takes
// away, the scope it was written at, which for a workflow's list tells what it inherits from what
// is written on it, and the permissions it gives there, which on a workflow never hold the
// secrets'.
func describeGrants(w io.Writer, grants []access.Grant) {
	ids, principals, gives, scopes := 0, 0, 0, 0
	for _, g := range grants {
		ids, principals = max(ids, len(g.ID)), max(principals, len(g.Principal))
		gives, scopes = max(gives, len(what(g))), max(scopes, len(where(g)))
	}
	for _, g := range grants {
		line := fmt.Sprintf("%-*s  %-*s  %-*s  %-*s", ids, g.ID, principals, g.Principal, gives, what(g), scopes, where(g))
		if g.Role != "" {
			held, err := access.Resolve(access.Principal{Ref: g.Principal}, []access.Grant{g}, g.Scope, g.GrantedAt)
			if err == nil {
				line += "  " + held.String()
			}
		}
		if g.ExpiresAt != nil {
			line += "  until " + g.ExpiresAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
}

// what a grant gives: its role, or the permission it denies.
func what(g access.Grant) string {
	if g.Deny != "" {
		return "deny " + string(g.Deny)
	}
	return string(g.Role)
}

// where a grant was written: on a namespace, which every workflow of it inherits, or on one
// workflow.
func where(g access.Grant) string {
	if g.Scope.Workflow == "" {
		return "namespace " + g.Scope.String()
	}
	return "workflow " + g.Scope.String()
}

// whoami is agk whoami [NS[/WORKFLOW]]: the caller, its groups and what it holds, everywhere or on
// the one namespace or workflow named, and with none named, what the installation tells it, since
// that is not about any one of them.
func whoami(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk whoami", "agk whoami [<ns>[/<workflow>]] [--server <url>] [-o json]")
	server := fs.String("server", "", "The installation. "+serverDefault)
	output := fs.String("o", "", "json writes the installation's answer as it gave it.")
	named, code, ok := positional(fs, args)
	if !ok {
		return code
	}
	var at access.Scope
	switch len(named) {
	case 0:
	case 1:
		var err error
		if at, err = access.ParseScope(named[0]); err != nil {
			fmt.Fprintln(e.Err, err)
			return exitUsage
		}
	default:
		fmt.Fprintln(e.Err, "agk whoami names one namespace or workflow at most, as finance or finance/monthly-invoicing")
		return exitUsage
	}
	if ok, code := oneFormat(e, *output); !ok {
		return code
	}
	installation, ok := reach(e, *server)
	if !ok {
		return exitUsage
	}
	var raw json.RawMessage
	if _, err := installation.send(ctx, http.MethodGet, "/api/v1/me", nil, &raw, http.StatusOK); err != nil {
		return sharingRefused(e, err, access.Scope{}, "", false)
	}
	if *output == "json" {
		return indented(e, raw)
	}
	var me api.Me
	if err := json.Unmarshal(raw, &me); err != nil {
		fmt.Fprintf(e.Err, "the installation's answer about who you are could not be read: %s\n", err)
		return exitNoOutcome
	}
	who := me.Principal
	if me.Admin {
		who += ", an administrator"
	}
	if len(me.Groups) > 0 {
		who += ", in " + strings.Join(me.Groups, ", ")
	}
	fmt.Fprintln(e.Out, who)
	if at.Namespace != "" {
		// What applies to a workflow is its own key where a grant, a deny or the credential makes
		// it other than its namespace's. With none, it is its namespace's, or nothing where denies
		// on it take all of that: the installation leaves out a workflow the caller cannot read,
		// rather than name it to them, so which of the two is not agk's to say.
		held, written := me.Permissions[at.String()]
		if !written && at.Workflow != "" {
			if inherited := me.Permissions[at.Namespace]; len(inherited) > 0 {
				fmt.Fprintf(e.Out, "on %s: %s, from %s, unless denies there take all of it\n", at, permissionsLine(inherited), at.Namespace)
				return exitSucceeded
			}
		}
		fmt.Fprintf(e.Out, "on %s: %s\n", at, permissionsLine(held))
		return exitSucceeded
	}
	if len(me.Permissions) == 0 {
		fmt.Fprintln(e.Out, "holds nothing in any namespace")
	}
	scopes := make([]string, 0, len(me.Permissions))
	for scope := range me.Permissions {
		scopes = append(scopes, scope)
	}
	slices.Sort(scopes)
	for _, scope := range scopes {
		fmt.Fprintf(e.Out, "on %s: %s\n", scope, permissionsLine(me.Permissions[scope]))
	}
	for _, n := range me.Notifications {
		fmt.Fprintln(e.Out, noticeLine(n))
	}
	return exitSucceeded
}

// permissionsLine is what is held at one scope, as the page writes permissions.
func permissionsLine(held []api.Permission) string {
	if len(held) == 0 {
		return "nothing"
	}
	names := make([]string, len(held))
	for i, p := range held {
		names[i] = string(p)
	}
	return strings.Join(names, ", ")
}

// noticeLine is one notification as whoami prints it, by the identifier that dismisses it.
func noticeLine(n api.Notification) string {
	when := n.At.UTC().Format(time.RFC3339)
	switch {
	case n.Kind == "admin_access_widened" && n.Grant != nil:
		if said := widenedBy(n, *n.Grant); said != "" {
			return fmt.Sprintf("told %s at %s: %s, an administrator, %s", n.ID, when, n.By, said)
		}
	case n.Kind == "passkey_counter_refused":
		return fmt.Sprintf("told %s at %s: a sign-in with your passkey %s was refused because its signature counter did not move forward, as a copy of it would; remove it if the other copy is not yours", n.ID, when, n.Credential)
	case n.Kind == "break_glass_recovery":
		// Told to every administrator, the one recovered among them: whoever did not run it learns
		// that whoever holds the host did.
		return fmt.Sprintf("told %s at %s: agentiik-api recover, run on the installation's host, issued %s, an administrator, a recovery code", n.ID, when, n.Login)
	case n.Kind == "recovery_code_issued" && n.By != "":
		// Told to the user alone, who is the one who knows whether they asked for it.
		return fmt.Sprintf("told %s at %s: %s issued you a recovery code, which enrols a passkey or sets a password on your account for whoever holds it, within the hour; if you did not ask for one, tell another administrator", n.ID, when, n.By)
	case n.Kind == "recovery_code_used" && n.By != "" && n.Credential != "":
		return fmt.Sprintf("told %s at %s: a recovery code %s issued you enrolled %s on your account; if that was not you, remove it from your sign-in methods and tell another administrator", n.ID, when, n.By, n.Credential)
	}
	return fmt.Sprintf("told %s at %s: %s", n.ID, when, n.Kind)
}

// widenedBy says what an administrator did that widened access, by the act the notification names,
// and nothing where it names an act this agk does not know.
func widenedBy(n api.Notification, g access.Grant) string {
	gives := string(g.Role) + " on " + g.Scope.String()
	if g.Deny != "" {
		gives = "a deny of " + string(g.Deny) + " on " + g.Scope.String()
	}
	until := ""
	if g.ExpiresAt != nil {
		until = " until " + g.ExpiresAt.UTC().Format(time.RFC3339)
	}
	switch n.Act {
	case "granted":
		return "granted " + gives + " to " + g.Principal + until
	case "deny_lifted":
		return "lifted the deny of " + string(g.Deny) + " on " + g.Scope.String() + " for " + g.Principal + ", widening their own access"
	case "joined_group":
		who := n.Login
		if who == n.By {
			who = "themselves"
		}
		return "put " + who + " in " + g.Principal + ", which holds " + gives + until
	case "left_group":
		return "took themselves out of " + g.Principal + ", which holds " + gives + until + ", widening their own access"
	case "group_removed":
		return "removed " + g.Principal + ", which they were in and which held " + gives + until + ", widening their own access"
	}
	return ""
}

// oneScope reads the flags and the one namespace or workflow a verb is about, written as a grant's
// scope is, and says whether it may go on.
func oneScope(e Env, fs *flag.FlagSet, args []string) (access.Scope, int, bool) {
	named, code, ok := positional(fs, args)
	if !ok {
		return access.Scope{}, code, false
	}
	if len(named) != 1 {
		fmt.Fprintf(e.Err, "%s names one namespace or workflow, as finance or finance/monthly-invoicing\n", fs.Name())
		return access.Scope{}, exitUsage, false
	}
	at, err := access.ParseScope(named[0])
	if err != nil {
		fmt.Fprintln(e.Err, err)
		return access.Scope{}, exitUsage, false
	}
	return at, exitSucceeded, true
}

// groupRef is a group as a grant names it, from --group written with or without its group: prefix.
func groupRef(group string) string {
	if group == "" || strings.HasPrefix(group, "group:") {
		return group
	}
	return "group:" + group
}

// grantsPath is where the grants at a scope are.
func grantsPath(at access.Scope) string {
	if at.Workflow == "" {
		return "/api/v1/" + url.PathEscape(at.Namespace) + "/grants"
	}
	return "/api/v1/" + url.PathEscape(at.Namespace) + "/workflows/" + url.PathEscape(at.Workflow) + "/grants"
}

// sharingRefused says why a request about grants, or about the caller, came to nothing, and answers
// the code to leave with: exit 1 where the installation said no, in its own sentence save where the
// command line's says more, and exit 4 where it did not answer or answered that it could not now.
// changes says whether the request asked for a change, which such an answer leaves unknown.
func sharingRefused(e Env, err error, at access.Scope, id string, changes bool) int {
	if errors.Is(err, errUnreachable) {
		fmt.Fprintln(e.Err, err)
		return exitNoOutcome
	}
	if passing(err) && statusOf(err) != http.StatusTooManyRequests {
		if changes {
			fmt.Fprintf(e.Err, "the installation answered %d, %s, and whether it did what was asked is not known: read it back with agk grants, or ask again\n", statusOf(err), err)
		} else {
			fmt.Fprintf(e.Err, "the installation answered %d, %s, and could not answer now: ask again\n", statusOf(err), err)
		}
		return exitNoOutcome
	}
	switch status := statusOf(err); {
	case status == http.StatusUnauthorized:
		fmt.Fprintf(e.Err, "%s: %s\n", credentialRefused(e.presentsKept()), err)
	case status == http.StatusNotFound && id != "":
		fmt.Fprintf(e.Err, "no grant %s written on %s, or not yours to revoke\n", id, at)
	case status == http.StatusNotFound && at.Namespace != "":
		fmt.Fprintf(e.Err, "no %s you may share: it is not there, or you hold no grant:manage on it\n", at)
	default:
		fmt.Fprintln(e.Err, err)
	}
	return exitRefused
}
