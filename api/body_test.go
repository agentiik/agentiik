package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
)

// What reading a request body takes, and that it is refused where it should be.
//
// These are internal tests because the reader is: a route is a handler in front of a database, and
// what is tested here is the part before either, turning a body into the request a handler is
// given.

// Every request reads back what encoding/json writes of it, field for field, so that the reader
// and the types a client encodes cannot drift apart: a field added to a type and not to its reader
// is a field every request carrying it is refused for.
func TestEveryBodyReadsWhatEncodingJSONWrites(t *testing.T) {
	value := "sk_live_notreal"
	for _, want := range []request{
		&Push{
			Entry: "agentiik.yaml", Document: []byte("kind: Workflow\n"),
			Includes:  map[string][]byte{"common.yaml": []byte(".brick: {}\n")},
			Manifests: map[string][]byte{"ghcr.io/acme/agk-invoice:1": []byte("kind: Brick\n"), "ghcr.io/acme/empty:1": {}},
			Tree: map[string]PushFile{
				"agentiik.yaml":     {Content: []byte("kind: Workflow\n"), Mode: "0644"},
				"scripts/render.sh": {Content: []byte{0xff, 0x00, '\n'}, Mode: "0755"},
				"empty":             {Content: []byte{}, Mode: "0644"},
			},
			Parent: "b4a0d2f6e1c9483a7d52b0e8f3a6c1d9e4b7a025", Branch: "main",
		},
		&Pool{Name: "dmz", Labels: []string{"zone=dmz", "arch=arm64"}, AcceptedNamespaces: []string{"finance"}, MaxCPU: 8, MaxMemoryBytes: 1 << 34, MaxDiskBytes: 1 << 40},
		&Issue{Labels: []string{"zone=dmz"}, ExpiresInSeconds: 600},
		&Join{Token: "agkjoin_x", Labels: []string{"zone=dmz"}, CPU: 4, MemoryBytes: 1 << 33, DiskBytes: 1 << 38, Architecture: "arm64", AgentVersion: "0.2.0"},
		&Beat{Tasks: []agk.TaskID{"01M2Z8V1P9C4XQ7K2N4D6F8H0B/normalize/1", "01M2Z8V1P9C4XQ7K2N4D6F8H0B/charge/1/2/3"}},
		&Beat{Tasks: nil},
		&Redemption{Grant: "agkgrant_x", TaskID: "01M2Z8V1P9C4XQ7K2N4D6F8H0C", IdempotencyKey: "01M2Z8V1P9C4XQ7K2N4D6F8H0B/normalize/1"},
		&Declare{Provider: "builtin", Value: &value, Encoding: "utf-8"},
		&Declare{Provider: "env", Path: "AGK_DEV_FINANCE_BILLING"},
	} {
		encoded, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		got := reflect.New(reflect.TypeOf(want).Elem()).Interface().(request)
		if err := readAtMost(httptest.NewRequest("POST", "/", bytes.NewReader(encoded)), got, pushMaxBytes); err != nil {
			t.Errorf("%T as encoding/json writes it is refused: %s", want, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%T reads back as %+v, and it was %+v", want, got, want)
		}
	}

	// A run's start reads into what the API keeps of it, the commit and the inputs as written.
	encoded, err := json.Marshal(Start{Commit: "a3f9c1e5d2b8470f9e61c3a8b0d4f7e2a9c5b1d3", Inputs: map[string]any{"orders": []any{map[string]any{"customer_id": "C-1042", "amount": 12.5}}}})
	if err != nil {
		t.Fatal(err)
	}
	var s starting
	if err := readAtMost(httptest.NewRequest("POST", "/", bytes.NewReader(encoded)), &s, startMaxBytes); err != nil {
		t.Fatalf("a start as encoding/json writes it is refused: %s", err)
	}
	if s.commit != "a3f9c1e5d2b8470f9e61c3a8b0d4f7e2a9c5b1d3" || string(s.inputs) != `{"orders":[{"amount":12.5,"customer_id":"C-1042"}]}` {
		t.Errorf("a start reads back as %q with inputs %s", s.commit, s.inputs)
	}
}

// A body is read closed: whatever the route does not read, or reads twice, or finds after its
// document, is refused with a sentence and a 400 rather than half understood.
func TestABodyIsReadClosed(t *testing.T) {
	for what, c := range map[string]struct {
		body string
		into request
	}{
		"an empty body":                          {``, new(Join)},
		"a body of whitespace":                   {" \n\t", new(Join)},
		"an array where an object is read":       {`[]`, new(Join)},
		"a field nobody reads":                   {`{"token":"x","priority":"urgent"}`, new(Join)},
		"a field in a different case":            {`{"Token":"x"}`, new(Join)},
		"a field written twice":                  {`{"token":"x","token":"y"}`, new(Join)},
		"a second document after its own":        {`{"token":"x"} {"token":"y"}`, new(Join)},
		"a stray brace after its own":            {`{"token":"x"}}`, new(Join)},
		"a document that stops":                  {`{"token":"x"`, new(Join)},
		"a number where a string is read":        {`{"token":7}`, new(Join)},
		"a fraction where a whole number is":     {`{"cpu":1.5}`, new(Join)},
		"a number past 64 bits":                  {`{"memory_bytes":18446744073709551616}`, new(Join)},
		"a string where a list is read":          {`{"labels":"zone=dmz"}`, new(Join)},
		"a number in a list of strings":          {`{"labels":["zone=dmz",4]}`, new(Join)},
		"a tree file written twice":              {`{"tree":{"a.sh":{"mode":"0644"},"a.sh":{"mode":"0755"}}}`, new(Push)},
		"an include written twice":               {`{"includes":{"a.yaml":"","a.yaml":"eA=="}}`, new(Push)},
		"a tree file with a field nobody reads":  {`{"tree":{"a.sh":{"mode":"0644","owner":"root"}}}`, new(Push)},
		"a tree file's mode written twice":       {`{"tree":{"a.sh":{"mode":"0644","mode":"0755"}}}`, new(Push)},
		"content that is not base64":             {`{"tree":{"a.sh":{"content":"not base64!","mode":"0644"}}}`, new(Push)},
		"content that is a list of bytes":        {`{"tree":{"a.sh":{"content":[104,105],"mode":"0644"}}}`, new(Push)},
		"text that is not UTF-8":                 {"{\"token\":\"caf\xe9\"}", new(Join)},
		"a field in a body that takes none":      {`{"pool":"elsewhere"}`, nothingAsked{}},
		"inputs that are a list":                 {`{"inputs":[1,2]}`, new(starting)},
		"inputs holding a number no float holds": {`{"inputs":{"n":1e400}}`, new(starting)},
		"inputs that stop":                       {`{"inputs":{"n":[1,2}}`, new(starting)},
	} {
		err := readAtMost(httptest.NewRequest("POST", "/", strings.NewReader(c.body)), c.into, smallMaxBytes)
		if err == nil {
			t.Errorf("%s was read", what)
			continue
		}
		if statusOf(err) != http.StatusBadRequest {
			t.Errorf("%s is answered %d: %s", what, statusOf(err), err)
		}
		if !strings.HasPrefix(err.Error(), "the request body") {
			t.Errorf("%s is refused with %q", what, err)
		}
	}

	// A body with no field at all, and one naming each field as null, read as encoding/json reads
	// them: as nothing.
	for _, body := range []string{`{}`, `null`, `{"token":null,"labels":null,"cpu":null}`} {
		var j Join
		if err := readAtMost(httptest.NewRequest("POST", "/", strings.NewReader(body)), &j, smallMaxBytes); err != nil {
			t.Errorf("%s is refused: %s", body, err)
		}
		if !reflect.DeepEqual(j, Join{}) {
			t.Errorf("%s reads as %+v", body, j)
		}
	}

	// And base64 is read however a writer escaped it, since JSON lets it escape any character.
	var p Push
	if err := readAtMost(httptest.NewRequest("POST", "/", strings.NewReader(`{"document":"aGk="}`)), &p, smallMaxBytes); err != nil || string(p.Document) != "hi" {
		t.Errorf("base64 with an escaped character reads as %q: %v", p.Document, err)
	}
}

// A refusal does not repeat what the body carried after its document, nor more than the start of
// a name it did not read, since either may be a secret or as long as its caller likes.
func TestARefusalRepeatsNothingItNeedNot(t *testing.T) {
	const value = "sk_live_notreal"
	for what, body := range map[string]string{
		"a value after the declaration":               `{"provider":"builtin"} {"value":"` + value + `"}`,
		"a value that is not the base64 it should be": `{"tree":{"a":{"content":"` + value + `!","mode":"0644"}}}`,
		"a value under a name nobody reads":           `{"provider":"builtin","valeu":"` + value + `"}`,
	} {
		var into request = new(Declare)
		if strings.Contains(body, "tree") {
			into = new(Push)
		}
		err := readAtMost(httptest.NewRequest("PUT", "/", strings.NewReader(body)), into, declareMaxBytes)
		if err == nil || strings.Contains(err.Error(), value) {
			t.Errorf("%s is refused with %v", what, err)
		}
	}

	long := strings.Repeat("n", 1<<16)
	err := readAtMost(httptest.NewRequest("POST", "/", strings.NewReader(`{"`+long+`":0}`)), nothingAsked{}, 1<<20)
	if err == nil || len(err.Error()) > 512 {
		t.Errorf("a name of %d bytes is refused in %d bytes", len(long), len(fmt.Sprint(err)))
	}
}

// Every collection a route reads is counted as it is read, and refused at the first entry past what
// the route takes, with a 413 that says what the limit is: the request is larger than the route is
// willing to read, rather than wrong. As many as the route takes are read.
func TestACollectionPastItsCountIsTooLarge(t *testing.T) {
	for _, c := range []struct {
		name  string
		most  int
		body  func(n int) string
		into  func() request
		limit int64
	}{
		{"a tree", TreeMaxFiles, func(n int) string {
			return entries(`{"tree":{`, n, func(i int) string { return fmt.Sprintf(`"f%d":{"mode":"0644"}`, i) }) + `}}`
		}, func() request { return new(Push) }, pushMaxBytes},
		{"includes", TreeMaxFiles, func(n int) string {
			return entries(`{"includes":{`, n, func(i int) string { return fmt.Sprintf(`"f%d":""`, i) }) + `}}`
		}, func() request { return new(Push) }, pushMaxBytes},
		{"manifests", TreeMaxFiles, func(n int) string {
			return entries(`{"manifests":{`, n, func(i int) string { return fmt.Sprintf(`"i%d":""`, i) }) + `}}`
		}, func() request { return new(Push) }, pushMaxBytes},
		{"a pool's labels", namesMax, labels("labels"), func() request { return new(Pool) }, smallMaxBytes},
		{"a pool's namespaces", namesMax, labels("accepted_namespaces"), func() request { return new(Pool) }, smallMaxBytes},
		{"a join token's labels", namesMax, labels("labels"), func() request { return new(Issue) }, smallMaxBytes},
		{"a machine's labels", namesMax, labels("labels"), func() request { return new(Join) }, smallMaxBytes},
		{"a heartbeat's tasks", beatMaxTasks, labels("tasks"), func() request { return new(Beat) }, beatMaxBytes},
		{"the values of a run's inputs", inputsMaxValues, func(n int) string {
			// The inputs object is a value itself, so it holds n-1 more.
			return entries(`{"inputs":{"a":[`, n-2, func(int) string { return `0` }) + `]}}`
		}, func() request { return new(starting) }, startMaxBytes},
	} {
		if err := readAtMost(httptest.NewRequest("POST", "/", strings.NewReader(c.body(c.most))), c.into(), c.limit); err != nil {
			t.Errorf("%s of %d is refused: %s", c.name, c.most, err)
		}
		err := readAtMost(httptest.NewRequest("POST", "/", strings.NewReader(c.body(c.most+1))), c.into(), c.limit)
		if err == nil {
			t.Errorf("%s of %d is read", c.name, c.most+1)
			continue
		}
		if statusOf(err) != http.StatusRequestEntityTooLarge || !strings.Contains(err.Error(), strconv.Itoa(c.most)) {
			t.Errorf("%s of %d is answered %d: %s", c.name, c.most+1, statusOf(err), err)
		}
	}
}

func entries(prefix string, n int, each func(i int) string) string {
	var b strings.Builder
	b.WriteString(prefix)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(each(i))
	}
	return b.String()
}

