package agk_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
)

const aRun agk.RunID = "01JMZ8V1P9C4XQ7K2N4D6F8H0A"

// A log is addressed by the task that wrote it, so eight shards of one step have eight logs
// and the URIs tell them apart. An artifact URI cannot: it names a port, and no step declares
// a port for its log.
func TestALogIsAddressedByItsTask(t *testing.T) {
	var seen []string
	for i := 1; i <= 3; i++ {
		task := agk.NewTaskID(aRun, "invoice", 2, agk.Shard{Index: i, Of: 8})
		u, err := agk.NewLogURI(task)
		if err != nil {
			t.Fatal(err)
		}
		if u.Run != aRun || u.Task != task {
			t.Fatalf("the log of %s is %+v", task, u)
		}
		seen = append(seen, u.String())
	}
	for i := range seen {
		for j := range seen {
			if i != j && seen[i] == seen[j] {
				t.Fatalf("two shards of one step share a log URI: %s", seen[i])
			}
		}
	}

	// And a log URI is a log URI and not an artifact one, in both directions, so a reader
	// knows what it holds without asking what it points at.
	if _, err := agk.ParseURI(seen[0]); err == nil {
		t.Error("a log URI parsed as an artifact URI")
	}
	artifact := agk.URI{Run: aRun, Step: "invoice", Port: "out", Name: "invoice.pdf"}
	if _, err := agk.ParseLogURI(artifact.String()); err == nil {
		t.Error("an artifact URI parsed as a log URI")
	}
}

// The task identifier carries slashes, so the URI has to survive the round trip rather than
// growing five segments a parser would have to guess the boundaries of.
func TestALogURISurvivesTheRoundTrip(t *testing.T) {
	for _, task := range []agk.TaskID{
		agk.NewTaskID(aRun, "invoice", 1, agk.Shard{}),
		agk.NewTaskID(aRun, "invoice", 2, agk.Shard{Index: 3, Of: 8}),
		agk.NewTaskID(aRun, "a-step_1", 11, agk.Shard{Index: 10, Of: 10}),
	} {
		u, err := agk.NewLogURI(task)
		if err != nil {
			t.Fatal(err)
		}
		back, err := agk.ParseLogURI(u.String())
		if err != nil {
			t.Fatalf("%s: %s", u, err)
		}
		if back != u {
			t.Errorf("%s came back as %+v", u, back)
		}

		encoded, err := json.Marshal(u)
		if err != nil {
			t.Fatal(err)
		}
		var read agk.LogURI
		if err := json.Unmarshal(encoded, &read); err != nil {
			t.Fatalf("%s: %s", encoded, err)
		}
		if read != u {
			t.Errorf("%s travelled as %s and came back as %+v", u, encoded, read)
		}
	}
}

// A task that wrote no log has no log URI, and that has to survive the round trip: the one case
// that must work is the one where nothing happened.
func TestATaskWithNoLogTravels(t *testing.T) {
	var none agk.LogURI
	if !none.IsZero() {
		t.Fatal("the zero value says it is a log")
	}
	encoded, err := json.Marshal(none)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "null" {
		t.Errorf("a task with no log travels as %s", encoded)
	}
	var back agk.LogURI
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("a task with no log could not be read back: %s", err)
	}
	if back != none {
		t.Errorf("it came back as %+v", back)
	}

	// And inside a document, which is where it actually travels.
	type result struct {
		Task agk.TaskID `json:"task"`
		Log  agk.LogURI `json:"log"`
	}
	in := result{Task: agk.NewTaskID(aRun, "invoice", 1, agk.Shard{})}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out result
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("%s could not be read back: %s", body, err)
	}
	if out != in {
		t.Errorf("%s came back as %+v", body, out)
	}
}

// The run in the URI and the run in the task have to be one run, or the URI addresses one
// run's prefix and another run's task and a sweep over a run's logs misses it.
func TestALogURICannotNameTwoRuns(t *testing.T) {
	const other agk.RunID = "01M2AAZ9G62NQXFAFCXKRPJEH5"
	task := agk.NewTaskID(other, "invoice", 1, agk.Shard{})
	crossed := "agk://log/" + string(aRun) + "/" + strings.ReplaceAll(string(task), "/", "%2F")
	if _, err := agk.ParseLogURI(crossed); err == nil {
		t.Fatal("a log URI named one run and a task of another")
	}
}

// What is refused, and why each one is refused rather than repaired.
func TestWhatIsNotALogURI(t *testing.T) {
	for _, c := range []struct{ uri, why string }{
		{"", "nothing at all"},
		{"agk://log/", "no run and no task"},
		{"agk://log/" + string(aRun), "a run and no task"},
		{"agk://log/" + string(aRun) + "/", "a task that is empty"},
		{"https://example.com/log", "a URL, which is what the scheme exists to refuse"},
		{"agk://run/" + string(aRun) + "/invoice/log", "the shape the page used to print, which names a port and not a task"},
		{"agk://log/not-a-run/x", "a run that is not an identifier"},
	} {
		if _, err := agk.ParseLogURI(c.uri); err == nil {
			t.Errorf("%q was read as a log URI, and it is %s", c.uri, c.why)
		}
	}
}
