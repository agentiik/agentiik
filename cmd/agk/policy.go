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
	"strconv"
	"strings"

	"github.com/agentiik/agentiik/api"
)

// agk auth policy: "Prints the authentication policy, of the installation or of --namespace, and
// with settings given, sets it."
//
// The routes replace the whole policy: the installation's returns a setting its body leaves out to
// its default, and a namespace's drops it, to inherit the installation's. A person typing one flag
// means one setting, not the others reset with it, so the command reads the policy first, sets the
// flags given on top, and sends that whole set, as agk namespace quotas does with the quotas. A
// namespace's setting is dropped only where --inherit names it, and the installation's are never
// dropped, since every one of them always holds a value.

// The settings of the policy, as $defs/authPolicy spells them, which --inherit names.
var policySettings = []string{"password", "passkey", "user_verification", "device_bound_only", "min_passkeys"}

// setBool is a boolean flag that says whether it was given, as --device-bound-only and
// --device-bound-only=false both are.
type setBool struct {
	value, given bool
}

func (b *setBool) String() string { return strconv.FormatBool(b.value) }

func (b *setBool) Set(s string) error {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return fmt.Errorf("%q is true or false", s)
	}
	b.value, b.given = v, true
	return nil
}

func (b *setBool) IsBoolFlag() bool { return true }

// inherited are the settings --inherit names, each by its own identifier.
type inherited []string

func (i *inherited) String() string { return strings.Join(*i, ",") }

func (i *inherited) Set(name string) error {
	for _, s := range policySettings {
		if s == name {
			*i = append(*i, name)
			return nil
		}
	}
	return fmt.Errorf("%q is not a setting of the policy: %s are", name, strings.Join(policySettings, ", "))
}

// authPolicy is agk auth policy.
func authPolicy(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk auth policy", "agk auth policy [--namespace <namespace>] [--password allowed|forbidden] [--passkey optional|required] [--user-verification required|preferred] [--device-bound-only[=false]] [--min-passkeys <n>] [--inherit <setting>] [--server <url>] [-o json]\n\n\tThe settings given are set and the others kept. A namespace's policy tightens the installation's,\n\tand --inherit min_passkeys drops one of its settings, which it then inherits; repeated for each.")
	namespace := fs.String("namespace", "", "The namespace whose tightening of the installation's policy is printed or set, rather than the installation's.")
	password := fs.String("password", "", "allowed, or forbidden, which deletes the passwords it reaches.")
	passkey := fs.String("passkey", "", "optional, or required.")
	verification := fs.String("user-verification", "", "required, or preferred.")
	var bound setBool
	fs.Var(&bound, "device-bound-only", "Refuses synced passkeys; --device-bound-only=false accepts them.")
	minimum := fs.Int("min-passkeys", 0, "How many passkeys an account holds before its password goes, and below which none is removed: a whole number from 1.")
	var inherit inherited
	fs.Var(&inherit, "inherit", "A setting of --namespace's policy to drop, which it then inherits from the installation's, by its identifier; repeated for each.")
	server := fs.String("server", "", "The installation. Defaults to "+serverVariable+".")
	output := fs.String("o", "", "json writes the installation's answer as it gave it.")
	words, code, ok := positional(fs, args)
	if !ok {
		return code
	}
	counted := false
	fs.Visit(func(f *flag.Flag) { counted = counted || f.Name == "min-passkeys" })
	given, err := policyGiven(*password, *passkey, *verification, bound, *minimum, counted)
	switch {
	case len(words) != 0:
		err = fmt.Errorf("agk auth policy names no word, and was given %q: a namespace is named with --namespace", words[0])
	case err == nil && len(inherit) > 0 && *namespace == "":
		err = errors.New("--inherit drops a setting of a namespace's policy, named with --namespace: the installation's holds every setting, and is set rather than inherited")
	case err == nil:
		err = inherit.against(given)
	}
	if err == nil && !namespaceFormat(e, *output) {
		return exitUsage
	}
	if err != nil {
		fmt.Fprintln(e.Err, err)
		return exitUsage
	}
	at, ok := reach(e, *server)
	if !ok {
		return exitUsage
	}
	path := "/api/v1/auth/policy"
	if *namespace != "" {
		path = "/api/v1/" + url.PathEscape(*namespace) + "/auth/policy"
	}
	var raw json.RawMessage
	if err := at.getJSON(ctx, path, &raw); err != nil {
		return policyRefused(e, *namespace, false, err)
	}
	if given != (api.AuthPolicy{}) || len(inherit) > 0 {
		var now api.AuthPolicy
		if err := json.Unmarshal(raw, &now); err != nil {
			fmt.Fprintf(e.Err, "the installation's answer about the authentication policy could not be read: %s\n", err)
			return exitNoOutcome
		}
		if err := at.sendJSON(ctx, http.MethodPut, path, mergedPolicy(now, given, inherit), http.StatusOK, &raw); err != nil {
			return policyRefused(e, *namespace, true, err)
		}
	}
	if *output == "json" {
		return indentedAnswer(e, raw)
	}
	var now api.AuthPolicy
	if err := json.Unmarshal(raw, &now); err != nil {
		fmt.Fprintf(e.Err, "the installation's answer about the authentication policy could not be read: %s\n", err)
		return exitNoOutcome
	}
	describePolicy(e.Out, *namespace, now)
	return exitSucceeded
}

