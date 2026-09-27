package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/agentiik/agentiik/api"
)

// agk token: the API tokens of whoever runs it, and of the service accounts of the namespaces they
// own, through /api/v1/auth/tokens.
//
// "Stored hashed, shown once, revocable one by one, listed with last use and device label." agk
// token create prints the token on standard output and nothing else there, so that a script keeps
// it with TOKEN=$(agk token create ...), and says on standard error what it minted and that this is
// the one time it is shown. agk token list prints one line a token, and agk token revoke takes the
// identifier the list prints. -o json writes the installation's answer as it gave it. A refusal is
// the installation's own sentence, exit 1; an installation that did not answer is exit 4.

// tokenCreate is agk token create [--for NS/NAME] [--expires 30d] [--scope ...] [--label TEXT].
func tokenCreate(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk token create", "agk token create [--for <ns>/<name>] [--expires <30d|instant>] [--scope <permission|scope>]... [--label <text>] [--server <url>] [-o json]")
	holder := fs.String("for", "", "A service account of a namespace you own, written NS/NAME. Defaults to you.")
	expires := fs.String("expires", "", "How long the token lasts, in days such as 30d or as a duration such as 12h, or the instant it ends, in RFC 3339. Defaults to 90 days, and a year at most.")
	var scope scopeEntries
	fs.Var(&scope, "scope", "What the token keeps: a permission such as workflow:run, or a namespace or a workflow it reaches, such as finance or finance/monthly-invoicing. Repeated, or separated by commas. Defaults to everything its principal holds.")
	label := fs.String("label", "", "What the token is for or on, listed beside it, so that the one on a lost machine can be revoked.")
	server := fs.String("server", "", "The installation. Defaults to "+serverVariable+".")
	output := fs.String("o", "", "json writes the installation's answer as it gave it.")
	named, code, ok := positional(fs, args)
	if !ok {
		return code
	}
	if len(named) != 0 {
		fmt.Fprintf(e.Err, "agk token create names nothing after it, and was given %q: whose token it is is --for\n", named[0])
		return exitUsage
	}
	if !tokenFormat(e, *output) {
		return exitUsage
	}
	q := api.TokenRequest{Principal: *holder, DeviceLabel: *label}
	if *expires != "" {
		at, err := expiryOf(*expires, e.now())
		if err != nil {
			fmt.Fprintf(e.Err, "--expires: %s\n", err)
			return exitUsage
		}
		q.ExpiresAt = &at
	}
	if len(scope.permissions) > 0 || len(scope.within) > 0 {
		q.Scope = &api.TokenScope{Permissions: scope.permissions, Within: scope.within}
	}
	at, ok := reach(e, *server)
	if !ok {
		return exitUsage
	}

	var raw json.RawMessage
	if err := at.tokenRequest(ctx, http.MethodPost, "/api/v1/auth/tokens", q, http.StatusCreated, &raw); err != nil {
		return tokenRefused(e, err, "")
	}
	if *output == "json" {
		return tokenJSON(e, raw)
	}
	var issued api.IssuedToken
	if err := json.Unmarshal(raw, &issued); err != nil || issued.Token == "" {
		fmt.Fprintf(e.Err, "the installation's answer could not be read as a token: %v\n", err)
		return exitNoOutcome
	}
	fmt.Fprintln(e.Out, issued.Token)
	fmt.Fprintf(e.Err, "minted token %s of %s, expiring at %s: it is shown this once, and kept as its hash alone\n",
		issued.APIToken.ID, issued.APIToken.Principal, issued.APIToken.ExpiresAt.UTC().Format(time.RFC3339))
	return exitSucceeded
}

