package version_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/version"
)

const commonCommit = "c41d9e2a7b3f5e8d1c0a9b6e4f2d8c7a5b3e1f09"

// commonLibrary is finance/common at v2.1.0, a library whose root includes a file of its own tree.
func commonLibrary() fstest.MapFS {
	return fstest.MapFS{
		"agentiik.yaml": &fstest.MapFile{Data: []byte("include:\n  - path: ./blocks/api.yaml\n")},
		"blocks/api.yaml": &fstest.MapFile{Data: []byte(`.api-brick:
  timeout: 2m
  image: alpine@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
`)},
		"README.md": &fstest.MapFile{Data: []byte("never read\n")},
	}
}

// reaching answers finance/common at v2.1.0 and nothing else, counting what it was asked.
func reaching(asked *int) func(context.Context, graph.WorkflowRef) (fs.FS, string, error) {
	return func(_ context.Context, ref graph.WorkflowRef) (fs.FS, string, error) {
		*asked++
		if ref.Namespace != "finance" || ref.Name != "common" || ref.Ref != "v2.1.0" {
			return nil, "", fmt.Errorf("no library %s", ref)
		}
		return commonLibrary(), commonCommit, nil
	}
}

// "A library repository, which its own hook validates as a fragment and nothing runs": its commit
// is checked as one, recorded as a library with the files it read, and never rebuilt into a graph.
func TestALibraryIsCheckedAsAFragmentAndNeverBuilt(t *testing.T) {
	asked := 0
	checked, err := version.Check(t.Context(), commonLibrary(), version.Checking{
		Commit: commonCommit, Committed: true, Namespace: "finance", Repository: "common",
		Resolvers: version.Resolvers{Include: reaching(&asked)},
	})
	if err != nil {
		t.Fatal(err)
	}
	v := checked.Version
	switch {
	case !checked.Library || !v.Library || checked.Workflow != nil || checked.Graph != nil:
		t.Errorf("the library was checked as %+v", checked)
	case v.Entry != "agentiik.yaml" || string(v.Document) != "include:\n  - path: ./blocks/api.yaml\n":
		t.Errorf("the library's root was recorded as %s: %q", v.Entry, v.Document)
	case len(v.Includes) != 1 || v.Includes["blocks/api.yaml"] == nil:
		t.Errorf("the library recorded the files %v", keys(v.Includes))
	case !slices.Equal(checked.Included, []graph.Included{{Path: "blocks/api.yaml"}}):
		t.Errorf("the library included %+v", checked.Included)
	}
	v.Workflow, v.Commit = "common", commonCommit
	if _, err := version.Build(v); !errors.Is(err, version.ErrLibrary) {
		t.Errorf("a library was built: %v", err)
	}

	// A fragment's rules are a library's.
	broken := commonLibrary()
	broken["agentiik.yaml"] = &fstest.MapFile{Data: []byte("on:\n  manual: {}\n")}
	if _, err := version.Check(t.Context(), broken, version.Checking{}); err == nil {
		t.Error("a library declaring a trigger was accepted")
	}
}

// "The run records the commit it resolved to": a version including a library keeps the commit its
// ref resolved to and every file of the library resolution read, and is rebuilt from them with
// nothing in reach, so that the library moving its tag or going changes nothing a run of it does.
func TestAVersionKeepsWhatItsWorkflowIncludesRead(t *testing.T) {
	tree := fstest.MapFS{"agentiik.yaml": &fstest.MapFile{Data: []byte(`apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing }
include:
  - workflow: finance/common
    ref: v2.1.0
steps:
  invoice:
    extends: .api-brick
    script: ["./invoice.sh"]
    outputs: [ok]
`)}}
	asked := 0
	checked, err := version.Check(t.Context(), tree, version.Checking{Resolvers: version.Resolvers{Include: reaching(&asked)}})
	if err != nil {
		t.Fatal(err)
	}
	v := checked.Version
	kept, ok := v.Libraries["finance/common@v2.1.0"]
	switch {
	case asked != 1 || len(v.Libraries) != 1 || !ok:
		t.Fatalf("asked %d times, and the version keeps %v", asked, v.Libraries)
	case kept.Commit != commonCommit || len(kept.Files) != 2 || kept.Files["agentiik.yaml"] == nil || kept.Files["blocks/api.yaml"] == nil:
		t.Errorf("the version keeps %s and %d files of the library", kept.Commit, len(kept.Files))
	case len(v.Includes) != 0:
		t.Errorf("a library's files were kept as files of the tree: %v", keys(v.Includes))
	}
	if want := []graph.Included{{Workflow: graph.WorkflowRef{Namespace: "finance", Name: "common", Ref: "v2.1.0"}, Commit: commonCommit}}; !slices.Equal(checked.Included, want) {
		t.Errorf("the workflow included %+v", checked.Included)
	}

	v.Workflow, v.Commit = "monthly-invoicing", "a3f9c1e04b7d2e8f6a1c3b5d7e9f0a2b4c6d8e0f"
	g, err := version.Build(v)
	if err != nil {
		t.Fatalf("the version does not rebuild from what it kept: %v", err)
	}
	resolved, err := g.Resolved(v.Commit)
	if err != nil || !strings.Contains(string(resolved), `"commit":"`+commonCommit+`"`) {
		t.Errorf("the graph records %s: %v", resolved, err)
	}

	// A version that kept nothing of an include it names is refused, rather than rebuilt from
	// whatever the library holds now.
	delete(v.Libraries, "finance/common@v2.1.0")
	if _, err := version.Build(v); err == nil || !strings.Contains(err.Error(), "kept nothing of it") {
		t.Errorf("a version that kept nothing of its include rebuilt: %v", err)
	}
}

// A library's files are read within the budget the tree including it leaves, every file of every
// tree together, and refused past it.
func TestALibrarysFilesAreReadWithinTheBudget(t *testing.T) {
	var read int64
	tree := fstest.MapFS{"agentiik.yaml": &fstest.MapFile{Data: []byte(`apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing }
include:
  - workflow: finance/common
    ref: v2.1.0
steps:
  invoice:
    image: ghcr.io/acme/agk-invoice@sha256:8214cabcb148ac56e69ee08e1554684f7609d08b58c4527938fdcf400be68595
    outputs: [ok]
`)}}
	_, err := version.Check(t.Context(), tree, version.Checking{Resolvers: version.Resolvers{
		Include: func(context.Context, graph.WorkflowRef) (fs.FS, string, error) {
			return endless{size: 10, read: &read}, commonCommit, nil
		},
	}})
	if err == nil || !strings.Contains(err.Error(), "past 16777216 bytes") {
		t.Errorf("a library that never ends is answered %v", err)
	}
	if read > version.ReadMaxBytes+64<<10 {
		t.Errorf("%d bytes of it were read", read)
	}
}
