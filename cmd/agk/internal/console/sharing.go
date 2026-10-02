package console

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/agentiik/agentiik/access"
)

// The sharing view is what agk grants prints, kept live, for a namespace or one workflow of it: the
// grants and denies as written, those a workflow inherits from its namespace said to be, and what
// they come to for one principal, each permission with the grants that give it and the deny that
// takes it away, worked out by access.Resolve, the rule the API decides every request by. It reads
// GET /api/v1/{ns}/grants and GET /api/v1/{ns}/workflows/{name}/grants, which grant:manage at the
// scope answers, so it offers the scopes where the principal holds it, and changes nothing: a grant
// deserves the sentence agk share makes a person write.

// sharingRead is the grants of a scope, and the members of every group where an administrator reads
// them.
type sharingRead struct {
	scope   string
	grants  []access.Grant
	members map[string][]string
	err     error
}

// grantsPath is the route listing a scope's grants.
func grantsPath(scope string) string {
	ns, wf, ok := strings.Cut(scope, "/")
	if ok {
		return "/api/v1/" + url.PathEscape(ns) + "/workflows/" + url.PathEscape(wf) + "/grants"
	}
	return "/api/v1/" + url.PathEscape(ns) + "/grants"
}

func (m Model) readSharing() tea.Cmd {
	if m.scope == "" {
		return nil
	}
	scope, admin, members := m.scope, m.me.Admin, m.members
	return func() tea.Msg {
		var listed struct {
			Grants []access.Grant `json:"grants"`
		}
		if err := m.o.Read(m.ctx, grantsPath(scope), &listed); err != nil {
			return sharingRead{scope: scope, err: err}
		}
		read := sharingRead{scope: scope, grants: listed.Grants, members: members}
		if admin && members == nil {
			// Who is in a group is an administrator's to read: anybody else is told that a group's
			// grants apply to its members, and not whether the principal is one.
			var groups struct {
				Groups []struct {
					Name    string   `json:"name"`
					Members []string `json:"members"`
				} `json:"groups"`
			}
			if err := m.o.Read(m.ctx, "/api/v1/groups", &groups); err == nil {
				read.members = map[string][]string{}
				for _, g := range groups.Groups {
					read.members[g.Name] = g.Members
				}
			}
		}
		return read
	}
}

// managed are the scopes whose grants the principal may list, where it holds grant:manage: the
// namespaces and workflows /me names, and the workflows of a namespace managed that the runs read
// name, in order.
func (m Model) managed() []string {
	held := map[string]bool{}
	for scope, permissions := range m.me.Permissions {
		if slices.Contains(permissions, string(access.GrantManage)) {
			held[scope] = true
		}
	}
	for _, r := range m.runs {
		if held[r.Namespace] {
			held[r.Namespace+"/"+r.Workflow] = true
		}
	}
	for _, f := range m.flows {
		if held[f.namespace] {
			held[f.key()] = true
		}
	}
	return slices.Sorted(func(yield func(string) bool) {
		for scope := range held {
			if !yield(scope) {
				return
			}
		}
	})
}

// sharingScope is the scope the sharing view opens on: the workflow in front of the principal, a
// run's or the one chosen, where it may list its grants, then that workflow's namespace, then the
// scope shown last, then the first it may list.
func (m Model) sharingScope() string {
	var ns, wf string
	switch m.view {
	case workflowsView:
		if f, ok := m.chosenFlow(); ok {
			ns, wf = f.namespace, f.workflow
		}
	case runView, graphView:
		if m.run != nil {
			ns, wf = m.run.Namespace, m.run.Workflow
		} else {
			ns, wf, _ = strings.Cut(m.graphOf, "/")
		}
	case runsView:
		for _, r := range m.runs {
			if string(r.Run) == m.selected {
				ns, wf = r.Namespace, r.Workflow
			}
		}
	}
	managed := m.managed()
	for _, s := range []string{ns + "/" + wf, ns, m.scope} {
		if s != "" && slices.Contains(managed, s) {
			return s
		}
	}
	if len(managed) > 0 {
		return managed[0]
	}
	return ""
}

