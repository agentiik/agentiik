package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/jackc/pgx/v5"
)

// credentialArgument is what an argument carrying a credential would be called, wherever it sits in
// a tool's input schema: a token, a password, a passkey, a recovery or an enrolment code, a
// certificate, a TOTP generator, a key, or a secret and its value.
var credentialArgument = regexp.MustCompile(`(?i)token|password|passkey|recovery|enrol|certificate|credential|totp|secret|private|^value$|^code$|^key$`)

// anAPIToken is an API token as the installation writes one, agktoken_ and what follows, which a
// tool answering a token it minted would carry whoever it was minted for.
var anAPIToken = regexp.MustCompile(`agktoken_[A-Za-z0-9_-]{20,}`)

// heldCredentials are the tables a credential is kept in, each read as what tells one row from
// another: a credential issued is a row added, and one a tool took is a row added or written over.
// A token is read by its identifier alone, since using one writes when it was last used.
var heldCredentials = map[string]string{
	"api_tokens":          "id::text",
	"enrolment_codes":     "encode(hash, 'hex')",
	"exchange_codes":      "encode(hash, 'hex')",
	"join_tokens":         "id::text",
	"runners":             "id",
	"sessions":            "encode(hash, 'hex')",
	"webauthn_challenges": "encode(challenge, 'hex')",
	"task_grants":         "hash",
	"totp_enrolments":     "t::text",
	"credentials":         "t::text",
	"webhook_credentials": "t::text",
	"secret_values":       "t::text",
}

// credentialsHeld reads every row heldCredentials names, as the role that migrated the database.
func (x *tenants) credentialsHeld() map[string][]string {
	x.t.Helper()
	conn, err := pgx.Connect(x.t.Context(), x.admin.ConnString())
	if err != nil {
		x.t.Fatal(err)
	}
	defer conn.Close(x.t.Context())
	held := map[string][]string{}
	for table, column := range heldCredentials {
		rows, err := conn.Query(x.t.Context(), "select "+column+" from "+table+" t")
		if err != nil {
			x.t.Fatalf("%s could not be read: %v", table, err)
		}
		values, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			x.t.Fatal(err)
		}
		held[table] = values
	}
	return held
}

// argumentNames are the names of every property of a schema, at any depth.
func argumentNames(schema any) []string {
	o, ok := schema.(map[string]any)
	if !ok {
		return nil
	}
	var names []string
	properties, _ := o["properties"].(map[string]any)
	for _, name := range slices.Sorted(maps.Keys(properties)) {
		names = append(names, name)
		names = append(names, argumentNames(properties[name])...)
	}
	for _, key := range []string{"items", "additionalProperties"} {
		names = append(names, argumentNames(o[key])...)
	}
	return names
}

