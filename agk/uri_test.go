package agk_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
)

func TestTheLogicalFormIsWhatTheDocumentationPrints(t *testing.T) {
	u := agk.URI{Run: "01JMZ8W4K2R7Q0E3N5T9", Step: "normalize", Port: "ok", Name: "purchase-order.pdf"}
	const want = "agk://run/01JMZ8W4K2R7Q0E3N5T9/normalize/ok/purchase-order.pdf"

	if u.String() != want {
		t.Fatalf("the URI reads %s, want %s", u, want)
	}
	back, err := agk.ParseURI(want)
	if err != nil {
		t.Fatal(err)
	}
	if back != u {
		t.Fatalf("the URI came back as %+v, want %+v", back, u)
	}
}

func TestParseURIRefusesWhatIsNotOne(t *testing.T) {
	cases := []struct {
		name string
		uri  string
	}{
		{"an http URL", "https://store.example/objects/purchase-order.pdf"},
		{"a physical key", "sha256/" + strings.Repeat("a", 64)},
		{"another scheme", "s3://run/r/s/p/a.pdf"},
		{"a URI with no name", "agk://run/r/s/p"},
		{"a URI with no port", "agk://run/r/s"},
		{"a name that is a path", "agk://run/r/s/p/dir/a.pdf"},
		{"a name that climbs out of the mount", "agk://run/r/s/p/.."},
		{"a name carrying a space", "agk://run/r/s/p/purchase order.pdf"},
		{"a step that is not an identifier", "agk://run/r/two words/p/a.pdf"},
		{"an empty name", "agk://run/r/s/p/"},
		{"nothing at all", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := agk.ParseURI(c.uri); err == nil {
				t.Fatalf("accepted %q", c.uri)
			}
		})
	}
}

func TestAURITravelsAsOneString(t *testing.T) {
	u := agk.URI{Run: "r", Step: "normalize", Port: "ok", Name: "a.pdf"}

	b, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"agk://run/r/normalize/ok/a.pdf"` {
		t.Fatalf("the URI was written as %s", b)
	}

	var back agk.URI
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back != u {
		t.Fatalf("the URI came back as %+v", back)
	}

	if err := json.Unmarshal([]byte(`{"run":"r"}`), &back); err == nil {
		t.Fatal("accepted an artifact addressed by an object rather than a URI")
	}
}
