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

	"github.com/agentiik/agentiik/api"
)

// agk namespace: an installation's namespaces through /api/v1/namespaces, as an administrator
// creates, bounds and removes them, and as whoever holds a grant in one reads it.
//
// Every verb names the namespace it is about as its one word, as agk status names its run, and
// reaches the installation as every other verb does, through --server or AGENTIIK_SERVER and the
// token in AGENTIIK_TOKEN or the one agk login kept. What the installation answers is printed as the wire writes it, each
// quota under its own name, so that what a person reads is what they would type in a request, and
// -o json writes the answer itself.

// quotaFlags are the six quotas as flags, each named after the quota it sets with its underscores
// written as hyphens, as a flag is spelled.
type quotaFlags struct {
	fs *flag.FlagSet

	concurrentTasks, runsPerHour, retentionDays int
	artifactBytes                               int64
	runDuration, pools                          string
}

func withQuotas(fs *flag.FlagSet) *quotaFlags {
	q := &quotaFlags{fs: fs}
	fs.IntVar(&q.concurrentTasks, "max-concurrent-tasks", 0, "max_concurrent_tasks: the tasks the namespace may hold at once.")
	fs.IntVar(&q.runsPerHour, "max-runs-per-hour", 0, "max_runs_per_hour: the runs it may create in any 60 minutes.")
	fs.Int64Var(&q.artifactBytes, "max-artifact-bytes", 0, "max_artifact_bytes: the bytes of live artifacts it may hold.")
	fs.IntVar(&q.retentionDays, "max-retention-days", 0, "max_retention_days: the most days it may ask to keep what its runs produce.")
	fs.StringVar(&q.runDuration, "max-run-duration", "", "max_run_duration: the longest root timeout a workflow of it may declare, written as a timeout, 24h.")
	fs.StringVar(&q.pools, "allowed-runner-pools", "", "allowed_runner_pools: the runner pools its steps may be sent to, by name, separated by commas.")
	return q
}

// given answers the quotas the command line set, and whether it set any. A count given is held to
// what the wire holds it to here, from 1, because the wire's refusal of 0 would otherwise read as a
// flag nobody passed: a quota is lifted by naming it, never by writing it as zero.
func (q *quotaFlags) given() (api.Quotas, bool, error) {
	var out api.Quotas
	set := false
	var err error
	q.fs.Visit(func(f *flag.Flag) {
		if err != nil {
			return
		}
		count := func(n int64) {
			if n < 1 {
				err = fmt.Errorf("--%s is %d, and it is a whole number from 1: a quota that bounds nothing is left out when a namespace is created, and lifted with --lift afterwards", f.Name, n)
			}
		}
		switch f.Name {
		case "max-concurrent-tasks":
			count(int64(q.concurrentTasks))
			out.MaxConcurrentTasks = q.concurrentTasks
		case "max-runs-per-hour":
			count(int64(q.runsPerHour))
			out.MaxRunsPerHour = q.runsPerHour
		case "max-artifact-bytes":
			count(q.artifactBytes)
			out.MaxArtifactBytes = q.artifactBytes
		case "max-retention-days":
			count(int64(q.retentionDays))
			out.MaxRetentionDays = q.retentionDays
		case "max-run-duration":
			if q.runDuration == "" {
				err = errors.New("--max-run-duration is empty: it is written as a timeout, 24h, and lifted with --lift max_run_duration")
			}
			out.MaxRunDuration = q.runDuration
		case "allowed-runner-pools":
			for _, pool := range strings.Split(q.pools, ",") {
				if pool = strings.TrimSpace(pool); pool != "" {
					out.AllowedRunnerPools = append(out.AllowedRunnerPools, pool)
				}
			}
			if out.AllowedRunnerPools == nil {
				err = errors.New("--allowed-runner-pools names no pool: a namespace allowed every pool that accepts it has no such quota, which --lift allowed_runner_pools lifts")
			}
		default:
			return
		}
		set = true
	})
	return out, set, err
}