// tokenList is agk token list: the tokens still accepted, one a line, newest first.
func tokenList(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk token list", "agk token list [--server <url>] [-o json]")
	server := fs.String("server", "", "The installation. Defaults to "+serverVariable+".")
	output := fs.String("o", "", "json writes the installation's answer as it gave it.")
	named, code, ok := positional(fs, args)
	if !ok {
		return code
	}
	if len(named) != 0 {
		fmt.Fprintf(e.Err, "agk token list names nothing, and was given %q: it lists every token you may revoke\n", named[0])
		return exitUsage
	}
	if !tokenFormat(e, *output) {
		return exitUsage
	}
	at, ok := reach(e, *server)
	if !ok {
		return exitUsage
	}
	var raw json.RawMessage
	if err := at.tokenRequest(ctx, http.MethodGet, "/api/v1/auth/tokens", nil, http.StatusOK, &raw); err != nil {
		return tokenRefused(e, err, "")
	}
	if *output == "json" {
		return tokenJSON(e, raw)
	}
	var listed api.TokenList
	if err := json.Unmarshal(raw, &listed); err != nil {
		fmt.Fprintf(e.Err, "the installation's list of tokens could not be read: %s\n", err)
		return exitNoOutcome
	}
	if len(listed.Tokens) == 0 {
		fmt.Fprintln(e.Out, "no token")
		return exitSucceeded
	}
	ids, principals := 0, 0
	for _, tk := range listed.Tokens {
		ids, principals = max(ids, len(tk.ID)), max(principals, len(tk.Principal))
	}
	for _, tk := range listed.Tokens {
		used := "never used"
		if tk.LastUsedAt != nil {
			used = "last used " + tk.LastUsedAt.UTC().Format(time.RFC3339)
		}
		line := fmt.Sprintf("%-*s  %-*s  expires %s  %s", ids, tk.ID, principals, tk.Principal, tk.ExpiresAt.UTC().Format(time.RFC3339), used)
		if tk.Scope != nil {
			line += "  " + scopeLine(*tk.Scope)
		}
		if tk.DeviceLabel != "" {
			line += "  " + printable(tk.DeviceLabel)
		}
		fmt.Fprintln(e.Out, line)
	}
	return exitSucceeded
}

// tokenRevoke is agk token revoke ID: one token, from its next request.
func tokenRevoke(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk token revoke", "agk token revoke <id> [--server <url>]")
	server := fs.String("server", "", "The installation. Defaults to "+serverVariable+".")
	named, code, ok := positional(fs, args)
	if !ok {
		return code
	}
	if len(named) != 1 {
		fmt.Fprintln(e.Err, "agk token revoke names one token, by the identifier agk token list prints, which is not the token")
		return exitUsage
	}
	at, ok := reach(e, *server)
	if !ok {
		return exitUsage
	}
	id := named[0]
	if err := at.tokenRequest(ctx, http.MethodDelete, "/api/v1/auth/tokens/"+url.PathEscape(id), nil, http.StatusNoContent, nil); err != nil {
		return tokenRefused(e, err, id)
	}
	fmt.Fprintf(e.Out, "revoked token %s: it opens nothing from its next request\n", id)
	return exitSucceeded
}

// scopeEntries is --scope, written more than once or with commas: each entry naming a permission,
// which holds a colon, or a namespace or a workflow it reaches, which never does.
type scopeEntries struct {
	permissions, within []string
}

func (s *scopeEntries) String() string {
	return strings.Join(append(append([]string(nil), s.permissions...), s.within...), ",")
}

func (s *scopeEntries) Set(v string) error {
	for _, entry := range strings.Split(v, ",") {
		entry = strings.TrimSpace(entry)
		switch {
		case entry == "":
			return errors.New("an entry is empty: name a permission such as workflow:run, or a namespace or a workflow such as finance/monthly-invoicing")
		case strings.Contains(entry, ":"):
			s.permissions = append(s.permissions, entry)
		default:
			s.within = append(s.within, entry)
		}
	}
	return nil
}

var _ flag.Value = (*scopeEntries)(nil)

