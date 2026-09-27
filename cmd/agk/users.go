package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/agentiik/agentiik/api"
)

// agk user and agk group: an installation's users and groups, as an administrator manages them
// through /api/v1/users and /api/v1/groups.
//
// "The first administrator is created this way with the bootstrap token, and run again for a fresh
// link until they have enrolled." agk user create prints the enrolment link the installation
// answers, which is the one thing the person running it has to pass on, and running it again for
// a user who has not enrolled prints a fresh one: the installation revokes the link before it.
//
// Each verb says what it did in one line, or prints what it read, and -o json writes the
// installation's answer as it gave it, for a script. A refusal is the installation's own sentence,
// exit 1; an installation that did not answer is exit 4.

// apiTokenPrefix is how an API token is written, agktoken_ and its secret. The bootstrap token is
// the only credential an installation takes that is not written so, which is how agk tells that
// the administrator it just created is the one that ends it.
const apiTokenPrefix = "agktoken_"

// userCreate is agk user create LOGIN [--admin] [--display-name NAME].
func userCreate(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk user create", "agk user create <login> [--admin] [--display-name <name>] [--server <url>]")
	admin := fs.Bool("admin", false, "Makes the user an administrator. The first administrator is created this way with the bootstrap token.")
	display := fs.String("display-name", "", "The name people read in the console and in the sharing panel. Defaults to the login.")
	server := fs.String("server", "", "The installation. Defaults to "+serverVariable+".")
	at, named, code, ok := administering(e, fs, args, server, 1, "agk user create names one login, the name the user signs in as")
	if !ok {
		return code
	}
	login := named[0]
	name := *display
	if name == "" {
		name = login
	}

	var made api.CreatedUser
	status, err := at.send(ctx, http.MethodPost, "/api/v1/users", api.NewUser{Login: login, DisplayName: name, Admin: *admin}, &made, http.StatusCreated, http.StatusOK)
	if err != nil {
		return administrationRefused(e, err, "user", login)
	}
	who := "a user"
	if made.User.Admin {
		who = "an administrator"
	}
	before := made.Enrolment.ExpiresAt.UTC().Format("15:04 UTC")
	if status == http.StatusOK {
		fmt.Fprintf(e.Out, "%s is %s who has not enrolled yet, and the link issued before no longer works. Open this one once, before %s, to enrol a passkey:\n", login, who, before)
	} else {
		fmt.Fprintf(e.Out, "%s is %s with no credential yet. Open this link once, before %s, to enrol a passkey:\n", login, who, before)
	}
	fmt.Fprintln(e.Out, made.Enrolment.Link)
	if made.User.Admin && !strings.HasPrefix(at.token, apiTokenPrefix) {
		fmt.Fprintf(e.Out, "the bootstrap token works until %s has enrolled; then sign in as %s with agk login\n", login, login)
	}
	return exitSucceeded
}

