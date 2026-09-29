package db

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
)

// A library's commit is a version marked as one, and a version including a library keeps what it
// read of it: both come back as they went, the listing says which versions are libraries, and a run
// names the commit each workflow include of its version resolved to.
func TestALibraryAndWhatAnIncludeReadAreKeptWithTheVersion(t *testing.T) {
	pool, super := opened(t)
	ctx := t.Context()
	const library = "c41d9e2a7b3f5e8d1c0a9b6e4f2d8c7a5b3e1f09"
	lib := Version{
		Workflow: "common", Commit: library,
		Entry: "agentiik.yaml", Document: []byte("include:\n  - path: ./blocks/api.yaml\n"),
		Includes: map[string][]byte{"blocks/api.yaml": []byte(".api-brick:\n  timeout: 2m\n")},
		Library:  true, Tree: aTree(), Author: "alice",
	}
	kept := map[string]LibraryFiles{"finance/common@v2.1.0": {Commit: library, Files: map[string][]byte{
		"agentiik.yaml": lib.Document, "blocks/api.yaml": lib.Includes["blocks/api.yaml"],
	}}}
	including := aVersion("b4a0d2f", aTree())
	including.Libraries = kept

	var back, backLib Version
	var listed map[string]Listed
	if err := pool.In(ctx, "finance", func(ctx context.Context, ns *NS) error {
		if err := ns.SaveWorkflow(ctx, "common", "main"); err != nil {
			return err
		}
		if _, err := ns.SaveVersion(ctx, lib); err != nil {
			return err
		}
		if _, err := ns.SaveVersion(ctx, including); err != nil {
			return err
		}
		var err error
		if backLib, err = ns.Version(ctx, "common", library); err != nil {
			return err
		}
		if back, err = ns.Version(ctx, "monthly-invoicing", "b4a0d2f"); err != nil {
			return err
		}
		listed, err = ns.VersionsAt(ctx, "common", []string{library})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	switch {
	case !backLib.Library || len(backLib.Libraries) != 0 || string(backLib.Includes["blocks/api.yaml"]) != ".api-brick:\n  timeout: 2m\n":
		t.Errorf("the library reads back as %+v", backLib)
	case back.Library || len(back.Libraries) != 1 || back.Libraries["finance/common@v2.1.0"].Commit != library:
		t.Errorf("the including version reads back as library %v with %v", back.Library, back.Libraries)
	case !maps.EqualFunc(back.Libraries["finance/common@v2.1.0"].Files, kept["finance/common@v2.1.0"].Files, slices.Equal):
		t.Errorf("the files kept of the library read back as %v", back.Libraries["finance/common@v2.1.0"].Files)
	case !listed[library].Library:
		t.Errorf("the listing does not say the library is one: %+v", listed)
	}

	// The run of a version including a library names the commit each include resolved to, and
	// the run of a version including none names none.
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var detail RunDetail
	read := func() {
		t.Helper()
		if err := pool.In(ctx, "finance", func(ctx context.Context, ns *NS) error {
			var err error
			detail, err = ns.RunDetail(ctx, financeRun)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if detail.Includes != nil {
		t.Errorf("a run of a version including nothing names %+v", detail.Includes)
	}
	if _, err := conn.Exec(ctx, `update runs set commit = 'b4a0d2f' where id = $1`, financeRun); err != nil {
		t.Fatal(err)
	}
	read()
	if want := []RunInclude{{Workflow: "finance/common", Ref: "v2.1.0", Commit: library}}; !slices.Equal(detail.Includes, want) {
		t.Errorf("the run names the includes %+v", detail.Includes)
	}
}
