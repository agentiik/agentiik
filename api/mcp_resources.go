package api

import (
	"bytes"
	"context"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/agentiik/agentiik/mcp"
)

// The user's server's resources: "Resources are read-only", a file in a workflow repository and a
// run beside the language's pages and the schema parts, each "under its tool's permission". A read
// is the route its tool makes, made as the caller as a tool's is (mcp_dispatch.go), so that a file
// is read exactly as far as workflow.get reads its workflow and a run exactly as far as run.get
// does; and a read the route refuses is the revision's resource not found, the same whether what it
// names is absent or out of the caller's reach, since the route's 404 is already the same for both.

// treeTemplate, runTemplate and artifactTemplate are the families of resource a caller's grants
// open. A run's file keeps the URI its envelope names it by, agk://run/, so that what run.output
// answers is what a client reads, with nothing to rewrite.
var (
	treeTemplate = mcp.Template{
		URITemplate: "agentiik://{ns}/{name}/tree/{ref}/{path}",
		Name:        "tree",
		Title:       "A file in a workflow repository",
		Description: "A file of a workflow's repository at a branch, a tag or a commit, as workflow.get's reader reads it: text where it is text, its bytes where it is not. A ref holding a slash writes it as %2F.",
	}
	runTemplate = mcp.Template{
		URITemplate: "agentiik://run/{id}",
		Name:        "run",
		Title:       "A run",
		Description: "A run as run.get answers it: its state, each step's verdict, what started it and whom it is attributed to, its inputs only to whoever holds run:read_data.",
		MimeType:    "application/json",
	}
	artifactTemplate = mcp.Template{
		URITemplate: "agk://run/{run}/{step}/{port}/{name}",
		Name:        "artifact",
		Title:       "A file a run wrote",
		Description: "A file a run's step wrote, by the URI an envelope's files name it by, as run.artifact reads it: text where it is text, its bytes where they are not, typed as its envelope declares it. Up to 1 MiB; a file its workflow gave a fetch budget spends one fetch.",
	}
)

// resourceTool is the tool a read is made as, which the request the route serves says it came by.
const resourceTool = "resources/read"

// reader answers resources/read for the caller of r: the language and the schema parts as anybody
// reads them, and a file or a run through its route.
func (m *MCP) reader(r *http.Request) func(context.Context, string) ([]mcp.Contents, error) {
	return func(ctx context.Context, uri string) ([]mcp.Contents, error) {
		if strings.HasPrefix(uri, "agentiik://language/") || strings.HasPrefix(uri, "agentiik://schema/") {
			return readLanguage(ctx, uri)
		}
		if strings.HasPrefix(uri, "agk://run/") {
			// The route parses the URI and reads the run out of it to authorise the read, so a URI
			// that names nothing is answered as one out of reach.
			a, err := m.dispatch(r.WithContext(ctx), resourceTool, "GET", "/api/v1/artifacts/"+segment(uri), nil, nil)
			if err != nil {
				return nil, err
			}
			if err := readable(uri, a); err != nil {
				return nil, err
			}
			return []mcp.Contents{artifactContents(uri, a.header.Get("Content-Type"), a.body.Bytes())}, nil
		}
		rest, ok := strings.CutPrefix(uri, "agentiik://")
		if !ok {
			return nil, unreadable(uri)
		}
		parts := strings.Split(rest, "/")
		switch {
		case len(parts) == 2 && parts[0] == "run":
			// A run's identifier is one segment, which a namespace called run cannot be taken for:
			// a file of one is five segments or more.
			id, err := url.PathUnescape(parts[1])
			if err != nil || id == "" {
				return nil, unreadable(uri)
			}
			a, err := m.dispatch(r.WithContext(ctx), resourceTool, "GET", "/api/v1/runs/"+segment(id), nil, nil)
			if err != nil {
				return nil, err
			}
			if err := readable(uri, a); err != nil {
				return nil, err
			}
			return []mcp.Contents{{URI: uri, MimeType: "application/json", Text: a.body.String()}}, nil
		case len(parts) >= 5 && parts[2] == "tree":
			var named [4]string
			for i, p := range []string{parts[0], parts[1], parts[3], strings.Join(parts[4:], "/")} {
				v, err := url.PathUnescape(p)
				if err != nil || v == "" {
					return nil, unreadable(uri)
				}
				named[i] = v
			}
			ns, name, ref, file := named[0], named[1], named[2], named[3]
			var at []string
			for _, s := range strings.Split(ref, "/") {
				at = append(at, segment(s))
			}
			a, err := m.dispatch(r.WithContext(ctx), resourceTool, "GET", "/api/v1/"+segment(ns)+"/workflows/"+segment(name)+"/tree/"+strings.Join(at, "/"), url.Values{"path": {file}}, nil)
			if err != nil {
				return nil, err
			}
			if err := readable(uri, a); err != nil {
				return nil, err
			}
			return []mcp.Contents{fileContents(uri, file, a.body.Bytes())}, nil
		}
		return nil, unreadable(uri)
	}
}