// userList is agk user list: every user, by login.
func userList(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk user list", "agk user list [--server <url>] [-o json]")
	server := fs.String("server", "", "The installation. Defaults to "+serverVariable+".")
	output := jsonFlag(fs)
	at, _, code, ok := administering(e, fs, args, server, 0, "agk user list names no user: it lists them all")
	if !ok {
		return code
	}
	if ok, code := oneFormat(e, *output); !ok {
		return code
	}
	var raw json.RawMessage
	if _, err := at.send(ctx, http.MethodGet, "/api/v1/users", nil, &raw, http.StatusOK); err != nil {
		return administrationRefused(e, err, "", "")
	}
	if *output == "json" {
		return indented(e, raw)
	}
	var listing struct {
		Users []api.User `json:"users"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		fmt.Fprintf(e.Err, "the installation's list of users could not be read: %s\n", err)
		return exitNoOutcome
	}
	if len(listing.Users) == 0 {
		fmt.Fprintln(e.Out, "no user yet")
		return exitSucceeded
	}
	logins, names := 0, 0
	for _, u := range listing.Users {
		logins, names = max(logins, len(u.Login)), max(names, len(u.DisplayName))
	}
	for _, u := range listing.Users {
		line := fmt.Sprintf("%-*s  %-*s  %s", logins, u.Login, names, u.DisplayName, standingOf(u))
		fmt.Fprintln(e.Out, strings.TrimRight(line, " "))
	}
	return exitSucceeded
}

// userShow is agk user show LOGIN: one user, and never a credential.
func userShow(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk user show", "agk user show <login> [--server <url>] [-o json]")
	server := fs.String("server", "", "The installation. Defaults to "+serverVariable+".")
	output := jsonFlag(fs)
	at, named, code, ok := administering(e, fs, args, server, 1, "agk user show names one login")
	if !ok {
		return code
	}
	if ok, code := oneFormat(e, *output); !ok {
		return code
	}
	login := named[0]
	var raw json.RawMessage
	if _, err := at.send(ctx, http.MethodGet, "/api/v1/users/"+url.PathEscape(login), nil, &raw, http.StatusOK); err != nil {
		return administrationRefused(e, err, "user", login)
	}
	if *output == "json" {
		return indented(e, raw)
	}
	var u api.User
	if err := json.Unmarshal(raw, &u); err != nil {
		fmt.Fprintf(e.Err, "the installation's answer about %s could not be read: %s\n", login, err)
		return exitNoOutcome
	}
	kind := "a user"
	if u.Admin {
		kind = "an administrator"
	}
	if u.Suspended {
		kind += ", suspended"
	}
	fmt.Fprintf(e.Out, "%s (%s): %s\n", u.Login, u.DisplayName, kind)
	signedIn := "never signed in"
	if !u.LastSignInAt.IsZero() {
		signedIn = "last signed in at " + u.LastSignInAt.UTC().Format(time.RFC3339)
	}
	fmt.Fprintf(e.Out, "created at %s, %s\n", u.CreatedAt.UTC().Format(time.RFC3339), signedIn)
	return exitSucceeded
}

// userDelete is agk user delete LOGIN: the user and everything they hold.
func userDelete(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk user delete", "agk user delete <login> [--server <url>]")
	server := fs.String("server", "", "The installation. Defaults to "+serverVariable+".")
	at, named, code, ok := administering(e, fs, args, server, 1, "agk user delete names one login")
	if !ok {
		return code
	}
	login := named[0]
	if _, err := at.send(ctx, http.MethodDelete, "/api/v1/users/"+url.PathEscape(login), nil, nil, http.StatusNoContent); err != nil {
		return administrationRefused(e, err, "user", login)
	}
	fmt.Fprintf(e.Out, "removed user %s, with their credentials, tokens, sessions, memberships and grants\n", login)
	return exitSucceeded
}

// groupCreate is agk group create NAME [LOGIN...]: a group, empty or with its first members.
func groupCreate(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk group create", "agk group create <group> [<login>...] [--server <url>]")
	server := fs.String("server", "", "The installation. Defaults to "+serverVariable+".")
	at, named, code, ok := administering(e, fs, args, server, -1, "agk group create names the group, and then its first members by login, if any")
	if !ok {
		return code
	}
	var made api.Group
	if _, err := at.send(ctx, http.MethodPost, "/api/v1/groups", api.NewGroup{Name: named[0], Members: named[1:]}, &made, http.StatusCreated); err != nil {
		return administrationRefused(e, err, "group", named[0])
	}
	fmt.Fprintf(e.Out, "created group %s, %s\n", made.Name, holding(made.Members))
	return exitSucceeded
}

// groupList is agk group list: every group, with its members.
func groupList(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk group list", "agk group list [--server <url>] [-o json]")
	server := fs.String("server", "", "The installation. Defaults to "+serverVariable+".")
	output := jsonFlag(fs)
	at, _, code, ok := administering(e, fs, args, server, 0, "agk group list names no group: it lists them all")
	if !ok {
		return code
	}
	if ok, code := oneFormat(e, *output); !ok {
		return code
	}
	var raw json.RawMessage
	if _, err := at.send(ctx, http.MethodGet, "/api/v1/groups", nil, &raw, http.StatusOK); err != nil {
		return administrationRefused(e, err, "", "")
	}
	if *output == "json" {
		return indented(e, raw)
	}
	var listing struct {
		Groups []api.Group `json:"groups"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		fmt.Fprintf(e.Err, "the installation's list of groups could not be read: %s\n", err)
		return exitNoOutcome
	}
	if len(listing.Groups) == 0 {
		fmt.Fprintln(e.Out, "no group yet")
		return exitSucceeded
	}
	width := 0
	for _, g := range listing.Groups {
		width = max(width, len(g.Name))
	}
	for _, g := range listing.Groups {
		fmt.Fprintf(e.Out, "%-*s  %s\n", width, g.Name, members(g.Members))
	}
	return exitSucceeded
}

