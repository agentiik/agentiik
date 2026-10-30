package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strconv"
	"strings"

	"github.com/agentiik/agentiik/mcp"
)

// A call of many items: run_cancel with runs, and workflow_run with inputs_each. "Cancelling 76 runs
// took 76 calls of run_cancel", each a round trip through the client and the model; a call listing
// them is answered in one. It is no route of its own: each item is made as its own request of the
// route the tool mirrors, as the caller, in the order given, so that each is authorised, refused,
// counted against the namespace's quota and audited exactly as a call naming it alone would be, and
// there is one way a run is started or cancelled to keep checked rather than two.

// bulk is the argument a call lists its items under, in place of one.
type bulk struct {
	// list is the argument the items are listed under, and one the argument each stands for in its
	// own request.
	list, one string

	// required says whether a call has to give one of the two; echo whether each item's outcome
	// names it under one, as a run's identifier is named, so that a refusal says which run it
	// refuses, where a run's inputs, the caller's own and of any size, are not repeated back.
	required, echo bool
}

// items are the items a call lists, and false for a call naming one item or none, which is made as
// the route's one request. A call naming both is refused, since which one it meant is not the
// server's to guess.
func (b *bulk) items(tool string, args map[string]any) ([]any, bool, error) {
	list, many := args[b.list]
	_, one := args[b.one]
	switch {
	case many && one:
		return nil, false, mcp.InvalidParams("%s takes %s or %s, and not both", tool, b.one, b.list)
	case !many && !one && b.required:
		return nil, false, mcp.InvalidParams("%s takes %s, or %s listing several", tool, b.one, b.list)
	}
	items, _ := list.([]any)
	return items, many, nil
}

// inBulk makes one request of t's route per item, as the caller, and answers each one's outcome in
// order: what the route answered, a run and its state or the refusal, with the item's index and the
// status. The call is an error only where no item succeeded, since a call that started 99 runs of
// 100 did what it was asked for 99 of them, and a client told it failed would start them again.
//
// A quota reached stops the items there: the namespace's runs per hour are what the route refused
// it for, and every item after it would be refused for the same until the time it names has passed,
// each one a request that changes nothing. The answer says how many were not attempted and when one
// more fits, so that the client calls again with the rest then.
func (m *MCP) inBulk(r *http.Request, t routed, args map[string]any, items []any) (*mcp.CallResult, error) {
	outcomes := make([]map[string]any, 0, len(items))
	var lines []string
	succeeded, refused, retry := 0, 0, -1
	for i, item := range items {
		if r.Context().Err() != nil {
			break
		}
		one := maps.Clone(args)
		delete(one, t.each.list)
		one[t.each.one] = item
		method, path, query, body := t.request(one)
		a, err := m.dispatch(r, t.name, method, path, query, body)
		if err != nil {
			return nil, err
		}
		outcome := map[string]any{}
		if strings.HasPrefix(a.header.Get("Content-Type"), "application/json") {
			if err := json.Unmarshal(a.body.Bytes(), &outcome); err != nil {
				outcome = map[string]any{}
			}
		}
		label := fmt.Sprintf("%s[%d]", t.each.list, i)
		if t.each.echo {
			label += " " + fmt.Sprint(item)
		}
		if a.status < 400 {
			succeeded++
			lines = append(lines, label+": "+doneText(t, outcome))
		} else {
			refused++
			said := strings.ReplaceAll(refusedText(a.status, outcome), "\n", "; ")
			if a.status == http.StatusTooManyRequests {
				if seconds, err := strconv.Atoi(a.header.Get("Retry-After")); err == nil {
					outcome["retry_after_seconds"], retry = seconds, seconds
				}
			}
			lines = append(lines, fmt.Sprintf("%s: refused, %d: %s", label, a.status, said))
		}
		if t.each.echo {
			outcome[t.each.one] = item
		}
		outcome["item"], outcome["status"] = i, a.status
		outcomes = append(outcomes, outcome)
		if a.status == http.StatusTooManyRequests {
			break
		}
	}

	rest := len(items) - len(outcomes)
	summary := fmt.Sprintf("Of %d, %d %s and %d refused", len(items), succeeded, doneWord(t), refused)
	if rest > 0 {
		summary += fmt.Sprintf("; %d not attempted", rest)
	}
	text := append([]string{summary + "."}, lines...)
	structured := map[string]any{"results": outcomes, "succeeded": succeeded, "refused": refused, "not_attempted": rest}
	switch last := len(outcomes) - 1; {
	case rest == 0:
	case last >= 0 && outcomes[last]["status"] == http.StatusTooManyRequests:
		line := fmt.Sprintf("The namespace's quota refused %s[%d], and would refuse the %d after it for the same reason until one more run fits, so they were not attempted", t.each.list, last, rest)
		if retry >= 0 {
			structured["retry_after_seconds"] = retry
			line += fmt.Sprintf(": call again with them once %d seconds have passed", retry)
		}
		text = append(text, line+".")
	default:
		text = append(text, fmt.Sprintf("The call was cancelled before the last %d were attempted.", rest))
	}
	return &mcp.CallResult{Content: []mcp.Content{mcp.Text(strings.Join(text, "\n"))}, StructuredContent: structured, IsError: succeeded == 0}, nil
}

// doneWord is what an item that succeeded did, as the summary counts it.
func doneWord(t routed) string {
	if t.each.echo {
		return "asked to cancel"
	}
	return "started"
}

// doneText is one item that succeeded, as a line of the answer: the run started, its state and the
// commit it is pinned to, or the run asked to cancel, as its route answers either.
func doneText(t routed, outcome map[string]any) string {
	if t.each.echo {
		return "asked to cancel"
	}
	line := fmt.Sprintf("started run %v", outcome["run"])
	if state, _ := outcome["state"].(string); state != "" {
		line += ", " + state
	}
	if commit, _ := outcome["commit"].(string); commit != "" {
		line += ", at " + commit
	}
	return line
}

// pinnedTags is workflow_pin's answer: each tag with the reference by digest it is pinned to, and
// the route's answer as structured content. A registry the installation could not ask, 502, is said
// as a refusal in the route's words rather than as a failure of the installation, since what comes
// next, asking again later or pinning with agk push, is the caller's to decide.
func pinnedTags(_ map[string]any, a *toolAnswer, _ requester) (*mcp.CallResult, error) {
	switch a.status {
	case http.StatusOK:
	case http.StatusBadGateway:
		var structured any
		_ = json.Unmarshal(a.body.Bytes(), &structured)
		return &mcp.CallResult{Content: []mcp.Content{mcp.Text(refusedText(a.status, structured))}, StructuredContent: structured, IsError: true}, nil
	default:
		return result(a)
	}
	var answer Images
	if err := json.Unmarshal(a.body.Bytes(), &answer); err != nil {
		return nil, err
	}
	res, err := result(a)
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(answer.Pins))
	for _, p := range answer.Pins {
		lines = append(lines, p.Reference+" is pinned to "+p.Image)
	}
	res.Content = []mcp.Content{mcp.Text(strings.Join(lines, "\n"))}
	return res, nil
}
