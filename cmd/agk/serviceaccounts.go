package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/agentiik/agentiik/api"
)

// agk service-account: the service accounts of the namespaces whoever runs it owns, through
// /api/v1/service-accounts.
//
// A service account is named as it is written wherever a principal is, NS/NAME, so that the name
// agk service-account create takes is the one agk token create --for takes next. What the
// installation answers is printed one line a service account, and -o json writes the answer as it
// gave it. A refusal is the installation's own sentence, exit 1; an installation that did not answer
// is exit 4, and so is a change answered with a 5xx, since whether it was made cannot be told from
// that answer.

// serviceAccountCreate is agk service-account create NS/NAME.
func serviceAccountCreate(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk service-account create", "agk service-account create <ns>/<name> [--server <url>] [-o json]")
	server := fs.String("server", "", "The installation. Defaults to "+serverVariable+".")
	output := jsonFlag(fs)
	namespace, name, code, ok := oneServiceAccount(e, fs, args, "agk service-account create")
	if !ok {
		return code
	}
	if ok, code := oneFormat(e, *output); !ok {
		return code
	}
	at, ok := reach(e, *server)
	if !ok {
		return exitUsage
	}
	var raw json.RawMessage
	if err := at.sendJSON(ctx, http.MethodPost, "/api/v1/service-accounts", api.NewServiceAccount{Namespace: namespace, Name: name}, http.StatusCreated, &raw); err != nil {
		return serviceAccountRefused(e, namespace+"/"+name, true, "", err)
	}
	if *output == "json" {
		return indented(e, raw)
	}
	fmt.Fprintf(e.Out, "created service account %s/%s, which holds no grant until one is given it, and no token until agk token create --for %s/%s mints one\n", namespace, name, namespace, name)
	return exitSucceeded
}

// serviceAccountList is agk service-account list [NS]: the service accounts of the namespaces the
// caller owns, or of the one named, the built-in identity of each among them.
func serviceAccountList(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk service-account list", "agk service-account list [<ns>] [--server <url>] [-o json]")
	server := fs.String("server", "", "The installation. Defaults to "+serverVariable+".")
	output := jsonFlag(fs)
	named, code, ok := positional(fs, args)
	if !ok {
		return code
	}
	if len(named) > 1 {
		fmt.Fprintln(e.Err, "agk service-account list names one namespace at most, and lists the service accounts of every namespace you own without one")
		return exitUsage
	}
	if ok, code := oneFormat(e, *output); !ok {
		return code
	}
	at, ok := reach(e, *server)
	if !ok {
		return exitUsage
	}
	var listed api.ServiceAccountList
	if err := at.getJSON(ctx, "/api/v1/service-accounts", &listed); err != nil {
		return serviceAccountRefused(e, "", false, "", err)
	}
	// The route lists every namespace the caller owns, and one of them is chosen here.
	if len(named) == 1 {
		within := listed.ServiceAccounts[:0]
		for _, sa := range listed.ServiceAccounts {
			if sa.Namespace == named[0] {
				within = append(within, sa)
			}
		}
		listed.ServiceAccounts = within
	}
	if *output == "json" {
		raw, err := json.Marshal(listed)
		if err != nil {
			fmt.Fprintf(e.Err, "the list of service accounts could not be written: %s\n", err)
			return exitNoOutcome
		}
		return indented(e, raw)
	}
	if len(listed.ServiceAccounts) == 0 {
		switch {
		case len(named) == 1:
			fmt.Fprintf(e.Err, "no service account of namespace %s: it does not exist, or is not one you own\n", named[0])
		default:
			fmt.Fprintln(e.Err, "no service account: you own no namespace, and a service account is listed to whoever owns its namespace")
		}
		return exitSucceeded
	}
	width := 0
	for _, sa := range listed.ServiceAccounts {
		width = max(width, len(sa.Namespace)+1+len(sa.Name))
	}
	for _, sa := range listed.ServiceAccounts {
		fmt.Fprintf(e.Out, "%-*s  %s\n", width, sa.Namespace+"/"+sa.Name, madeBy(sa))
	}
	return exitSucceeded
}

