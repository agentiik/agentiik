package audit

import "context"

// "Record MCP commits, grants, secret declarations, run creations and cancellations in the audit log
// beside API ones, naming principal and tool." A tool of an MCP server acts through the routes the
// API serves, as its caller, so the entry an act writes is the route's; what the log adds is that it
// came through MCP and by which tool, written into every entry of the act rather than by each route,
// since a route that forgot would record a tool's act as one somebody made by hand.

// throughKey is where a context says it carries an act an MCP tool asked for.
type throughKey struct{}

// Through is ctx carrying an act the MCP tool named asked for: every entry recorded under it says
// through: mcp, and the tool, beside what its act writes.
func Through(ctx context.Context, tool string) context.Context {
	return context.WithValue(ctx, throughKey{}, tool)
}

// ThroughOf is the MCP tool an act was asked for by, and false for an act no tool asked for.
func ThroughOf(ctx context.Context) (string, bool) {
	tool, ok := ctx.Value(throughKey{}).(string)
	return tool, ok
}

// Marked is r as recorded under ctx: its detail naming through: mcp and the tool where an MCP tool
// asked for the act and the detail does not already say so, and r as it is otherwise. The detail r
// carries is never written into, since its caller still holds it.
func Marked(ctx context.Context, r Record) Record {
	tool, ok := ThroughOf(ctx)
	if !ok {
		return r
	}
	detail := make(map[string]any, len(r.Detail)+2)
	for k, v := range r.Detail {
		detail[k] = v
	}
	if _, said := detail["through"]; !said {
		detail["through"] = "mcp"
	}
	if _, said := detail["tool"]; !said {
		detail["tool"] = tool
	}
	r.Detail = detail
	return r
}