// groupShow is agk group show NAME: one group and its members.
func groupShow(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk group show", "agk group show <group> [--server <url>] [-o json]")
	server := fs.String("server", "", "The installation. Defaults to "+serverVariable+".")
	output := jsonFlag(fs)
	at, named, code, ok := administering(e, fs, args, server, 1, "agk group show names one group")
	if !ok {
		return code
	}
	if ok, code := oneFormat(e, *output); !ok {
		return code
	}
	var raw json.RawMessage
	if _, err := at.send(ctx, http.MethodGet, "/api/v1/groups/"+url.PathEscape(named[0]), nil, &raw, http.StatusOK); err != nil {
		return administrationRefused(e, err, "group", named[0])
	}
	if *output == "json" {
		return indented(e, raw)
	}
	var g api.Group
	if err := json.Unmarshal(raw, &g); err != nil {
		fmt.Fprintf(e.Err, "the installation's answer about group %s could not be read: %s\n", named[0], err)
		return exitNoOutcome
	}
	fmt.Fprintf(e.Out, "group:%s: %s\n", g.Name, members(g.Members))
	return exitSucceeded
}

// groupDelete is agk group delete NAME: the group, its memberships and its grants; the users stay.
func groupDelete(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk group delete", "agk group delete <group> [--server <url>]")
	server := fs.String("server", "", "The installation. Defaults to "+serverVariable+".")
	at, named, code, ok := administering(e, fs, args, server, 1, "agk group delete names one group")
	if !ok {
		return code
	}
	if _, err := at.send(ctx, http.MethodDelete, "/api/v1/groups/"+url.PathEscape(named[0]), nil, nil, http.StatusNoContent); err != nil {
		return administrationRefused(e, err, "group", named[0])
	}
	fmt.Fprintf(e.Out, "removed group %s, with its memberships and grants; its members stay\n", named[0])
	return exitSucceeded
}

// groupAdd is agk group add GROUP LOGIN: one member put in, touching no grant.
func groupAdd(ctx context.Context, e Env, args []string) int {
	return membership(ctx, e, args, true)
}

// groupRemove is agk group remove GROUP LOGIN: one member taken out, touching no grant.
func groupRemove(ctx context.Context, e Env, args []string) int {
	return membership(ctx, e, args, false)
}

// membership puts one user in a group or takes them out, and says what the group now holds.
func membership(ctx context.Context, e Env, args []string, in bool) int {
	verb, method, done := "add", http.MethodPut, "is in"
	if !in {
		verb, method, done = "remove", http.MethodDelete, "is not in"
	}
	fs := flags(e, "agk group "+verb, "agk group "+verb+" <group> <login> [--server <url>]")
	server := fs.String("server", "", "The installation. Defaults to "+serverVariable+".")
	at, named, code, ok := administering(e, fs, args, server, 2, "agk group "+verb+" names the group, then the member by login")
	if !ok {
		return code
	}
	group, login := named[0], named[1]
	var now api.Group
	path := "/api/v1/groups/" + url.PathEscape(group) + "/members/" + url.PathEscape(login)
	if _, err := at.send(ctx, method, path, nil, &now, http.StatusOK); err != nil {
		return administrationRefused(e, err, "member", group+"/"+login)
	}
	fmt.Fprintf(e.Out, "%s %s %s, which now %s\n", login, done, group, holds(now.Members))
	return exitSucceeded
}

// administering reads a verb's arguments, want of them where want is not negative and at least one
// where it is, and which installation it talks to. It answers false once it has said what is wrong,
// with the code to leave with.
func administering(e Env, fs *flag.FlagSet, args []string, server *string, want int, usage string) (remote, []string, int, bool) {
	named, code, ok := positional(fs, args)
	if !ok {
		return remote{}, nil, code, false
	}
	if (want >= 0 && len(named) != want) || (want < 0 && len(named) == 0) {
		fmt.Fprintln(e.Err, usage)
		return remote{}, nil, exitUsage, false
	}
	at, ok := reach(e, *server)
	if !ok {
		return remote{}, nil, exitUsage, false
	}
	return at, named, exitSucceeded, true
}