// serviceAccountDelete is agk service-account delete NS/NAME: the service account with its tokens
// and its grants.
func serviceAccountDelete(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk service-account delete", "agk service-account delete <ns>/<name> [--server <url>]")
	server := fs.String("server", "", "The installation. Defaults to "+serverVariable+".")
	namespace, name, code, ok := oneServiceAccount(e, fs, args, "agk service-account delete")
	if !ok {
		return code
	}
	at, ok := reach(e, *server)
	if !ok {
		return exitUsage
	}
	path := "/api/v1/service-accounts/" + url.PathEscape(namespace) + "/" + url.PathEscape(name)
	if err := at.sendJSON(ctx, http.MethodDelete, path, nil, http.StatusNoContent, nil); err != nil {
		return serviceAccountRefused(e, namespace+"/"+name, true, fmt.Sprintf("no service account %s/%s, or not of a namespace you own", namespace, name), err)
	}
	fmt.Fprintf(e.Out, "removed service account %s/%s, with its tokens and its grants\n", namespace, name)
	return exitSucceeded
}

// oneServiceAccount reads the one NS/NAME a verb names, and answers false once it has said what is
// wrong, with the code to leave with.
func oneServiceAccount(e Env, fs *flag.FlagSet, args []string, verb string) (string, string, int, bool) {
	named, code, ok := positional(fs, args)
	if !ok {
		return "", "", code, false
	}
	if len(named) != 1 {
		fmt.Fprintf(e.Err, "%s names one service account, written <ns>/<name>, such as finance/nightly-sync\n", verb)
		return "", "", exitUsage, false
	}
	namespace, name, cut := strings.Cut(named[0], "/")
	if !cut || namespace == "" || name == "" || strings.Contains(name, "/") {
		fmt.Fprintf(e.Err, "%q names no service account: one is written <ns>/<name>, its namespace and its name, such as finance/nightly-sync\n", named[0])
		return "", "", exitUsage, false
	}
	return namespace, name, exitSucceeded, true
}

// madeBy is who created a service account and when, or that it is its namespace's built-in identity.
func madeBy(sa api.ServiceAccount) string {
	if sa.CreatedBy == "" {
		return "built-in: the runs nobody started are attributed to it"
	}
	return fmt.Sprintf("created by %s at %s", sa.CreatedBy, sa.CreatedAt.UTC().Format(time.RFC3339))
}

// serviceAccountRefused says why a request about service accounts came to nothing, and answers the
// code to leave with: exit 1 where the installation said no, in its own sentence, and exit 4 where
// it did not answer, or answered a change with a 5xx, since the change may have been made before the
// answer failed, and a script told exit 1, "refused, and nothing ran", would believe it had not. A
// read answered with a 5xx changed nothing, and is exit 1.
//
// absent is what a 404 means where the path names a service account, and empty where it names
// none, where a 404 is an installation serving no such route.
func serviceAccountRefused(e Env, who string, change bool, absent string, err error) int {
	said := err.Error()
	switch status := statusOf(err); {
	case errors.Is(err, errUnreachable):
		fmt.Fprintln(e.Err, said)
		return exitNoOutcome
	case status == http.StatusUnauthorized:
		said = fmt.Sprintf("the installation did not accept the credential in %s: %s", tokenVariable, said)
	case status == http.StatusNotFound && absent != "":
		said = absent
	case status == http.StatusNotFound:
		said = fmt.Sprintf("the installation serves no route for service accounts: it predates v0.3.0, which brought them, or it is not an Agentiik API (%s)", said)
	case change && status >= 500:
		fmt.Fprintf(e.Err, "the installation answered %d, %s, and whether service account %s was changed cannot be told from it: agk service-account list reads it back\n", status, said, who)
		return exitNoOutcome
	}
	fmt.Fprintln(e.Err, said)
	return exitRefused
}