// namespaceCreate is agk namespace create: a shared namespace, its owner and its quotas.
func namespaceCreate(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk namespace create", "agk namespace create <name> --owner <user or group:NAME> [--max-... <quota>] [--server <url>] [-o json]")
	owner := fs.String("owner", "", "Who owns it: a login, or group:NAME. Required: the owner holds the owner role on it and is told when an administrator widens their own access in it.")
	quotas := withQuotas(fs)
	server := fs.String("server", "", "The installation. "+serverDefault)
	output := fs.String("o", "", "json writes the installation's answer as it gave it.")
	name, code, ok := oneNamespace(e, fs, args)
	if !ok {
		return code
	}
	if *owner == "" {
		fmt.Fprintln(e.Err, "--owner is required: a namespace is created with an owner, a login or group:NAME, who holds the owner role on it")
		return exitUsage
	}
	q, set, err := quotas.given()
	if err != nil {
		fmt.Fprintln(e.Err, err)
		return exitUsage
	}
	ask := api.NamespaceRecord{Name: name, Owner: *owner}
	if set {
		ask.Quotas = &q
	}
	at, ok := reach(e, *server)
	if !ok {
		return exitUsage
	}
	var raw json.RawMessage
	if err := at.sendJSON(ctx, http.MethodPost, "/api/v1/namespaces", ask, http.StatusCreated, &raw); err != nil {
		return namespaceRefused(e, name, true, err)
	}
	return answered(e, raw, *output, "created namespace ")
}

// namespaceList is agk namespace list: every namespace for an administrator, and the ones the
// caller holds a grant in for anybody else.
func namespaceList(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk namespace list", "agk namespace list [--server <url>] [-o json]")
	server := fs.String("server", "", "The installation. "+serverDefault)
	output := fs.String("o", "", "json writes the installation's answer as it gave it.")
	named, code, ok := positional(fs, args)
	if !ok {
		return code
	}
	if len(named) != 0 {
		fmt.Fprintln(e.Err, "agk namespace list names no namespace: agk namespace show <name> reads one")
		return exitUsage
	}
	if !namespaceFormat(e, *output) {
		return exitUsage
	}
	at, ok := reach(e, *server)
	if !ok {
		return exitUsage
	}
	var raw json.RawMessage
	if err := at.getJSON(ctx, "/api/v1/namespaces", &raw); err != nil {
		return namespaceRefused(e, "", false, err)
	}
	if *output == "json" {
		return indentedAnswer(e, raw)
	}
	var listed api.NamespaceList
	if err := json.Unmarshal(raw, &listed); err != nil {
		fmt.Fprintf(e.Err, "the installation's list of namespaces could not be read: %s\n", err)
		return exitNoOutcome
	}
	if len(listed.Namespaces) == 0 {
		fmt.Fprintln(e.Err, "no namespace: an administrator sees every one, and anybody else those they hold a grant in")
		return exitSucceeded
	}
	width, kinds := 0, 0
	for _, n := range listed.Namespaces {
		width, kinds = max(width, len(n.Name)), max(kinds, len(n.Kind))
	}
	for _, n := range listed.Namespaces {
		fmt.Fprintf(e.Out, "%-*s  %-*s  %s\n", width, n.Name, kinds, n.Kind, ownedBy(n))
	}
	return exitSucceeded
}

// namespaceShow is agk namespace show: one namespace, its kind, its owner and its quotas.
func namespaceShow(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk namespace show", "agk namespace show <name> [--server <url>] [-o json]")
	server := fs.String("server", "", "The installation. "+serverDefault)
	output := fs.String("o", "", "json writes the installation's answer as it gave it.")
	name, code, ok := oneNamespace(e, fs, args)
	if !ok {
		return code
	}
	at, ok := reach(e, *server)
	if !ok {
		return exitUsage
	}
	var raw json.RawMessage
	if err := at.getJSON(ctx, "/api/v1/namespaces/"+url.PathEscape(name), &raw); err != nil {
		return namespaceRefused(e, name, false, err)
	}
	return answered(e, raw, *output, "")
}