// unreadable is the revision's answer to a resource nobody may read, absent or out of reach alike.
func unreadable(uri string) *mcp.Error {
	return mcp.InvalidParams("Resource not found: %s", uri)
}

// readable is nil where the route answered what was asked, the route's refusal as resource not
// found, with its sentence, which says the same of what is absent and what is hidden, and a failure
// of the installation as one the client is told only that the read could not be made.
func readable(uri string, a *toolAnswer) error {
	switch {
	case a.status >= 500:
		_, err := result(a)
		return err
	case a.status >= 400:
		r, _ := result(a)
		said := ""
		if r != nil && len(r.Content) > 0 {
			said = r.Content[0].Text
		}
		return mcp.InvalidParams("Resource not found: %s: %s", uri, said)
	}
	return nil
}

// fileContents is a file of a tree as a read answers it: text, typed by its extension where the
// language has a name for it, where the bytes are UTF-8 holding no NUL, and its bytes otherwise.
func fileContents(uri, file string, b []byte) mcp.Contents {
	if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return mcp.Contents{URI: uri, MimeType: "application/octet-stream", Blob: b}
	}
	kind := "text/plain"
	switch path.Ext(file) {
	case ".yaml", ".yml":
		kind = "application/yaml"
	case ".json":
		kind = "application/json"
	case ".md":
		kind = "text/markdown"
	}
	return mcp.Contents{URI: uri, MimeType: kind, Text: string(b)}
}

// artifactContents is a run's file as a read answers it: text where its media type is a kind of text
// and its bytes are UTF-8 holding no NUL, its bytes otherwise, typed as its envelope declares it.
func artifactContents(uri, kind string, b []byte) mcp.Contents {
	if textual(kind) && utf8.Valid(b) && bytes.IndexByte(b, 0) < 0 {
		return mcp.Contents{URI: uri, MimeType: kind, Text: string(b)}
	}
	return mcp.Contents{URI: uri, MimeType: kind, Blob: b}
}

// fileBlock is a run's file as a tool answers it: text as text, an image of a kind a client shows as
// an image, and any other file as the resource it is, embedded with its URI and media type.
func fileBlock(uri, kind string, b []byte) mcp.Content {
	c := artifactContents(uri, kind, b)
	if c.Blob == nil {
		return mcp.Text(c.Text)
	}
	if t, _, err := mime.ParseMediaType(kind); err == nil && shown[t] {
		return mcp.Image(b, t)
	}
	return mcp.Embedded(c)
}

// shown are the images a client of MCP shows, those the clients that take an image block take.
var shown = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// textual says whether a media type is text: text/ anything, JSON, YAML, XML and their suffixes, and
// the few application/ types that are text by another name.
func textual(kind string) bool {
	t, _, err := mime.ParseMediaType(kind)
	if err != nil {
		return false
	}
	switch {
	case strings.HasPrefix(t, "text/"), strings.HasSuffix(t, "+json"), strings.HasSuffix(t, "+xml"), strings.HasSuffix(t, "+yaml"):
		return true
	}
	switch t {
	case "application/json", "application/x-ndjson", "application/yaml", "application/x-yaml", "application/xml", "application/javascript", "application/toml", "application/sql":
		return true
	}
	return false
}
