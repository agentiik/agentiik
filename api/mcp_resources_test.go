package api_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
)

// A file of a workflow's repository is a resource of the user's server, read as the caller through
// the route workflow.get's reader reads the tree by: text where it is text, its bytes where they are
// not, at a branch, a branch holding a slash and a commit. A workflow nobody let the caller see is
// answered as one that does not exist, and a run as run.get answers it.
func TestAFileOfARepositoryIsAResource(t *testing.T) {
	g := servingGit(t, owningGrants{granted: everyone().(granted)})
	rt := g.h.(*api.Router)
	if _, err := api.NewMCP(rt, api.MCPOptions{PublicURL: "https://agentiik.example.com"}); err != nil {
		t.Fatal(err)
	}
	logo := "\x89PNG\r\n\x1a\n\x00\x00"
	work := g.newClone("alice")
	work.write("agentiik.yaml", workflowDocument)
	work.write("scripts/normalize.sh", "echo normalize\n")
	work.write("logo.png", logo)
	first := work.commit("first")
	work.must("push", "-q", "origin", "main")
	work.must("push", "-q", "origin", "main:release/2026")

	read := func(as, uri string) (map[string]any, string) {
		t.Helper()
		_, res, rpcErr := called(t, rt, as, "resources/read", map[string]any{"uri": uri})
		if rpcErr != nil {
			if rpcErr.Code != -32602 {
				t.Errorf("%s read as %s is refused with %d, where a resource not found is -32602", uri, as, rpcErr.Code)
			}
			return nil, strings.ReplaceAll(rpcErr.Message, uri, "<uri>")
		}
		contents, _ := res["contents"].([]any)
		if len(contents) != 1 {
			t.Fatalf("%s is read as %v", uri, res)
		}
		return contents[0].(map[string]any), ""
	}

	for _, uri := range []string{
		"agentiik://finance/monthly-invoicing/tree/main/agentiik.yaml",
		"agentiik://finance/monthly-invoicing/tree/release%2F2026/agentiik.yaml",
		"agentiik://finance/monthly-invoicing/tree/" + first + "/agentiik.yaml",
	} {
		c, refused := read("bob", uri)
		if refused != "" || c["text"] != workflowDocument || c["mimeType"] != "application/yaml" || c["uri"] != uri {
			t.Errorf("%s is read as %v %s", uri, c, refused)
		}
	}
	for _, uri := range []string{
		"agentiik://finance/monthly-invoicing/tree/main/scripts/normalize.sh",
		"agentiik://finance/monthly-invoicing/tree/main/scripts%2Fnormalize.sh",
	} {
		if c, refused := read("bob", uri); refused != "" || c["text"] != "echo normalize\n" || c["mimeType"] != "text/plain" {
			t.Errorf("%s is read as %v %s", uri, c, refused)
		}
	}
	c, refused := read("bob", "agentiik://finance/monthly-invoicing/tree/main/logo.png")
	if blob, _ := c["blob"].(string); refused != "" || blob != base64.StdEncoding.EncodeToString([]byte(logo)) || c["text"] != nil {
		t.Errorf("a file that is no text is read as %v %s", c, refused)
	}

	// Absent and hidden alike: a stranger reading a workflow they cannot see is told what a reader
	// asking for one that does not exist is told, word for word.
	_, hidden := read("stranger", "agentiik://finance/monthly-invoicing/tree/main/agentiik.yaml")
	_, absent := read("bob", "agentiik://finance/no-such-workflow/tree/main/agentiik.yaml")
	_, nowhere := read("bob", "agentiik://nowhere/monthly-invoicing/tree/main/agentiik.yaml")
	if hidden == "" || hidden != absent || hidden != nowhere {
		t.Errorf("a hidden workflow's file is refused with %q, an absent workflow's with %q and an absent namespace's with %q", hidden, absent, nowhere)
	}
	if _, refused := read("bob", "agentiik://finance/monthly-invoicing/tree/main/missing.txt"); refused == "" {
		t.Error("a file the tree does not hold was read")
	}
	if _, refused := read("bob", "agentiik://finance/monthly-invoicing/tree/main"); refused == "" {
		t.Error("a URI naming no file was read")
	}

	// A run nobody started is no resource.
	if _, refused := read("bob", "agentiik://run/01JMZ8V1P9C4XQ7K2N4D6F8H0A"); refused == "" {
		t.Error("a run nobody started was read")
	}
}

// A run is a resource of the user's server, read as run.get answers it, and one the caller may not
// read is answered word for word as one nobody started.
func TestARunIsAResource(t *testing.T) {
	s := withSomeRuns(t)
	run := s.finance[0]
	rt := s.servedTo(t, owningGrants{granted: granted{
		"alice": {{api.RunRead, api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}}},
		"bob":   {{api.RunRead, api.Target{Namespace: "finance", Workflow: "payroll"}}},
	}}).(*api.Router)
	if _, err := api.NewMCP(rt, api.MCPOptions{PublicURL: "https://agentiik.example.com"}); err != nil {
		t.Fatal(err)
	}
	uri := "agentiik://run/" + run
	_, res, rpcErr := called(t, rt, "alice", "resources/read", map[string]any{"uri": uri})
	if rpcErr != nil {
		t.Fatalf("the run is read as %v", rpcErr)
	}
	c := res["contents"].([]any)[0].(map[string]any)
	w, _ := call(t, rt, "GET", "/api/v1/runs/"+run, "alice", nil)
	if c["mimeType"] != "application/json" || c["text"] != w.Body.String() {
		t.Errorf("the run is read as %v, and run.get's route answers %s", c, w.Body)
	}

	_, _, hidden := called(t, rt, "bob", "resources/read", map[string]any{"uri": uri})
	nobody := "agentiik://run/01M2ZZZZZZZZZZZZZZZZZZZZZZ"
	_, _, absent := called(t, rt, "alice", "resources/read", map[string]any{"uri": nobody})
	if hidden == nil || absent == nil || hidden.Code != absent.Code || strings.ReplaceAll(hidden.Message, uri, "") != strings.ReplaceAll(absent.Message, nobody, "") {
		t.Errorf("a run out of reach is refused with %v, and one nobody started with %v", hidden, absent)
	}
}
