package agk_test

import (
	"encoding/json"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/internal/fixtures"
)

// TestTheIdentifierGrammar is the grammar the schema writes as
// ^[A-Za-z0-9][A-Za-z0-9_-]*$, and the reason it is that one: the name travels as a
// directory under /agk/in/, as a file under /agk/out/ports/ and as one entry of the
// comma separated AGK_OUT_PORTS.
func TestTheIdentifierGrammar(t *testing.T) {
	cases := []struct {
		name  string
		valid bool
	}{
		{"normalize", true},
		{"out", true},
		{"ok", true},
		{"error", true},
		{"fetch-invoices", true},
		{"step_2", true},
		{"9lives", true},
		{"", false},
		{"out,error", false},
		{"two words", false},
		{"in/out", false},
		{"-leading", false},
		{"_leading", false},
		{"café", false},
		{"out.json", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, err := range []error{agk.Step(c.name).Validate(), agk.Port(c.name).Validate()} {
				if c.valid && err != nil {
					t.Fatalf("refused: %v", err)
				}
				if !c.valid && err == nil {
					t.Fatal("accepted")
				}
			}
		})
	}
}

// A step or a port is at most 255 characters, the longest name a filesystem holds: a
// step is a directory of its task's work directory and a port a file under
// /agk/out/ports/, so a longer one is a name no runner could lay out.
func TestAnIdentifierIsNoLongerThanAFileName(t *testing.T) {
	longest := strings.Repeat("n", 255)
	for _, err := range []error{agk.Step(longest).Validate(), agk.Port(longest).Validate()} {
		if err != nil {
			t.Errorf("a name of 255 characters was refused: %v", err)
		}
	}
	for _, err := range []error{agk.Step(longest + "n").Validate(), agk.Port(longest + "n").Validate()} {
		if err == nil {
			t.Error("a name of 256 characters was accepted")
			continue
		}
		// The bound, and not the whole name printed back.
		if said := err.Error(); !strings.Contains(said, "at most 255") || strings.Contains(said, longest) {
			t.Errorf("the refusal reads %q", said)
		}
	}
}

func TestAPortNameSaysWhatTheRuleIs(t *testing.T) {
	err := agk.Port("out,error").Validate()
	if err == nil {
		t.Fatal("accepted a port name carrying a comma")
	}
	if !strings.Contains(err.Error(), `^[A-Za-z0-9][A-Za-z0-9_-]*$`) {
		t.Fatalf("the refusal does not print the grammar: %v", err)
	}
}

// TestARunIdentifier imposes no length. The documentation says a run carries a ULID and
// then prints run identifiers shorter than twenty-six characters, no two of them the
// same length; the schema records that reading and asks only that the value be there.
func TestARunIdentifier(t *testing.T) {
	cases := []struct {
		id    agk.RunID
		valid bool
	}{
		{"01JMZ8W4K2R7Q0E3N5T9ZQ4XKB", true},
		{"01JMZ8W4K2R7Q0E3N5T9", true},
		{"run-12", true},
		{"", false},
		{"01JMZ8/W4K2R7", false},
		{"01JMZ8 W4K2R7", false},
		{"..", false},
	}
	for _, c := range cases {
		err := c.id.Validate()
		if c.valid && err != nil {
			t.Errorf("%q was refused: %v", c.id, err)
		}
		if !c.valid && err == nil {
			t.Errorf("%q was accepted", c.id)
		}
	}
}

func TestNewRunIDMintsAULID(t *testing.T) {
	id := agk.NewRunID()
	if len(id) != 26 {
		t.Fatalf("NewRunID minted %q, which is not a twenty-six character identifier", id)
	}
	if err := id.Validate(); err != nil {
		t.Fatalf("NewRunID minted an identifier it then refuses: %v", err)
	}
	if agk.NewRunID() == id {
		t.Fatal("NewRunID minted the same identifier twice")
	}
}