// scopeOf is a scope as a grant writes it.
func scopeOf(scope string) access.Scope {
	ns, wf, _ := strings.Cut(scope, "/")
	return access.Scope{Namespace: ns, Workflow: wf}
}

// kindOf is what a grant's principal names, by how it is written: group:NAME, NS/NAME for a service
// account, a login otherwise.
func kindOf(principal string) string {
	switch {
	case strings.HasPrefix(principal, "group:"):
		return "group"
	case strings.Contains(principal, "/"):
		return "service account"
	}
	return "user"
}

// principalsOf is whom the view offers to resolve: the caller first, then every principal the
// grants name, users, then groups, then service accounts, each by name.
func (m Model) principalsOf() []string {
	order := []string{"user", "group", "service account"}
	var named []string
	for _, g := range m.grants {
		if g.Principal != m.me.Principal && !slices.Contains(named, g.Principal) {
			named = append(named, g.Principal)
		}
	}
	slices.SortFunc(named, func(a, b string) int {
		if d := slices.Index(order, kindOf(a)) - slices.Index(order, kindOf(b)); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	if m.me.Principal == "" {
		return named
	}
	return append([]string{m.me.Principal}, named...)
}

// whomOf is the principal the grants are resolved for: the one chosen while the grants still name
// it, and the caller otherwise.
func (m Model) whomOf() string {
	if m.whom != "" && slices.Contains(m.principalsOf(), m.whom) {
		return m.whom
	}
	return m.me.Principal
}

// groupsOf are the groups of the principal resolved for, and known says whether they could be
// read: the caller's from GET /api/v1/me, anybody's for an administrator, and none for a group or a
// service account, since a group's grants are its own and a service account belongs to none.
func (m Model) groupsOf(whom string) (groups []string, known bool) {
	switch {
	case kindOf(whom) != "user":
		return nil, true
	case whom == m.me.Principal:
		for _, g := range m.me.Groups {
			groups = append(groups, strings.TrimPrefix(g, "group:"))
		}
		return groups, true
	case m.members != nil:
		for name, members := range m.members {
			if slices.Contains(members, whom) {
				groups = append(groups, name)
			}
		}
		slices.Sort(groups)
		return groups, true
	}
	return nil, false
}

// resolvedLine is one permission of the arithmetic: the grants that give it, the denies that take
// it, and, where the principal's groups could not be read, the grants of groups that would give it
// to a member; held is what access.Resolve answers.
type resolvedLine struct {
	permission            access.Permission
	gives, takes, members []access.Grant
	held                  bool
}

// resolved is what whom holds at the scope as of now, permission by permission, held as
// access.Resolve answers it and explained by the grants Gives and Takes name.
func (m Model) resolved(whom string, now time.Time) ([]resolvedLine, error) {
	at := scopeOf(m.scope)
	groups, known := m.groupsOf(whom)
	who := access.Principal{Ref: whom, Groups: groups}
	held, err := access.Resolve(who, m.grants, at, now)
	if err != nil {
		return nil, err
	}
	var lines []resolvedLine
	for _, p := range access.Permissions {
		l := resolvedLine{permission: p, held: held.Has(p)}
		for _, g := range m.grants {
			if g.Expired(now) {
				continue
			}
			switch {
			case g.Takes(who, p, at):
				l.takes = append(l.takes, g)
			case g.Gives(who, p, at):
				l.gives = append(l.gives, g)
			case !known && kindOf(g.Principal) == "group" && g.Gives(access.Principal{Ref: whom, Groups: []string{strings.TrimPrefix(g.Principal, "group:")}}, p, at):
				l.members = append(l.members, g)
			}
		}
		lines = append(lines, l)
	}
	return lines, nil
}

// source is a grant as the arithmetic names it, as the documentation's sharing view writes it: the
// scope it was written at, through whom where it is not the principal's own, and what it gives.
func source(g access.Grant, whom string) string {
	what := string(g.Role)
	if g.Deny != "" {
		what = "deny " + string(g.Deny)
	}
	if g.Principal != whom {
		what = g.Principal + ", " + what
	}
	return g.Scope.String() + ": " + what
}

// unheld says why a permission nobody gives is not held: secret:use and secret:write come from
// namespace grants alone, and workflow:delete and grant:manage are the owner's.
func (m Model) unheld(p access.Permission) string {
	switch p {
	case access.SecretUse, access.SecretWrite:
		if scopeOf(m.scope).Workflow != "" {
			return "from namespace grants alone"
		}
	case access.WorkflowDelete, access.GrantManage:
		return "the owner's"
	}
	return "not given"
}

// day is an expiry as a date, with its time where it is not midnight UTC, where one written as a
// number of days or a date falls.
func day(at time.Time) string {
	at = at.UTC()
	if at.Equal(at.Truncate(24 * time.Hour)) {
		return at.Format("2006-01-02")
	}
	return at.Format("2006-01-02 15:04")
}

// roleColumns are the roles' columns, as the documentation's table of roles writes them, and the
// roles each column is in.
var roleColumns = []struct {
	name        string
	permissions []access.Permission
}{
	{"read", []access.Permission{access.WorkflowRead, access.RunRead}},
	{"run", []access.Permission{access.WorkflowRun, access.RunRead}},
	{"write", []access.Permission{access.WorkflowWrite, access.SecretUse}},
	{"data", []access.Permission{access.RunReadData}},
	{"secret", []access.Permission{access.SecretWrite}},
	{"delete", []access.Permission{access.WorkflowDelete}},
	{"grant", []access.Permission{access.GrantManage}},
}

// carried says whether a role says yes to a column: holds every permission of it.
func carried(r access.Role, permissions []access.Permission) bool {
	for _, p := range permissions {
		if !r.Permissions().Has(p) {
			return false
		}
	}
	return true
}

// neverShown is what no grant shows anybody, as the web console's sharing panel says it.
var neverShown = []string{
	"Secret values, at any role: a declaration is shown, never what it holds, which exists inside a container alone.",
	"Another namespace's artifacts: a presigned address covers one artifact of one run, for 5 minutes.",
	"The host that ran a task: a run names each task's runner by its identifier, and nothing of the machine.",
	"Workflows somebody cannot read, in any listing, search or error, or in how long a refusal takes.",
}

// grantColumns are the grants' columns: the principal, what it gives, where it was written, when it
// ends, and at full width who wrote it and when.
func (m Model) grantColumns(width int, principal, gives, scope, expires, granted []part) []part {
	cells := []cell{{principal, 22, false}, {gives, 19, false}, {scope, 0, false}, {expires, 10, false}}
	if width >= 110 {
		cells = append(cells, cell{granted, 22, false})
	}
	return laid(cells, 2, width)
}

// grantsLines are the grants as written, the one chosen drawn as the selection, then what each
// role carries and what sharing never exposes, as many as room holds.
func (m Model) grantsLines(t theme, width, room int) []string {
	line := func(parts ...part) string { return t.line(false, width, within(parts, width)...) }
	at := scopeOf(m.scope)
	inherited, denies := 0, 0
	for _, g := range m.grants {
		if at.Workflow != "" && g.Scope.Workflow == "" {
			inherited++
		}
		if g.Deny != "" {
			denies++
		}
	}
	count := counting(len(m.grants), "grant")
	if at.Workflow != "" {
		count += " · " + fmt.Sprint(inherited) + " from the namespace"
	}
	if denies == 1 {
		count += " · 1 deny"
	} else if denies > 1 {
		count += " · " + fmt.Sprint(denies) + " denies"
	}
	lines := []string{line(fitted([]part{{strong, "Grants"}, {muted, " as written on "}, {plain, m.scope}}, []part{{muted, count}}, width)...)}
	shown := m.shownGrants()
	if m.grantsFiltering || m.grantsFilter != "" {
		left := []part{{strong, "/ "}, {plain, m.grantsFilter}}
		if m.grantsFilter == "" {
			left = []part{{strong, "/ "}, {quiet, "filter by principal, role, permission or scope"}}
		}
		if m.grantsFiltering {
			left = append(left, part{strong, "▏"})
		}
		lines = append(lines, line(fitted(left, []part{{muted, fmt.Sprint(len(shown)) + " of " + counting(len(m.grants), "grant")}}, width)...))
	}
	switch {
	case len(m.grants) == 0:
		lines = append(lines, line(part{quiet, "No grant is written on " + m.scope + "."}))
	case len(shown) == 0:
		lines = append(lines, line(part{quiet, "No grant matches the filter."}))
	default:
		h := func(s string) []part { return []part{{quiet, s}} }
		lines = append(lines, line(m.grantColumns(width, h("PRINCIPAL"), h("ROLE OR DENY"), h("SCOPE"), h("EXPIRES"), h("GRANTED"))...))
		chosen := m.chosenGrant()
		// A list longer than half the room is cut around the grant chosen, so that the roles' table
		// keeps a place beneath it.
		rows := max(1, min(len(shown), room/2))
		first := min(max(0, chosen-rows+1), max(0, len(shown)-rows))
		now := m.o.Now()
		for i := first; i < first+rows && i < len(shown); i++ {
			g := shown[i]
			gives := []part{{plain, string(g.Role)}}
			if g.Deny != "" {
				gives = []part{{failedText, "deny " + string(g.Deny)}}
			}
			scope := []part{{plain, g.Scope.String()}}
			if at.Workflow != "" && g.Scope.Workflow == "" {
				scope = append(scope, part{quiet, " inherited"})
			}
			expires := []part{{muted, "never"}}
			if g.ExpiresAt != nil {
				expires = []part{{plain, day(*g.ExpiresAt)}}
			}
			t.pick(len(lines), width, "grant", g.ID)
			lines = append(lines, t.line(i == chosen, width, m.grantColumns(width, []part{{plain, g.Principal}}, gives, scope, expires, []part{{muted, g.GrantedBy + " " + clock(g.GrantedAt, now)}})...))
		}
		if len(shown) > rows {
			lines = append(lines, line(part{quiet, "and " + counting(len(shown)-rows, "more grant") + ": ↑↓ moves over them"}))
		}
	}
	lines = append(lines, t.said(quiet, "A namespace grant reaches every workflow inside; a deny names one permission, never a role.", width)...)
	if len(lines)+8 > room {
		return append(lines, line(), line(part{quiet, "read only: agk share and the web console change grants"}))
	}

	lines = append(lines, line(), line(part{strong, "What each role carries"}))
	head := []part{{quiet, cellOf("", 10)}}
	for _, c := range roleColumns {
		head = append(head, part{quiet, cellOf(c.name, 8)})
	}
	lines = append(lines, line(head...))
	for _, r := range access.Roles {
		row := []part{{plain, cellOf(string(r), 10)}}
		for _, c := range roleColumns {
			if carried(r, c.permissions) {
				row = append(row, part{succeededText, cellOf("●", 8)})
			} else {
				row = append(row, part{quiet, cellOf("·", 8)})
			}
		}
		lines = append(lines, line(row...))
	}
	var legend []string
	for _, c := range roleColumns {
		var names []string
		for _, p := range c.permissions {
			names = append(names, string(p))
		}
		legend = append(legend, c.name+" "+strings.Join(names, ", "))
	}
	for _, l := range packed(legend, width) {
		lines = append(lines, line(part{muted, l}))
	}
	lines = append(lines, t.said(muted, "operator runs and follows runs without workflow:read: the queries inside stay out of reach.", width)...)
	if len(lines)+len(neverShown)+4 <= room {
		lines = append(lines, line(), line(part{strong, "Never shown, to anyone"}))
		for _, s := range neverShown {
			for i, l := range folded(s, width-2) {
				mark := "  "
				if i == 0 {
					mark = "· "
				}
				lines = append(lines, line(part{quiet, mark}, part{plain, l}))
			}
		}
	}
	return append(lines, line(), line(part{quiet, "read only: agk share and the web console change grants"}))
}

// resolvedLines are the grants resolved for one principal: each of the nine permissions held,
// taken away or not given, and the grants that make it so.
func (m Model) resolvedLines(t theme, width int) []string {
	line := func(parts ...part) string { return t.line(false, width, within(parts, width)...) }
	whom := m.whomOf()
	about := kindOf(whom)
	groups, known := m.groupsOf(whom)
	switch {
	case !known:
		about += " · groups an administrator's to read"
	case len(groups) > 0:
		named := make([]string, len(groups))
		for i, g := range groups {
			named[i] = "group:" + g
		}
		about += " · member of " + strings.Join(named, ", ")
	}
	you := ""
	if whom == m.me.Principal {
		you = " (you)"
	}
	lines := []string{line(fitted([]part{{strong, "Resolved"}, {muted, " for "}, {plain, whom + you}}, []part{{muted, about}}, width)...)}
	resolved, err := m.resolved(whom, m.o.Now())
	if err != nil {
		return append(lines, line(part{failedText, "The grants could not be resolved: " + err.Error()}))
	}
	both := "the namespace"
	if scopeOf(m.scope).Workflow != "" {
		both = "both scopes"
	}
	lines = append(append(lines, t.said(muted, "Every grant that applies, its own and its groups', at "+both+": their union, then each deny taken away.", width)...), line())
	maybe := false
	for _, l := range resolved {
		sign, role := " ", quiet
		var why []string
		switch {
		case len(l.takes) > 0:
			sign, role = "−", failedText
			for _, g := range l.takes {
				why = append(why, "denied at "+g.Scope.String())
			}
			if len(l.gives) > 0 {
				var over []string
				for _, g := range l.gives {
					over = append(over, string(g.Role))
				}
				why = append(why, strings.Join(over, ", ")+" holds it; the deny wins")
			}
		case l.held:
			sign, role = "+", succeededText
			for _, g := range l.gives {
				why = append(why, source(g, whom))
			}
		case len(l.members) > 0:
			maybe = true
			for _, g := range l.members {
				why = append(why, "to a member: "+source(g, whom))
			}
		default:
			why = []string{m.unheld(l.permission)}
		}
		name := plain
		if !l.held {
			name = muted
		}
		first := []part{{role, sign + " "}, {name, cellOf(string(l.permission), 18)}}
		room := max(12, width-20)
		for i, w := range wrapped(why, room) {
			if i == 0 {
				lines = append(lines, line(append(first, part{muted, w})...))
				continue
			}
			lines = append(lines, line(part{plain, strings.Repeat(" ", 20)}, part{muted, w}))
		}
	}
	lines = append(append(lines, line()), t.said(quiet, "Grants add up across both scopes; a deny is the only subtraction, and wins over any allow wherever it is set.", width)...)
	if maybe {
		lines = append(lines, t.said(quiet, "Who is in a group is an administrator's to read, so whether "+whom+" is in one of them is not said here.", width)...)
	}
	return lines
}

// wrapped joins what is said with semicolons into as few lines as the width holds, a line broken
// between two things said rather than inside one, and inside one between words where it is wider
// than a line.
func wrapped(said []string, width int) []string {
	var lines []string
	for _, s := range said {
		if n := len(lines); n > 0 && len([]rune(lines[n-1]))+2+len([]rune(s)) <= width {
			lines[n-1] += "; " + s
			continue
		}
		lines = append(lines, folded(s, width)...)
	}
	return lines
}

// folded is a sentence broken between words into lines no wider than width.
func folded(sentence string, width int) []string {
	var lines []string
	for _, w := range strings.Fields(sentence) {
		if n := len(lines); n > 0 && len([]rune(lines[n-1]))+1+len([]rune(w)) <= width {
			lines[n-1] += " " + w
			continue
		}
		lines = append(lines, w)
	}
	return lines
}

// said is a sentence as lines of one role, broken between words to the width.
func (t theme) said(r role, sentence string, width int) []string {
	var lines []string
	for _, l := range folded(sentence, width) {
		lines = append(lines, t.line(false, width, within([]part{{r, l}}, width)...))
	}
	return lines
}

// shownGrants are the grants the filter's words leave, in the order they are written: each word is
// found whole in the grant's principal, what it gives or takes away, or its scope. Whole rather
// than letter by letter as the runs are matched, since a short list of names is where letters
// apart match nearly everything: alice is in finance/deploy-bot. The filter narrows the list and
// nothing else: the grants are resolved, and counted, whole.
func (m Model) shownGrants() []access.Grant {
	words := strings.Fields(m.grantsFilter)
	if len(words) == 0 {
		return m.grants
	}
	var shown []access.Grant
	for _, g := range m.grants {
		text := strings.ToLower(g.Principal + " " + string(g.Role) + " " + string(g.Deny) + " " + g.Scope.String())
		if g.Deny != "" {
			text += " deny"
		}
		if !slices.ContainsFunc(words, func(w string) bool { return !strings.Contains(text, strings.ToLower(w)) }) {
			shown = append(shown, g)
		}
	}
	return shown
}

// chosenGrant is the index of the grant chosen among those shown, the first where none is.
func (m Model) chosenGrant() int {
	if i := slices.IndexFunc(m.shownGrants(), func(g access.Grant) bool { return g.ID == m.grant }); i >= 0 {
		return i
	}
	return 0
}

// typingGrants takes a key while the grants' filter line is open: a character narrows the list,
// ↑ ↓ move over what it leaves, enter keeps it, esc clears it.
func (m Model) typingGrants(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch key := msg.String(); key {
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		m.grantsFiltering = false
		return m, nil
	case "esc":
		m.grantsFiltering, m.grantsFilter = false, ""
		return m, nil
	case "up", "down":
		return m.sharingPress(key)
	case "backspace":
		if r := []rune(m.grantsFilter); len(r) > 0 {
			m.grantsFilter = string(r[:len(r)-1])
		}
		return m, nil
	}
	m.grantsFilter += msg.Text
	return m, nil
}

// sharingLines is the sharing view: the grants as written beside what they come to for one
// principal where the window is wide enough for both, the one above the other where it is not.
func (m Model) sharingLines(t theme, height int) []string {
	line := func(parts ...part) string { return t.line(false, m.width, within(parts, m.width)...) }
	switch {
	case m.scope == "":
		return append([]string{line(part{strong, "Sharing"}), line()},
			t.said(quiet, "Nothing to list: the grants of a namespace or a workflow are listed to whoever holds grant:manage there, as agk grants lists them, and you hold it nowhere.", m.width)...)
	case m.grantsFor != m.scope && m.grantsFailed != "":
		return []string{line(part{strong, "Sharing "}, part{plain, m.scope}), line(), line(part{failedText, "The grants could not be read: " + m.grantsFailed})}
	case m.grantsFor != m.scope:
		return []string{line(part{strong, "Sharing "}, part{plain, m.scope}), line(), line(part{quiet, "Reading the grants of " + m.scope + "."})}
	}
	if m.width < 150 {
		resolved := m.resolvedLines(t, m.width)
		grants := m.grantsLines(t, m.width, max(8, height-len(resolved)-1))
		return append(append(grants, line()), resolved...)
	}
	left := m.width * 11 / 20
	right := m.width - left - 3
	grants, resolved := m.grantsLines(t, left, height), m.resolvedLines(t.at(left+3, 0), right)
	gap := t.line(false, 3)
	var lines []string
	for i := 0; i < max(len(grants), len(resolved)); i++ {
		l, r := t.line(false, left), t.line(false, right)
		if i < len(grants) {
			l = grants[i]
		}
		if i < len(resolved) {
			r = resolved[i]
		}
		lines = append(lines, l+gap+r)
	}
	return lines
}

// whomCommands are the principals the grants can be resolved for, which enter chooses among.
func (m Model) whomCommands() []command {
	var out []command
	for _, p := range m.principalsOf() {
		label := p
		if p == m.me.Principal {
			label += " (you)"
		}
		out = append(out, command{label: label, kind: kindOf(p), act: func(m Model) (tea.Model, tea.Cmd) {
			m.whom = p
			return m, nil
		}})
	}
	return out
}

// scopeCommands are the scopes whose grants the principal may list, which s chooses among.
func (m Model) scopeCommands() []command {
	var out []command
	for _, s := range m.managed() {
		kind := "namespace"
		if strings.Contains(s, "/") {
			kind = "workflow"
		}
		out = append(out, command{label: s, kind: kind, act: func(m Model) (tea.Model, tea.Cmd) {
			if m.scope == s {
				return m, nil
			}
			m.scope, m.grant = s, ""
			return m.showing(sharingView)
		}})
	}
	return out
}

// sharingPress does what a key names in the sharing view.
func (m Model) sharingPress(key string) (tea.Model, tea.Cmd) {
	shown := m.shownGrants()
	switch key {
	case "esc":
		if m.grantsFilter != "" {
			m.grantsFilter = ""
			return m, nil
		}
		return m.showing(runsView)
	case "/":
		m.grantsFiltering = true
	case "up", "k", "down", "j":
		if len(shown) > 0 {
			i := m.chosenGrant()
			if key == "up" || key == "k" {
				i = max(0, i-1)
			} else {
				i = min(len(shown)-1, i+1)
			}
			m.grant = shown[i].ID
		}
	case "enter":
		if m.grantsFor == m.scope && m.scope != "" {
			chosen := 0
			if len(shown) > 0 {
				chosen = max(0, slices.Index(m.principalsOf(), shown[m.chosenGrant()].Principal))
			}
			m.palette = &palette{chosen: chosen, among: Model.whomCommands, prompt: "resolve for: ", none: "Nobody to resolve for."}
		}
	case "s":
		m.palette = &palette{chosen: max(0, slices.Index(m.managed(), m.scope)), among: Model.scopeCommands, prompt: "scope: ", none: "No scope whose grants you may list."}
	}
	return m, nil
}

// sharingKeys are the key line of the sharing view.
func (m Model) sharingKeys() [][2]string {
	if m.grantsFiltering {
		return [][2]string{{"type", "Filter"}, {"↑↓", "Move"}, {"enter", "Keep"}, {"esc", "Clear"}}
	}
	keys := [][2]string{}
	if len(m.grants) > 0 {
		keys = append(keys, [2]string{"↑↓", "Grant"})
	}
	if m.grantsFor == m.scope && m.scope != "" {
		keys = append(keys, [2]string{"enter", "Resolve for…"})
	}
	keys = append(keys, [2]string{"s", "Scope"}, [2]string{"/", "Filter"}, [2]string{":", "Commands"})
	if m.grantsFilter != "" {
		keys = append(keys, [2]string{"esc", "Clear"})
	} else {
		keys = append(keys, [2]string{"esc", "Runs"})
	}
	return append(keys, [2]string{"q", "Quit"}, [2]string{"?", "Every key"})
}
