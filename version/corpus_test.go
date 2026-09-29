package version_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/fixtures"
	"github.com/agentiik/agentiik/version"
)

// TestTheRepositoryCorpus runs every repository case of the released corpus through Check, as a
// pre-receive hook runs the commit at a pushed tip: a valid case resolves to exactly the graph its
// expected.json writes, and an invalid one meets exactly the refusal its refusal.json names, by
// rule, file, line and column.
//
// Each case holds what the installation holds as stand-ins, the repository's pins, manifests and
// secret declarations, the versions already recorded and the other repositories an include reads,
// and they reach Check as the resolvers the hook will give it, so that the case and the hook read
// the same stores. "Permissions are not part of a case": the pusher holds whatever the push needs.
func TestTheRepositoryCorpus(t *testing.T) {
	cases, err := fixtures.Repositories()
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("the corpus holds no repository case")
	}
	resolvedGraph := wireSchema(t, "resolvedGraph")
	for _, c := range cases {
		t.Run(path.Base(c.File), func(t *testing.T) {
			checked, err := version.Check(t.Context(), treeOf(t, c.File), checkingOf(t, c.File))
			if c.Valid {
				if err != nil {
					t.Fatalf("the corpus says this case covers %s, and it was refused: %v", c.Covers, err)
				}
				got, err := checked.Resolved()
				if err != nil {
					t.Fatal(err)
				}
				if err := validates(resolvedGraph, got); err != nil {
					t.Fatalf("the resolved graph is not one the wire carries: %v\n%s", err, got)
				}
				want := readFile(t, c.File+"/expected.json")
				if !sameJSON(t, got, want) {
					t.Fatalf("the case resolves to\n%s\nand the corpus expects\n%s", indent(t, got), want)
				}
				return
			}

			var want struct {
				Rule   graph.Rule `json:"rule"`
				File   string     `json:"file"`
				Line   int        `json:"line"`
				Column int        `json:"column"`
			}
			if err := json.Unmarshal(readFile(t, c.File+"/refusal.json"), &want); err != nil {
				t.Fatal(err)
			}
			if err == nil {
				t.Fatalf("this case was accepted, and the corpus refuses it by %s", want.Rule)
			}
			var r *graph.Refusal
			if !errors.As(err, &r) {
				t.Fatalf("this case was refused without a rule, and the corpus refuses it by %s: %v", want.Rule, err)
			}
			got := graph.Position{File: listed(r.At.File), Line: r.At.Line, Column: r.At.Column}
			if r.Rule != want.Rule || got != (graph.Position{File: want.File, Line: want.Line, Column: want.Column}) {
				t.Fatalf("this case was refused by %s at %+v, and the corpus refuses it by %s at %s:%d:%d: %v", r.Rule, got, want.Rule, want.File, want.Line, want.Column, err)
			}
		})
	}
}

// aCase is case.json.
type aCase struct {
	Namespace    string            `json:"namespace"`
	Repository   string            `json:"repository"`
	Commit       string            `json:"commit"`
	Versions     []string          `json:"versions"`
	Pins         map[string]string `json:"pins"`
	Manifests    map[string]string `json:"manifests"`
	Secrets      []string          `json:"secrets"`
	Repositories map[string]struct {
		Refs  map[string]string `json:"refs"`
		Trees map[string]string `json:"trees"`
	} `json:"repositories"`
}