func labels(field string) func(n int) string {
	return func(n int) string {
		return entries(`{"`+field+`":[`, n, func(i int) string { return fmt.Sprintf(`"l%d"`, i) }) + `]}`
	}
}

// A body past its route's cap is refused with a 413 that wraps *http.MaxBytesError, whether it
// declared its length or not, and one declaring more than the cap is refused before any of it is
// read.
func TestABodyPastItsCapIsTooLarge(t *testing.T) {
	large := `{"token":"` + strings.Repeat("x", smallMaxBytes) + `"}`

	declared := httptest.NewRequest("POST", "/", unread{t})
	declared.ContentLength = smallMaxBytes + 1
	chunked := httptest.NewRequest("POST", "/", strings.NewReader(large))
	chunked.ContentLength = -1
	for what, r := range map[string]*http.Request{
		"a body declaring more than the cap": declared,
		"a body longer than the cap":         httptest.NewRequest("POST", "/", strings.NewReader(large)),
		"a body sent in chunks past the cap": chunked,
	} {
		err := readAtMost(r, new(Join), smallMaxBytes)
		if statusOf(err) != http.StatusRequestEntityTooLarge || !errors.As(err, new(*http.MaxBytesError)) {
			t.Errorf("%s is refused with %v", what, err)
		}
	}

	// And one in chunks within the cap is read like any other.
	within := httptest.NewRequest("POST", "/", strings.NewReader(`{"token":"agkjoin_x"}`))
	within.ContentLength = -1
	var j Join
	if err := readAtMost(within, &j, smallMaxBytes); err != nil || j.Token != "agkjoin_x" {
		t.Errorf("a body in chunks within the cap reads as %+v: %v", j, err)
	}
}