// expiryOf reads --expires: a number of days, 30d, a duration Go reads, 12h, or an instant in RFC
// 3339. A length is counted from now, on this machine's clock, and the installation holds the
// instant it comes to to a year after it mints the token.
func expiryOf(written string, now time.Time) (time.Time, error) {
	if at, err := time.Parse(time.RFC3339, written); err == nil {
		return at, nil
	}
	var length time.Duration
	if days, ok := strings.CutSuffix(written, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return time.Time{}, fmt.Errorf("%q is not a number of days: write 30d", written)
		}
		// Outside these bounds the length does not fit a Duration, and multiplying would wrap
		// it round to some other length the token would then be minted with.
		switch {
		case n < 1:
			return time.Time{}, fmt.Errorf("%q is no time at all: a token expires after it is minted", written)
		case n > int(math.MaxInt64/int64(24*time.Hour)):
			return time.Time{}, fmt.Errorf("%q is more days than a length holds, and a token lasts a year at most", written)
		}
		length = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if length, err = time.ParseDuration(written); err != nil {
			return time.Time{}, fmt.Errorf("%q is neither a length, such as 30d or 12h, nor an instant in RFC 3339, such as 2027-03-26T09:10:00Z", written)
		}
	}
	if length <= 0 {
		return time.Time{}, fmt.Errorf("%q is no time at all: a token expires after it is minted", written)
	}
	return now.UTC().Add(length).Truncate(time.Second), nil
}

// scopeLine is a scope as the list prints it: the permissions kept, then where.
func scopeLine(s api.TokenScope) string {
	var parts []string
	if len(s.Permissions) > 0 {
		parts = append(parts, "keeps "+strings.Join(s.Permissions, ","))
	}
	if len(s.Within) > 0 {
		parts = append(parts, "within "+strings.Join(s.Within, ","))
	}
	return strings.Join(parts, " ")
}

// printable is a label as the list prints it: as written, or quoted where it holds a character a
// terminal would act on rather than show, since whoever minted the token chose it.
func printable(label string) string {
	if strings.ContainsFunc(label, unicode.IsControl) {
		return strconv.Quote(label)
	}
	return label
}

// tokenFormat refuses an -o other than json, saying so.
func tokenFormat(e Env, output string) bool {
	if output != "" && output != "json" {
		fmt.Fprintf(e.Err, "-o is %q: json is the one format there is\n", output)
		return false
	}
	return true
}

// tokenJSON writes the installation's answer as it gave it, indented.
func tokenJSON(e Env, raw json.RawMessage) int {
	var indented bytes.Buffer
	if err := json.Indent(&indented, raw, "", "  "); err != nil {
		fmt.Fprintf(e.Err, "the installation's answer is not JSON: %s\n", err)
		return exitNoOutcome
	}
	fmt.Fprintln(e.Out, indented.String())
	return exitSucceeded
}

// tokenRequest sends one request about tokens, body as JSON where it is not nil, and reads an answer
// of the status expected into out where out is not nil.
func (r remote) tokenRequest(ctx context.Context, method, path string, body any, want int, out any) error {
	var sent io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		sent = bytes.NewReader(encoded)
	}
	req, err := r.request(ctx, method, path, sent)
	if err != nil {
		return err
	}
	return r.do(req, want, out)
}

// tokenRefused says why the installation said no, in its own sentence, and leaves with the code for
// it: no answer at all is no outcome, and anything the installation said is a refusal.
func tokenRefused(e Env, err error, id string) int {
	switch status := statusOf(err); {
	case errors.Is(err, errUnreachable):
		fmt.Fprintln(e.Err, err)
		return exitNoOutcome
	case status == http.StatusUnauthorized:
		fmt.Fprintf(e.Err, "the installation did not accept the credential in %s: %s\n", tokenVariable, err)
	case status == http.StatusNotFound && id != "":
		fmt.Fprintf(e.Err, "no token %s, or not yours to revoke\n", id)
	default:
		fmt.Fprintln(e.Err, err)
	}
	return exitRefused
}