// "No credential crosses MCP. No tool issues, shows or takes one: no API token, no service account's
// token, no passkey, recovery code or enrolment link, no webhook's secret or certificate, and no
// secret's value." The installation holds one of each: its principals' tokens and finance/nightly's,
// the bootstrap token, a runner's credential and a join token, mallory's passkey and alice's
// password, an enrolment link and a recovery code, a webhook's secret and another's certificate, and
// finance's two secrets. Every tool the user's server lists to a service account, an administrator,
// the bootstrap token and finance's owner is called, naming what finance holds: those that read by
// each of them, and those that change something by the owner, removals last and a secret's own last
// of all, so that each is asked while what it names is there; and the resources it lists are read.
// No input schema names an argument a credential would be passed in, or takes one it does not name;
// no answer carries any of those credentials in any spelling, nor an API token of anybody's; and no
// table a credential is kept in gains a row, or has one written over, by anything a tool did.
func TestNoToolIssuesShowsOrTakesACredential(t *testing.T) {
	x := someTenants(t)
	credentials := map[string]string{"the bootstrap token": theToken, "a runner's credential": x.runnerCredential}
	for who, as := range x.as {
		if as.bearer != "" {
			credentials[who+"'s token"] = as.bearer
		}
	}
	for i, value := range x.values {
		credentials[fmt.Sprintf("secret %d's value", i)] = value
	}
	join := x.answer(x.must("POST", "/api/v1/runner-pools/dmz/join-tokens", asker{bearer: theToken}, api.Issue{Labels: []string{"zone=dmz"}}, http.StatusCreated))
	credentials["a join token"], _ = join["join_token"].(map[string]any)["token"].(string)
	key := make([]byte, 32)
	rand.Read(key)
	secret := "whsec_" + base64.StdEncoding.EncodeToString(key)
	credentials["a webhook's secret"] = secret
	x.must("PUT", "/api/v1/finance/workflows/monthly-invoicing/webhooks/POST/invoicing", x.as["alice"], map[string]string{"secret": secret}, http.StatusNoContent)
	certificate, _ := aCertificate(t, "ledger.example.com", time.Now().Add(-time.Hour), time.Now().Add(time.Hour), false)
	block, _ := pem.Decode(certificate)
	credentials["a webhook's certificate"] = string(certificate)
	credentials["a webhook's certificate, its DER"] = base64.StdEncoding.EncodeToString(block.Bytes)
	x.must("PUT", "/api/v1/finance/workflows/payroll/webhooks/POST/certified", x.as["alice"], map[string]string{"certificate": string(certificate)}, http.StatusNoContent)
	enrolment := x.answer(x.must("POST", "/api/v1/users/oscar/enrolment", x.as["carol"], nil, http.StatusCreated))
	credentials["an enrolment link"], _ = enrolment["link"].(string)
	recovery := x.answer(x.must("POST", "/api/v1/users/mallory/recovery", x.as["carol"], nil, http.StatusCreated))
	credentials["a recovery code"], _ = recovery["code"].(string)
	credentials["a recovery link"], _ = recovery["link"].(string)
	for what, value := range credentials {
		if value == "" {
			t.Fatalf("the installation holds no %s to look for", what)
		}
	}
	before := x.credentialsHeld()

	// carrying names the credential an answer carries, and nothing where it carries none.
	carrying := func(answer string) string {
		for what, value := range credentials {
			for _, spelled := range secretSpellings(value) {
				if strings.Contains(answer, spelled) {
					return what
				}
			}
		}
		if token := anAPIToken.FindString(answer); token != "" {
			return "an API token, " + token
		}
		return ""
	}
	answered := 0
	ask := func(who, method string, params map[string]any) string {
		t.Helper()
		asked, _ := json.Marshal(params)
		result, refusal := x.mcpAsked(x.as[who], method, params)
		b, _ := json.Marshal(map[string]any{"result": result, "error": refusal})
		if what := carrying(string(b)); what != "" {
			t.Errorf("%s %s, asked by %s, answered %s: %s", method, asked, who, what, b)
		}
		if refusal == nil && result["isError"] != true {
			answered++
		}
		return string(b)
	}

	x.as["the bootstrap token"] = asker{bearer: theToken}
	present := map[string]any{
		"namespace": "finance", "workflow": "monthly-invoicing", "run": x.run, "collection": x.collection, "step": "normalize",
		"id": x.grant, "principal": "group:auditors", "role": "viewer", "message": "Through a tool",
		"files": map[string]any{"agentiik.yaml": theWorkflow}, "inputs": map[string]any{"orders": []any{}},
	}
	// What a tool names besides what present says, where it is not what finance holds by that key.
	own := map[string]map[string]any{
		"run.output":        {"name": "invoices"},
		"secret.declare":    {"name": "billing", "provider": "builtin"},
		"secret.remove":     {"name": "ledger"},
		"namespace.update":  {"name": "finance-desk"},
		"workflow.create":   {"name": "drafted"},
		"namespace.create":  {"name": "drafts"},
		"collection.create": {"name": "desk"},
	}
	order := func(tool map[string]any) int {
		hints, _ := tool["annotations"].(map[string]any)
		switch name := tool["name"].(string); {
		case strings.HasPrefix(name, "secret.") && hints["readOnlyHint"] != true:
			return 3
		case hints["destructiveHint"] == true:
			return 2
		case hints["readOnlyHint"] != true:
			return 1
		}
		return 0
	}
	called, seen := 0, map[string]bool{}
	for _, who := range []string{"finance/nightly", "carol", "the bootstrap token", "alice"} {
		result, refusal := x.mcpAsked(x.as[who], "tools/list", nil)
		if refusal != nil {
			t.Fatalf("tools/list as %s answered %v", who, refusal)
		}
		var tools []map[string]any
		for _, tool := range result["tools"].([]any) {
			tools = append(tools, tool.(map[string]any))
		}
		slices.SortStableFunc(tools, func(a, b map[string]any) int { return order(a) - order(b) })
		for _, tool := range tools {
			name := tool["name"].(string)
			input := tool["inputSchema"].(map[string]any)
			if input["additionalProperties"] != false {
				t.Errorf("%s takes arguments its input schema does not name, which a credential could be passed in", name)
			}
			for _, argument := range argumentNames(input) {
				if credentialArgument.MatchString(argument) {
					t.Errorf("%s takes an argument named %s, which is what a credential would be passed in", name, argument)
				}
			}
			seen[name] = true
			if who != "alice" && order(tool) != 0 {
				// Somebody else changing things would change what alice is asked about.
				continue
			}
			named := maps.Clone(present)
			maps.Copy(named, own[name])
			properties, _ := input["properties"].(map[string]any)
			arguments := map[string]any{}
			for key := range properties {
				if v, ok := named[key]; ok {
					arguments[key] = v
				}
			}
			ask(who, "tools/call", map[string]any{"name": name, "arguments": arguments})
			called++
		}
		for _, method := range []string{"resources/list", "resources/templates/list"} {
			ask(who, method, nil)
		}
		for _, uri := range []string{"agentiik://finance/monthly-invoicing/tree/" + theCommit + "/agentiik.yaml", "agentiik://run/" + x.run} {
			ask(who, "resources/read", map[string]any{"uri": uri})
		}
	}
	if called < 50 || len(seen) < 30 || answered < 40 {
		t.Fatalf("%d calls were made of %d tools and %d answered as done, and the user's server lists more than that", called, len(seen), answered)
	}

	// secret.declare is offered no value to write, and is refused one before anything is asked.
	refused := ask("alice", "tools/call", map[string]any{"name": "secret.declare", "arguments": map[string]any{
		"namespace": "finance", "name": "billing", "provider": "builtin", "value": "sk_live_from_a_model",
	}})
	if !strings.Contains(refused, `"code":-32602`) {
		t.Errorf("secret.declare given a value answered %s", refused)
	}

	after := x.credentialsHeld()
	for table, rows := range after {
		for _, row := range rows {
			if !slices.Contains(before[table], row) {
				t.Errorf("a tool wrote %s into %s, which keeps credentials", row, table)
			}
		}
	}
}