// namespaceDelete is agk namespace delete: a namespace that holds nothing but its built-in
// identity, which the installation refuses otherwise, saying what it holds.
func namespaceDelete(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk namespace delete", "agk namespace delete <name> [--server <url>]")
	server := fs.String("server", "", "The installation. "+serverDefault)
	name, code, ok := oneNamespace(e, fs, args)
	if !ok {
		return code
	}
	at, ok := reach(e, *server)
	if !ok {
		return exitUsage
	}
	if err := at.sendJSON(ctx, http.MethodDelete, "/api/v1/namespaces/"+url.PathEscape(name), nil, http.StatusNoContent, nil); err != nil {
		return namespaceRefused(e, name, true, err)
	}
	fmt.Fprintf(e.Out, "removed namespace %s\n", name)
	return exitSucceeded
}

// namespaceQuotas is agk namespace quotas: a namespace's quotas, and with quotas given or lifted,
// the quotas set.
//
// PUT /api/v1/namespaces/{ns}/quotas reads its body as the whole set, and lifts each of the four
// optional bounds the body leaves out, as a Terraform apply means it to. A person typing one flag
// means that one bound, and a command sending it alone would lift the rest without a word, among
// them allowed_runner_pools, which keeps a namespace's steps off the pools it was not given. So the
// command reads the quotas first, puts the flags given on top of them, and sends that whole set: a
// bound nobody named is kept, and one is lifted only by naming it with --lift. Another
// administrator's change landing between the read and the write is overwritten, which a person
// setting quotas by hand accepts for a command that never lifts what they did not name; the quotas
// are printed as they then stand.
func namespaceQuotas(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk namespace quotas", "agk namespace quotas <name> [--max-... <quota>] [--lift <quota>] [--server <url>] [-o json]\n\n\tThe quotas given are set and the others kept; --lift max_runs_per_hour lifts one, and is\n\trepeated for each. max_concurrent_tasks and max_retention_days always hold a value.")
	quotas := withQuotas(fs)
	var lifts lifted
	fs.Var(&lifts, "lift", "A quota to lift, by its identifier, max_runs_per_hour, max_artifact_bytes, max_run_duration or allowed_runner_pools; repeated for each.")
	server := fs.String("server", "", "The installation. "+serverDefault)
	output := fs.String("o", "", "json writes the installation's answer as it gave it.")
	name, code, ok := oneNamespace(e, fs, args)
	if !ok {
		return code
	}
	q, set, err := quotas.given()
	if err == nil {
		err = lifts.against(q)
	}
	if err != nil {
		fmt.Fprintln(e.Err, err)
		return exitUsage
	}
	at, ok := reach(e, *server)
	if !ok {
		return exitUsage
	}
	path := "/api/v1/namespaces/" + url.PathEscape(name) + "/quotas"
	var raw json.RawMessage
	if err := at.getJSON(ctx, path, &raw); err != nil {
		return namespaceRefused(e, name, false, err)
	}
	change := set || len(lifts) > 0
	if change {
		var now api.Quotas
		if err := json.Unmarshal(raw, &now); err != nil {
			fmt.Fprintf(e.Err, "the installation's answer about the quotas of %s could not be read: %s\n", name, err)
			return exitNoOutcome
		}
		if err := at.sendJSON(ctx, http.MethodPut, path, merged(now, q, lifts), http.StatusOK, &raw); err != nil {
			return namespaceRefused(e, name, true, err)
		}
	}
	if *output == "json" {
		return indentedAnswer(e, raw)
	}
	var now api.Quotas
	if err := json.Unmarshal(raw, &now); err != nil {
		fmt.Fprintf(e.Err, "the installation's answer about the quotas of %s could not be read: %s\n", name, err)
		return exitNoOutcome
	}
	describeQuotas(e.Out, now)
	return exitSucceeded
}

// lifted are the quotas --lift names, each by its own identifier, as the wire writes it.
type lifted []string

