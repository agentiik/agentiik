package driver

import (
	"encoding/json"
	"testing"

	"github.com/agentiik/agentiik/agk"
)

// The observer carries the three things graph.Result has nowhere to put, and two of them
// are blocks of the documented result message rather than shapes this package invented.
// A runner forwards them, so their members are spelled here the way that message spells
// them and not the way Go would have spelled them.
//
// A name is easy to get almost right, image_pull_millis for image_pull_ms among them, and
// almost right is a field the controller reads as absent. So the spelling is a test and
// not an intention.
func TestTheUsageBlockIsSpelledAsTheResultMessageSpellsIt(t *testing.T) {
	b, err := json.Marshal(Usage{CPUSeconds: 12.4, MaxRSSBytes: 198443008, ImagePullMS: 1})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	// "usage": { "cpu_seconds": 12.4, "max_rss_bytes": 198443008, "image_pull_ms": 0 }
	for _, name := range []string{"cpu_seconds", "max_rss_bytes", "image_pull_ms"} {
		if _, ok := got[name]; !ok {
			t.Errorf("the usage block carries no %s: %s", name, b)
		}
		delete(got, name)
	}
	for name := range got {
		t.Errorf("the usage block carries %s, which the result message does not name: %s", name, b)
	}
}

// The log reference is the other block, and it is the one the runner fills the URI of,
// since the sink knows where it went and this package does not.
func TestTheLogReferenceIsSpelledAsTheResultMessageSpellsIt(t *testing.T) {
	b, err := json.Marshal(LogRef{
		URI:       agk.URI{Run: "01JMZ8V1P9C4", Step: "invoice", Port: "log"},
		Lines:     412,
		Truncated: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	// "log": { "uri": "agk://run/…/invoice/log", "lines": 412, "truncated": false }
	for _, name := range []string{"uri", "lines", "truncated"} {
		if _, ok := got[name]; !ok {
			t.Errorf("the log reference carries no %s: %s", name, b)
		}
		delete(got, name)
	}
	for name := range got {
		t.Errorf("the log reference carries %s, which the result message does not name: %s", name, b)
	}
}

// An empty usage block is omitted rather than written as three zeroes, which is what
// IsZero is for: a task that cost nothing anybody measured says nothing about its cost.
func TestAnEmptyUsageBlockIsOmitted(t *testing.T) {
	b, err := json.Marshal(Event{Task: "01JMZ8V1P9C4/invoice/1", State: agk.TaskSucceeded})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["usage"]; ok {
		t.Errorf("a task nobody measured carries a usage block: %s", b)
	}
	if _, ok := got["log"]; ok {
		t.Errorf("a task whose log went nowhere carries a log reference: %s", b)
	}
}