// checkingOf is what a hook is told beside a case's tree: the repository and the commit, and the
// installation's stores, built from the case's stand-ins and nothing else.
func checkingOf(t *testing.T, dir string) version.Checking {
	t.Helper()
	var c aCase
	if err := json.Unmarshal(readFile(t, dir+"/case.json"), &c); err != nil {
		t.Fatal(err)
	}
	return version.Checking{
		Commit: c.Commit, Committed: true,
		Namespace: c.Namespace, Repository: c.Repository,
		Stored: slices.Contains(c.Versions, c.Commit),
		Resolvers: version.Resolvers{
			Pin: func(_ context.Context, reference string, _ agk.Step) (string, error) {
				if digest, ok := c.Pins[reference]; ok {
					return digest, nil
				}
				return "", version.ErrNotHeld
			},
			Manifest: func(_ context.Context, image string, _ agk.Step) ([]byte, error) {
				file, ok := c.Manifests[image]
				if !ok {
					return nil, version.ErrNotHeld
				}
				return readFile(t, dir+"/"+file), nil
			},
			// "An include's ref is read as a tag of that name, or as a commit a tree below is
			// given for."
			Include: func(_ context.Context, ref graph.WorkflowRef) (fs.FS, string, error) {
				repository, ok := c.Repositories[ref.Namespace+"/"+ref.Name]
				if !ok {
					return nil, "", fmt.Errorf("the case holds no repository %s/%s", ref.Namespace, ref.Name)
				}
				commit, tagged := repository.Refs["refs/tags/"+ref.Ref]
				if !tagged && len(ref.Ref) >= 7 {
					for held := range repository.Trees {
						if strings.HasPrefix(held, ref.Ref) {
							commit = held
						}
					}
				}
				at, ok := repository.Trees[commit]
				if !ok {
					return nil, "", fmt.Errorf("%s/%s holds no ref %s", ref.Namespace, ref.Name, ref.Ref)
				}
				sub, err := fs.Sub(fixtures.FS, dir+"/"+at)
				return sub, commit, err
			},
			Secrets:   func(context.Context) ([]string, error) { return c.Secrets, nil },
			SecretUse: func(context.Context, []string) (bool, error) { return true, nil },
		},
	}
}

// treeOf is a case's pushed tree: tree/, each file with mode 100644, and the entries tree.json
// lists, which cannot be files there. A symbolic link and a submodule are what a tree read out
// of git reports them as, fs.ModeSymlink and fs.ModeIrregular.
func treeOf(t *testing.T, dir string) fstest.MapFS {
	t.Helper()
	files := fstest.MapFS{}
	sub, err := fs.Sub(fixtures.FS, dir+"/tree")
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.WalkDir(sub, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(sub, name)
		files[name] = &fstest.MapFile{Data: body, Mode: 0o644}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	listing, err := fs.ReadFile(fixtures.FS, dir+"/tree.json")
	if errors.Is(err, fs.ErrNotExist) {
		return files
	}
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		Path, Mode, Target, Commit, Content string
	}
	if err := json.Unmarshal(listing, &entries); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		f := &fstest.MapFile{}
		switch e.Mode {
		case "100644":
			f.Data, f.Mode = []byte(e.Content), 0o644
		case "100755":
			f.Data, f.Mode = []byte(e.Content), 0o755
		case "120000":
			f.Data, f.Mode = []byte(unlisted(t, e.Target)), fs.ModeSymlink|0o777
		case "160000":
			f.Mode = fs.ModeIrregular
		default:
			t.Fatalf("tree.json lists %s with mode %s", e.Path, e.Mode)
		}
		files[unlisted(t, e.Path)] = f
	}
	return files
}

// unlisted is a path as tree.json writes it, as the bytes it stands for: "a printable ASCII
// character other than % as itself, and any other byte as % and two uppercase hexadecimal
// digits".
func unlisted(t *testing.T, s string) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		n, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
		if err != nil {
			t.Fatalf("tree.json writes %q", s)
		}
		b.WriteByte(byte(n))
		i += 2
	}
	return b.String()
}

// listed is a path as refusal.json writes it, which is how tree.json writes one.
func listed(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 0x20 && c <= 0x7e && c != '%' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", s[i])
	}
	return b.String()
}

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := fs.ReadFile(fixtures.FS, name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// sameJSON is whether two documents say the same thing, whatever order their keys are in.
func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(x, y)
}

func indent(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := json.Indent(&out, b, "", "  "); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// wireSchema compiles one definition of the vendored wire.schema.json, pinned to 2020-12 rather
// than left to be guessed from a $schema the compiler would then go and fetch.
func wireSchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	doc, err := fixtures.Wire()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource("wire.schema.json", raw); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("wire.schema.json#/$defs/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func validates(s *jsonschema.Schema, body []byte) error {
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		return err
	}
	return s.Validate(v)
}