func (l *lifted) String() string { return strings.Join(*l, ",") }

func (l *lifted) Set(name string) error {
	switch name {
	case "max_runs_per_hour", "max_artifact_bytes", "max_run_duration", "allowed_runner_pools":
		*l = append(*l, name)
		return nil
	case "max_concurrent_tasks", "max_retention_days":
		return fmt.Errorf("%s always holds a value, 20 or 90 until an administrator sets another, and is set rather than lifted", name)
	}
	return fmt.Errorf("%q is not a quota that can be lifted: max_runs_per_hour, max_artifact_bytes, max_run_duration and allowed_runner_pools can", name)
}

// against refuses a quota both given and lifted, which asks for two things at once.
func (l lifted) against(given api.Quotas) error {
	for _, name := range l {
		set := map[string]bool{
			"max_runs_per_hour": given.MaxRunsPerHour != 0, "max_artifact_bytes": given.MaxArtifactBytes != 0,
			"max_run_duration": given.MaxRunDuration != "", "allowed_runner_pools": given.AllowedRunnerPools != nil,
		}[name]
		if set {
			return fmt.Errorf("%s is both given and lifted: a quota is set or lifted, not both", name)
		}
	}
	return nil
}

// merged is the quotas a namespace holds, with those given set on top and those lifted taken away:
// the whole set the route is sent, so that what nobody named stays as it was.
func merged(now, given api.Quotas, lifts lifted) api.Quotas {
	out := now
	if given.MaxConcurrentTasks != 0 {
		out.MaxConcurrentTasks = given.MaxConcurrentTasks
	}
	if given.MaxRunsPerHour != 0 {
		out.MaxRunsPerHour = given.MaxRunsPerHour
	}
	if given.MaxArtifactBytes != 0 {
		out.MaxArtifactBytes = given.MaxArtifactBytes
	}
	if given.MaxRetentionDays != 0 {
		out.MaxRetentionDays = given.MaxRetentionDays
	}
	if given.MaxRunDuration != "" {
		out.MaxRunDuration = given.MaxRunDuration
	}
	if given.AllowedRunnerPools != nil {
		out.AllowedRunnerPools = given.AllowedRunnerPools
	}
	for _, name := range lifts {
		switch name {
		case "max_runs_per_hour":
			out.MaxRunsPerHour = 0
		case "max_artifact_bytes":
			out.MaxArtifactBytes = 0
		case "max_run_duration":
			out.MaxRunDuration = ""
		case "allowed_runner_pools":
			out.AllowedRunnerPools = nil
		}
	}
	return out
}

// oneNamespace reads the flags and the one namespace a verb is about, and says whether it may go
// on.
func oneNamespace(e Env, fs *flag.FlagSet, args []string) (string, int, bool) {
	named, code, ok := positional(fs, args)
	if !ok {
		return "", code, false
	}
	if len(named) != 1 {
		fmt.Fprintf(e.Err, "%s names one namespace, as its first word\n", fs.Name())
		return "", exitUsage, false
	}
	// Looked up rather than handed in, since its value is only known once the flags are read.
	if f := fs.Lookup("o"); f != nil && !namespaceFormat(e, f.Value.String()) {
		return "", exitUsage, false
	}
	return named[0], exitSucceeded, true
}

// namespaceFormat refuses an output format other than json, the one there is.
func namespaceFormat(e Env, output string) bool {
	if output != "" && output != "json" {
		fmt.Fprintf(e.Err, "-o is %q: json is the one format there is\n", output)
		return false
	}
	return true
}

// answered prints one namespace the installation answered, after what the command did where it
// says something, or the answer itself with -o json.
func answered(e Env, raw json.RawMessage, output, did string) int {
	if output == "json" {
		return indentedAnswer(e, raw)
	}
	var n api.NamespaceRecord
	if err := json.Unmarshal(raw, &n); err != nil {
		fmt.Fprintf(e.Err, "the installation's answer about a namespace could not be read: %s\n", err)
		return exitNoOutcome
	}
	fmt.Fprintf(e.Out, "%s%s: %s, %s\n", did, n.Name, n.Kind, ownedBy(n))
	if n.Quotas != nil {
		describeQuotas(e.Out, *n.Quotas)
	}
	return exitSucceeded
}