// unread is a body that fails the test if anybody reads it.
type unread struct{ t *testing.T }

func (u unread) Read([]byte) (int, error) {
	u.t.Error("a body declaring more than its cap was read")
	return 0, errors.New("read")
}

// The inputs of a run are counted and kept as they were written, a slice of the body, rather than
// decoded and written again: what whitespace and order they were sent in, and a name written
// twice, which whoever decodes them decides by the last.
func TestTheInputsAreKeptAsTheyWereWritten(t *testing.T) {
	for body, want := range map[string]string{
		`{"commit":"c","inputs" :  {"b": [1, 2.50, "x"], "a": {}}  }`: `{"b": [1, 2.50, "x"], "a": {}}`,
		`{"inputs":{"a":1,"a":2},"commit":"c"}`:                       `{"a":1,"a":2}`,
		`{"inputs":{}}`:                                               `{}`,
		`{"inputs":null}`:                                             ``,
		`{"commit":"c"}`:                                              ``,
	} {
		var s starting
		if err := readAtMost(httptest.NewRequest("POST", "/", strings.NewReader(body)), &s, startMaxBytes); err != nil {
			t.Errorf("%s is refused: %s", body, err)
			continue
		}
		if string(s.inputs) != want {
			t.Errorf("%s keeps the inputs %q, want %q", body, s.inputs, want)
		}
	}
}