// policyGiven is the settings the flags give, refusing a value the wire refuses before anything is
// sent; counted is whether --min-passkeys was given, which zero cannot say. A setting not given is
// left out, as the wire leaves out what a namespace does not set.
func policyGiven(password, passkey, verification string, bound setBool, minimum int, counted bool) (api.AuthPolicy, error) {
	var given api.AuthPolicy
	for _, c := range []struct {
		name, value string
		values      [2]string
		into        *string
	}{
		{"password", password, [2]string{"allowed", "forbidden"}, &given.Password},
		{"passkey", passkey, [2]string{"optional", "required"}, &given.Passkey},
		{"user-verification", verification, [2]string{"required", "preferred"}, &given.UserVerification},
	} {
		if c.value == "" {
			continue
		}
		if c.value != c.values[0] && c.value != c.values[1] {
			return api.AuthPolicy{}, fmt.Errorf("--%s is %q, and it is %s or %s", c.name, c.value, c.values[0], c.values[1])
		}
		*c.into = c.value
	}
	if bound.given {
		given.DeviceBoundOnly = &bound.value
	}
	if counted && minimum < 1 {
		return api.AuthPolicy{}, fmt.Errorf("--min-passkeys is %d, and it is a whole number from 1: zero would let a password go from an account with nothing to replace it", minimum)
	}
	given.MinPasskeys = minimum
	return given, nil
}

// against refuses a setting both given and inherited, which asks for two things at once.
func (i inherited) against(given api.AuthPolicy) error {
	for _, name := range i {
		set := map[string]bool{
			"password": given.Password != "", "passkey": given.Passkey != "", "user_verification": given.UserVerification != "",
			"device_bound_only": given.DeviceBoundOnly != nil, "min_passkeys": given.MinPasskeys != 0,
		}[name]
		if set {
			return fmt.Errorf("%s is both given and inherited: a setting is set or inherited, not both", name)
		}
	}
	return nil
}

// mergedPolicy is the policy held, with the settings given on top and those inherited dropped: the
// whole set the route is sent, so that what nobody named stays as it was.
func mergedPolicy(now, given api.AuthPolicy, inherit inherited) api.AuthPolicy {
	out := now
	if given.Password != "" {
		out.Password = given.Password
	}
	if given.Passkey != "" {
		out.Passkey = given.Passkey
	}
	if given.UserVerification != "" {
		out.UserVerification = given.UserVerification
	}
	if given.DeviceBoundOnly != nil {
		out.DeviceBoundOnly = given.DeviceBoundOnly
	}
	if given.MinPasskeys != 0 {
		out.MinPasskeys = given.MinPasskeys
	}
	for _, name := range inherit {
		switch name {
		case "password":
			out.Password = ""
		case "passkey":
			out.Passkey = ""
		case "user_verification":
			out.UserVerification = ""
		case "device_bound_only":
			out.DeviceBoundOnly = nil
		case "min_passkeys":
			out.MinPasskeys = 0
		}
	}
	return out
}

// describePolicy writes each setting the policy holds, one to a line under its own name: every one
// of the installation's, and those a namespace tightens, which are all it holds.
func describePolicy(w io.Writer, namespace string, p api.AuthPolicy) {
	if namespace != "" && p == (api.AuthPolicy{}) {
		fmt.Fprintf(w, "namespace %s tightens nothing, and the installation's policy applies there as it is\n", namespace)
		return
	}
	bound, count := "", ""
	if p.DeviceBoundOnly != nil {
		bound = strconv.FormatBool(*p.DeviceBoundOnly)
	}
	if p.MinPasskeys != 0 {
		count = strconv.Itoa(p.MinPasskeys)
	}
	for _, line := range []struct{ name, value string }{
		{"password", p.Password}, {"passkey", p.Passkey}, {"user_verification", p.UserVerification},
		{"device_bound_only", bound}, {"min_passkeys", count},
	} {
		if line.value != "" {
			fmt.Fprintf(w, "  %-17s  %s\n", line.name, line.value)
		}
	}
}

// policyRefused says what a refusal of the policy's routes means, and leaves with the code it means.
func policyRefused(e Env, namespace string, change bool, err error) int {
	said := err.Error()
	switch status := statusOf(err); {
	case status == http.StatusUnauthorized:
		said = fmt.Sprintf("the installation did not accept the credential in %s: %s", tokenVariable, said)
	case status == http.StatusForbidden && change:
		said = "setting the authentication policy is an administrator's, through a token with no scope: " + said
	case status == http.StatusNotFound && namespace != "":
		said = fmt.Sprintf("no namespace %s, or not yours", namespace)
	case change && passing(err) && !errors.Is(err, errUnreachable):
		fmt.Fprintf(e.Err, "the installation answered %s, and whether the policy was changed cannot be told from it: agk auth policy reads it back\n", said)
		return exitNoOutcome
	}
	fmt.Fprintln(e.Err, said)
	if errors.Is(err, errUnreachable) {
		return exitNoOutcome
	}
	return exitRefused
}