// indentedAnswer writes an answer as the installation gave it, indented.
func indentedAnswer(e Env, raw json.RawMessage) int {
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		fmt.Fprintf(e.Err, "the installation's answer is not JSON: %s\n", err)
		return exitNoOutcome
	}
	fmt.Fprintln(e.Out, b.String())
	return exitSucceeded
}

// ownedBy is who owns a namespace, as a person reads it: a namespace v0.2 made names nobody.
func ownedBy(n api.NamespaceRecord) string {
	if n.Owner == "" {
		return "owned by nobody"
	}
	return "owned by " + n.Owner
}

// describeQuotas writes each quota set, one to a line under its own name, and none that is not: a
// quota left out bounds nothing, and a line saying so of four of them would bury the ones that do.
func describeQuotas(w io.Writer, q api.Quotas) {
	for _, line := range []struct {
		name  string
		value string
	}{
		{"max_concurrent_tasks", countOf(int64(q.MaxConcurrentTasks))},
		{"max_runs_per_hour", countOf(int64(q.MaxRunsPerHour))},
		{"max_artifact_bytes", countOf(q.MaxArtifactBytes)},
		{"max_retention_days", countOf(int64(q.MaxRetentionDays))},
		{"max_run_duration", q.MaxRunDuration},
		{"allowed_runner_pools", strings.Join(q.AllowedRunnerPools, ", ")},
	} {
		if line.value != "" {
			fmt.Fprintf(w, "  %-20s  %s\n", line.name, line.value)
		}
	}
}

func countOf(n int64) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprint(n)
}

// namespaceRefused says what a refusal of a request about a namespace means, and leaves with the
// code it calls for: no outcome where the installation did not answer, and refused otherwise.
//
// A namespace the caller may not read answers what one that does not exist answers, which is the
// point, so the sentence says both rather than guessing; and a change, which only an administrator
// makes, is said to be one where it is refused, since the installation's own sentence names no
// route. A listing names no namespace, so its 404 is the installation's own sentence, which is an
// installation serving no /api/v1/namespaces.
//
// A change answered with a failure that may pass is no outcome rather than a refusal: a gateway
// answering 504 after the API committed is a namespace removed that a script told otherwise would
// try to remove again, and be refused for it. So the sentence says the change may have been made,
// and how to read it back.
func namespaceRefused(e Env, name string, change bool, err error) int {
	said := err.Error()
	status := statusOf(err)
	switch {
	case status == http.StatusUnauthorized:
		said = credentialRefused(e.presentsKept()) + ": " + said
	case status == http.StatusForbidden && change:
		said = "creating, bounding and removing a namespace are an administrator's, through a token with no scope: " + said
	case status == http.StatusNotFound && name != "":
		said = fmt.Sprintf("no namespace %s, or not yours", name)
	case change && passing(err) && !errors.Is(err, errUnreachable):
		fmt.Fprintf(e.Err, "the installation answered %s, and whether namespace %s was changed cannot be told from it: agk namespace show %s reads it back\n", said, name, name)
		return exitNoOutcome
	}
	fmt.Fprintln(e.Err, said)
	if errors.Is(err, errUnreachable) {
		return exitNoOutcome
	}
	return exitRefused
}

// sendJSON sends body as JSON, where it is not nil, and decodes an answer of the status expected
// into out, where out is not nil. Any other status is a *refused, and no answer errUnreachable.
func (r remote) sendJSON(ctx context.Context, method, path string, body any, want int, out any) error {
	var sent io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("the request could not be written: %w", err)
		}
		sent = bytes.NewReader(encoded)
	}
	req, err := r.request(ctx, method, path, sent)
	if err != nil {
		return err
	}
	return r.do(req, want, out)
}