// jsonFlag is -o, which writes the installation's answer as it gave it.
func jsonFlag(fs *flag.FlagSet) *string {
	return fs.String("o", "", "json writes the installation's answer as it gave it.")
}

// oneFormat refuses an -o that is not json.
func oneFormat(e Env, output string) (bool, int) {
	if output != "" && output != "json" {
		fmt.Fprintf(e.Err, "-o is %q: json is the one format there is\n", output)
		return false, exitUsage
	}
	return true, exitSucceeded
}

// indented writes an answer as the installation gave it, indented.
func indented(e Env, raw json.RawMessage) int {
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		fmt.Fprintf(e.Err, "the installation's answer is not JSON: %s\n", err)
		return exitNoOutcome
	}
	fmt.Fprintln(e.Out, out.String())
	return exitSucceeded
}

// send sends one request, with body as JSON where it is not nil, and decodes an answer of one of
// the statuses wanted into out, where out is not nil. It answers the status. Any other status is a
// *refused, and no answer at all is errUnreachable.
func (r remote) send(ctx context.Context, method, path string, body, out any, want ...int) (int, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := r.request(ctx, method, path, reader)
	if err != nil {
		return 0, err
	}
	answer, err := client(answerTimeout).Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w at %s: %v", errUnreachable, r.base, err)
	}
	defer answer.Body.Close()
	wanted := false
	for _, w := range want {
		wanted = wanted || answer.StatusCode == w
	}
	if !wanted {
		return answer.StatusCode, refusedBy(answer)
	}
	if out != nil {
		if err := json.NewDecoder(answer.Body).Decode(out); err != nil {
			return answer.StatusCode, fmt.Errorf("%w: its answer to %s %s could not be read: %v", errUnreachable, method, path, err)
		}
	}
	return answer.StatusCode, nil
}

// administrationRefused says why a request to administer users or groups came to nothing, and
// answers the code to leave with: exit 1 where the installation said no, in its own sentence, save
// where a sentence of the command line's says more, and exit 4 where it did not answer, or answered
// that it could not answer now. A gateway's 502 in front of an API that had already acted is no
// refusal, and a script told exit 1, "refused, and nothing ran", would believe a user removed was
// still there.
func administrationRefused(e Env, err error, what, name string) int {
	if errors.Is(err, errUnreachable) {
		fmt.Fprintf(e.Err, "%s\n", err)
		return exitNoOutcome
	}
	if passing(err) {
		fmt.Fprintf(e.Err, "the installation answered %d, %s, and whether it did what was asked is not known: read it back, or ask again\n", statusOf(err), err)
		return exitNoOutcome
	}
	said := err.Error()
	switch statusOf(err) {
	case http.StatusForbidden:
		said += fmt.Sprintf(": users and groups are an administrator's to manage, and the token in %s is not an administrator's with no scope, nor the bootstrap token before the first administrator has enrolled", tokenVariable)
	case http.StatusNotFound:
		switch what {
		case "user":
			said = "no user " + name
		case "group":
			said = "no group " + name
		case "member":
			// The installation says which of the two it did not find.
			group, login, _ := strings.Cut(name, "/")
			said = "no user " + login
			if strings.Contains(err.Error(), "group") {
				said = "no group " + group
			}
		}
	}
	fmt.Fprintf(e.Err, "%s\n", said)
	return exitRefused
}

// standingOf is what a user is besides their names: an administrator, suspended, or neither.
func standingOf(u api.User) string {
	var standing []string
	if u.Admin {
		standing = append(standing, "administrator")
	}
	if u.Suspended {
		standing = append(standing, "suspended")
	}
	return strings.Join(standing, ", ")
}

// members lists a group's members, or says it has none.
func members(logins []string) string {
	if len(logins) == 0 {
		return "no member"
	}
	return strings.Join(logins, ", ")
}

// holding is what a group was created with.
func holding(logins []string) string {
	if len(logins) == 0 {
		return "with no member yet"
	}
	return "with " + strings.Join(logins, ", ")
}

// holds is what a group holds now.
func holds(logins []string) string {
	if len(logins) == 0 {
		return "holds no member"
	}
	return "holds " + strings.Join(logins, ", ")
}
