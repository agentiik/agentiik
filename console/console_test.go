package console_test

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/console"
)

// This is the state of the tree: dist/ carries a .gitignore so that //go:embed compiles, and the
// console's build is put there by the release build, or by whoever builds it on this machine. So
// the test reads what this build actually carries: nothing where dist/ holds no index.html, and
// otherwise a build the API serves, which is where a file of a type it does not serve, or a page
// with no base for it to rewrite, is found before a release rather than at the first start.
func TestTheBuildCarriedIsNoneOrOneTheAPIServes(t *testing.T) {
	_, err := os.Stat("dist/" + console.Index)
	built := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	files := console.Files()
	switch {
	case !built && files != nil:
		t.Fatal("dist/ holds no index.html and this build carries a console")
	case !built:
		return
	case files == nil:
		t.Fatal("dist/ holds a console's build and this build carries none")
	}
	nobody := func(*http.Request) (api.Identity, error) { return api.Identity{}, nil }
	rt, err := api.NewRouter(api.DenyAll{}, nobody)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewConsole(rt, api.ConsoleOptions{Files: files, PublicURL: "https://agentiik.example.com"}); err != nil {
		t.Errorf("the console this build carries is refused: %s", err)
	}
}