// A word reserved late is one of the reserved words, needed by a route under it that a later
// release serves, and it is the only kind a reference may still name: every other reserved
// word was reserved before any namespace could take it, so no namespace carries one.
func TestAWordReservedLateIsReservedAndStillNamesWhatCarriesIt(t *testing.T) {
	if len(agk.LateReservations) == 0 {
		t.Fatal("no word is reserved late, and stats is, for GET /api/v1/stats/pools")
	}
	for _, r := range agk.LateReservations {
		if !agk.IsReservedNamespace(r.Word) {
			t.Errorf("%s is reserved late and is not one of the reserved words", r.Word)
		}
		if _, path, _ := strings.Cut(r.Route, " "); !strings.HasPrefix(path, "/api/v1/"+r.Word+"/") {
			t.Errorf("%s is reserved for %q, which is not a route under it", r.Word, r.Route)
		}
		release := regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
		if !release.MatchString(r.Served) || !release.MatchString(r.Since) || !earlier(r.Since, r.Served) {
			t.Errorf("%s is reserved from %q for a route served from %q, which are not two releases, the first before the second", r.Word, r.Since, r.Served)
		}
		if agk.NamesNoNamespace(r.Word) {
			t.Errorf("%s names no namespace, and an installation may hold one it created before the word was reserved", r.Word)
		}
		if got, late := agk.ReservedLate(r.Word); !late || got != r {
			t.Errorf("the late reservation of %s reads %+v, %v", r.Word, got, late)
		}
	}
	if !agk.NamesNoNamespace("runs") || !agk.IsReservedNamespace("runs") {
		t.Error("runs, reserved since namespaces were first created, names a namespace")
	}
	if _, late := agk.ReservedLate("runs"); late {
		t.Error("runs reads as reserved late")
	}
	if agk.NamesNoNamespace("finance") || agk.IsReservedNamespace("finance") {
		t.Error("finance, which no route takes, is refused")
	}
}

// earlier says whether release a comes before release b, each written vX.Y.Z or X.Y.Z.
func earlier(a, b string) bool {
	parse := func(r string) [3]int {
		var v [3]int
		for i, part := range strings.SplitN(strings.TrimPrefix(r, "v"), ".", 3) {
			v[i], _ = strconv.Atoi(part)
		}
		return v
	}
	x, y := parse(a), parse(b)
	return slices.Compare(x[:], y[:]) < 0
}

// The words the schemas refuse as a namespace's name are the engine's: a word the schemas refuse
// and the engine does not is a name a client refuses and the API creates, and one the engine
// reserves and the schemas do not is a name a client offers and the API refuses. The vendored
// schemas may lag behind a word reserved late alone, and only while they are of a release before
// the one that reserves it, since the schemas' own pull request lands first and they are vendored
// at the release.
func TestTheReservedWordsAreTheSchemasOwn(t *testing.T) {
	for _, c := range []struct{ document, definition string }{
		{"wire.schema.json", "namespace"},
		{"workflow.schema.json", "namespace"},
	} {
		raw, err := fs.ReadFile(fixtures.FS, c.document)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Defs map[string]struct {
				Not struct {
					Pattern string `json:"pattern"`
				} `json:"not"`
			} `json:"$defs"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		words, ok := strings.CutPrefix(doc.Defs[c.definition].Not.Pattern, "^(")
		if words, ok = strings.CutSuffix(words, ")$"); !ok {
			t.Fatalf("%s $defs/%s refuses %q, which is not a list of words", c.document, c.definition, doc.Defs[c.definition].Not.Pattern)
		}
		refused := strings.Split(words, "|")
		for _, w := range refused {
			if !agk.IsReservedNamespace(w) {
				t.Errorf("%s refuses %s as a namespace, which the engine does not reserve", c.document, w)
			}
		}
		for _, w := range agk.ReservedNamespaces {
			if r, late := agk.ReservedLate(w); late && earlier(fixtures.Version, r.Since) || slices.Contains(refused, w) {
				continue
			}
			t.Errorf("the engine reserves %s, which %s %s does not refuse as a namespace", w, c.document, fixtures.Version)
		}
	}
}
