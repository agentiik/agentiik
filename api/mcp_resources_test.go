package api_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
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

// A run's files are read through MCP by the URI their envelope names them by, with run.artifact or
// as a resource, under run:read_data as the route: text as text, an image as an image and anything
// else as an embedded resource, the bytes answered whatever the budget and never a presigned URL,
// since "no credential crosses MCP". A budget is spent as any fetch spends it, a file above the
// ceiling is refused with its size before anything is held, and a caller holding run:read alone is
// refused either read as for a file never written.
func TestARunsFilesAreReadThroughMCP(t *testing.T) {
	s := withSomeRuns(t)
	run := s.finance[0]
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	pdf := []byte("%PDF-1.7\n\x00\xff binary")
	text := s.aTypedArtifact(t, run, "summary.txt", "text/plain; charset=utf-8", 0, []byte("charged 2, rejected 1"))
	image := s.aTypedArtifact(t, run, "chart.png", "image/png", 0, png)
	document := s.aTypedArtifact(t, run, "invoice.pdf", "application/pdf", 0, pdf)
	once := s.aTypedArtifact(t, run, "payslip.txt", "text/plain", 1, []byte("payslip 2026-01"))
	large := s.aTypedArtifact(t, run, "dump.txt", "text/plain", 0, bytes.Repeat([]byte("a"), 1<<20+1))
	invoicing := api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}
	rt := s.servedTo(t, owningGrants{granted: granted{
		"alice": {{api.RunReadData, invoicing}},
		"dave":  {{api.RunRead, invoicing}},
	}}).(*api.Router)
	if _, err := api.NewMCP(rt, api.MCPOptions{PublicURL: "https://agentiik.example.com"}); err != nil {
		t.Fatal(err)
	}
	read := func(as string, u agk.URI) map[string]any {
		t.Helper()
		_, res, rpcErr := called(t, rt, as, "tools/call", map[string]any{"name": "run.artifact", "arguments": map[string]any{"uri": u.String()}})
		if rpcErr != nil {
			t.Fatalf("run.artifact of %s answered %v", u, rpcErr)
		}
		return res
	}
	block := func(res map[string]any) map[string]any { return res["content"].([]any)[0].(map[string]any) }

	if b := block(read("alice", text)); b["type"] != "text" || b["text"] != "charged 2, rejected 1" {
		t.Errorf("a text file is answered %v", b)
	}
	if b := block(read("alice", image)); b["type"] != "image" || b["mimeType"] != "image/png" || b["data"] != base64.StdEncoding.EncodeToString(png) || b["text"] != nil {
		t.Errorf("an image is answered %v", b)
	}
	b := block(read("alice", document))
	r, _ := b["resource"].(map[string]any)
	if b["type"] != "resource" || r["uri"] != document.String() || r["mimeType"] != "application/pdf" || r["blob"] != base64.StdEncoding.EncodeToString(pdf) || r["text"] != nil {
		t.Errorf("a PDF is answered %v", b)
	}

	if res := read("alice", once); res["isError"] == true || block(res)["text"] != "payslip 2026-01" {
		t.Errorf("a file with one fetch left is answered %v", res)
	}
	if res := read("alice", once); res["isError"] != true || !strings.Contains(block(res)["text"].(string), "finished") {
		t.Errorf("a file whose one fetch went is answered %v", res)
	}
	if res := read("alice", large); res["isError"] != true || !strings.Contains(block(res)["text"].(string), "1048577 bytes") {
		t.Errorf("a file above the ceiling is answered %v", res)
	}

	_, res, rpcErr := called(t, rt, "alice", "resources/read", map[string]any{"uri": text.String()})
	if rpcErr != nil {
		t.Fatalf("a run's file is read as a resource as %v", rpcErr)
	}
	if c := res["contents"].([]any)[0].(map[string]any); c["uri"] != text.String() || c["mimeType"] != "text/plain; charset=utf-8" || c["text"] != "charged 2, rejected 1" {
		t.Errorf("a run's file is read as %v", c)
	}
	_, res, _ = called(t, rt, "alice", "resources/templates/list", nil)
	if !strings.Contains(fmt.Sprint(res["resourceTemplates"]), "agk://run/{run}/{step}/{port}/{name}") {
		t.Errorf("alice is listed the templates %v", res["resourceTemplates"])
	}

	if res := read("dave", text); res["isError"] != true || block(res)["text"] != block(read("alice", agk.URI{Run: text.Run, Step: "archive", Port: "ok", Name: "nothing.txt"}))["text"] {
		t.Errorf("dave, holding run:read alone, is answered %v", res)
	}
	_, _, hidden := called(t, rt, "dave", "resources/read", map[string]any{"uri": text.String()})
	nobody := agk.URI{Run: text.Run, Step: "archive", Port: "ok", Name: "nothing.txt"}.String()
	_, _, absent := called(t, rt, "alice", "resources/read", map[string]any{"uri": nobody})
	if hidden == nil || absent == nil || strings.ReplaceAll(hidden.Message, text.String(), "") != strings.ReplaceAll(absent.Message, nobody, "") {
		t.Errorf("a file out of reach is refused with %v, and one never written with %v", hidden, absent)
	}
}
