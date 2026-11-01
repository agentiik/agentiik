package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// agk brick init writes the template of the language asked for, named as the brick, and the
// brick it writes is one agk brick test runs as it stands: brick-sdk's CI builds each template and
// runs its cases, and what is checked here is that the copy written is that template.
func TestBrickInitWritesTheTemplateOfTheLanguageNamedAsTheBrick(t *testing.T) {
	for _, lang := range []string{"go", "python", "typescript"} {
		t.Run(lang, func(t *testing.T) {
			e, out, errs := reading(t)
			e.Dir = t.TempDir()
			if code := run(t.Context(), e, []string{"brick", "init", "invoice-label", "--language", lang}); code != exitSucceeded {
				t.Fatalf("left with %d: %s", code, errs)
			}
			dir := filepath.Join(e.Dir, "invoice-label")
			manifest, err := os.ReadFile(filepath.Join(dir, "brick.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(manifest), "name: invoice-label\n") {
				t.Errorf("the manifest is not named as the brick:\n%s", manifest)
			}
			for _, f := range []string{"Dockerfile", "cases/labels-each-item/out/out.json", "cases/refuses-a-step-with-no-label/exit"} {
				if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
					t.Errorf("%s was not written", f)
				}
			}
			err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				b, _ := os.ReadFile(p)
				if strings.Contains(string(b), "{{name}}") || strings.HasSuffix(p, ".tmpl") {
					t.Errorf("%s was written as a template rather than as the brick", p)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "agk brick test --image invoice-label:dev") {
				t.Errorf("the next steps are not said:\n%s", out)
			}
		})
	}
}

func TestBrickInitRefusesWhatItCannotWrite(t *testing.T) {
	for _, c := range []struct {
		args []string
		code int
		says string
	}{
		{[]string{"brick", "init"}, exitUsage, "takes the brick's name"},
		{[]string{"brick", "init", "label"}, exitUsage, "--language names no language"},
		{[]string{"brick", "init", "label", "--language", "cobol"}, exitUsage, "no template in \"cobol\""},
		{[]string{"brick", "init", "Http_Request", "--language", "go"}, exitUsage, "lowercase letters and digits"},
		{[]string{"brick", "init", "--language", "python", "taken"}, exitRefused, "exists and is not empty"},
	} {
		e, _, errs := reading(t)
		e.Dir = t.TempDir()
		os.MkdirAll(filepath.Join(e.Dir, "taken"), 0o755)
		os.WriteFile(filepath.Join(e.Dir, "taken", "main.py"), []byte("work in progress"), 0o644)
		if code := run(t.Context(), e, c.args); code != c.code || !strings.Contains(errs.String(), c.says) {
			t.Errorf("%v left with %d saying %q, want %d saying %q", c.args, code, errs, c.code, c.says)
		}
	}
}
