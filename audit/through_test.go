package audit

import (
	"context"
	"maps"
	"testing"
)

// An act an MCP tool asked for is recorded naming it, beside what its act writes, and an act no tool
// asked for as it is written; a detail that already says so is kept, and the record handed in is
// never written into.
func TestAnActAToolAskedForNamesTheTool(t *testing.T) {
	r := Record{Actor: "alice", Action: "grant.create", Target: "finance", Result: Done, Detail: map[string]any{"role": "viewer"}}
	if got := Marked(context.Background(), r); !maps.Equal(got.Detail, r.Detail) {
		t.Errorf("an act no tool asked for is recorded with %v", got.Detail)
	}
	got := Marked(Through(context.Background(), "grant_create"), r)
	if want := map[string]any{"role": "viewer", "through": "mcp", "tool": "grant_create"}; !maps.Equal(got.Detail, want) {
		t.Errorf("an act grant_create asked for is recorded with %v, want %v", got.Detail, want)
	}
	if len(r.Detail) != 1 {
		t.Errorf("the record handed in was written into: %v", r.Detail)
	}
	said := Record{Actor: "alice", Action: "ref.update", Target: "finance/w", Result: Done, Detail: map[string]any{"through": "mcp", "tool": "workflow_commit"}}
	if got := Marked(Through(context.Background(), "resources/read"), said); got.Detail["tool"] != "workflow_commit" {
		t.Errorf("a detail naming its tool was recorded naming %v", got.Detail["tool"])
	}
	if got := Marked(Through(context.Background(), "run_cancel"), Record{Actor: "alice", Action: "run.cancel", Target: "r", Result: Done}); got.Detail["tool"] != "run_cancel" {
		t.Errorf("an act with no detail was recorded with %v", got.Detail)
	}
}
