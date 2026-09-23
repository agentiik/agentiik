package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
)

// What reading a request body costs, and that it is refused where it should be.
//
// These are internal tests because the reader is: a route is a handler in front of a database, and
// what is measured here is the part before either, the bytes allocated to turn a body into the
// request a handler is given.

const mib = 1 << 20

// filled is a body of prefix and suffix around as many entries as fit in limit bytes, written by
// each and separated by commas.
func filled(prefix, suffix string, limit int64, each func(i int) string) []byte {
	var b bytes.Buffer
	b.WriteString(prefix)
	for i := 0; ; i++ {
		e := each(i)
		if int64(b.Len()+1+len(e)+len(suffix)) > limit {
			break
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(e)
	}
	b.WriteString(suffix)
	return b.Bytes()
}

// named is the i-th of a run of distinct short names, quoted.
func named(i int) string { return `"` + strconv.FormatInt(int64(i), 36) + `"` }

// spent is what reading the body of one request allocates: the least of three readings, so that
// whatever else the process allocates meanwhile is not counted against it.
func spent(ask func() *http.Request, into func() request, limit int64) (uint64, error) {
	least := uint64(math.MaxUint64)
	var err error
	for range 3 {
		r := ask()
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		err = readAtMost(r, into(), limit)
		runtime.ReadMemStats(&after)
		least = min(least, after.TotalAlloc-before.TotalAlloc)
	}
	return least, err
}

// declaring is a request carrying body that declares its length, as httptest's does and as most
// clients' do, and chunked one that declares nothing, as a client sending in chunks does.
func declaring(body []byte) func() *http.Request {
	return func() *http.Request { return httptest.NewRequest("POST", "/", bytes.NewReader(body)) }
}

func chunked(body []byte) func() *http.Request {
	return func() *http.Request {
		r := httptest.NewRequest("POST", "/", bytes.NewReader(body))
		r.ContentLength = -1
		return r
	}
}

// Every route's body, shaped to cost as much as that route lets it, at its route's cap. A body of
// many small values is the expensive one, since decoding pays for each value whatever it weighs
// on the wire, so each collection a route reads is filled with the smallest entries it takes.
//
// Before is what encoding/json spent reading the same shape at the cap the route had then, 8 MiB
// for every route but a push (16 MiB) and a secret's declaration (2 MiB); after is what this
// reader spends at the route's cap now, the same whether the body declares its length or is sent
// in chunks. Both were measured with go1.27.1 on darwin/arm64, without the race detector, as
// bytes allocated. Nothing asserts either number, which move with the toolchain and the platform.
// What is asserted is the ceiling, both ways: no body costs more than costsAtMost times its
// route's cap to read.
//
// The object store's routes are not here: they read no JSON, the form before a file is held to
// 64 KiB, and the file is hashed as it streams.
func TestNoBodyCostsMoreThanTwiceAndAHalfItsCapToRead(t *testing.T) {
	const costsAtMost = 2.5

	empty := func(int) string { return `""` }
	for _, c := range []struct {
		name  string
		into  func() request
		limit int64
		body  func(limit int64) []byte

		was, before, after float64 // the cap then, what it cost then, what it costs now, in MiB
	}{
		{"a push of empty tree entries", func() request { return new(Push) }, pushMaxBytes,
			func(l int64) []byte {
				return filled(`{"tree":{`, `}}`, l, func(i int) string { return named(i) + `:{}` })
			}, 16, 295.2, 22.533},
		{"a push of tree entries written out", func() request { return new(Push) }, pushMaxBytes,
			func(l int64) []byte {
				return filled(`{"tree":{`, `}}`, l, func(i int) string { return named(i) + `:{"content":"","mode":"0644"}` })
			}, 16, 142.1, 22.793},
		{"a push of empty includes", func() request { return new(Push) }, pushMaxBytes,
			func(l int64) []byte {
				return filled(`{"includes":{`, `}}`, l, func(i int) string { return named(i) + `:""` })
			}, 16, 231.3, 22.090},
		{"a push of empty manifests", func() request { return new(Push) }, pushMaxBytes,
			func(l int64) []byte {
				return filled(`{"manifests":{`, `}}`, l, func(i int) string { return named(i) + `:""` })
			}, 16, 231.2, 22.090},
		{"a push of one file as large as the body", func() request { return new(Push) }, pushMaxBytes,
			func(l int64) []byte {
				return []byte(`{"tree":{"big.bin":{"mode":"0644","content":"` + strings.Repeat("A", int(l-60)/4*4) + `"}}}`)
			}, 16, 44.0, 33.335},
		{"inputs of zeros", func() request { return new(starting) }, startMaxBytes,
			func(l int64) []byte { return filled(`{"inputs":{"a":[`, `]}}`, l, func(int) string { return `0` }) },
			8, 402.4, 5.334},
		{"inputs of empty objects", func() request { return new(starting) }, startMaxBytes,
			func(l int64) []byte { return filled(`{"inputs":{"a":[`, `]}}`, l, func(int) string { return `{}` }) },
			8, 508.5, 5.334},
		{"inputs of objects of one member", func() request { return new(starting) }, startMaxBytes,
			func(l int64) []byte {
				return filled(`{"inputs":{"a":[`, `]}}`, l, func(int) string { return `{"a":0}` })
			},
			8, 491.9, 5.334},
		{"inputs of one object of many members", func() request { return new(starting) }, startMaxBytes,
			func(l int64) []byte {
				return filled(`{"inputs":{`, `}}`, l, func(i int) string { return named(i) + `:0` })
			}, 8, 176.8, 5.334},
		{"a pool of empty labels", func() request { return new(Pool) }, smallMaxBytes,
			func(l int64) []byte { return filled(`{"labels":[`, `]}`, l, empty) }, 8, 273.8, 0.141},
		{"a pool of empty namespaces", func() request { return new(Pool) }, smallMaxBytes,
			func(l int64) []byte { return filled(`{"accepted_namespaces":[`, `]}`, l, empty) }, 8, 273.8, 0.141},
		{"a join token of empty labels", func() request { return new(Issue) }, smallMaxBytes,
			func(l int64) []byte { return filled(`{"labels":[`, `]}`, l, empty) }, 8, 273.8, 0.141},
		{"a join of empty labels, from anybody", func() request { return new(Join) }, smallMaxBytes,
			func(l int64) []byte { return filled(`{"labels":[`, `]}`, l, empty) }, 8, 273.8, 0.141},
		{"a heartbeat of empty keys", func() request { return new(Beat) }, beatMaxBytes,
			func(l int64) []byte { return filled(`{"tasks":[`, `]}`, l, empty) }, 8, 273.8, 1.563},
		{"a redemption of one long grant", func() request { return new(Redemption) }, smallMaxBytes,
			func(l int64) []byte { return []byte(`{"grant":"` + strings.Repeat("A", int(l)-12) + `"}`) }, 8, 24.0, 0.145},
		{"a request for a bus credential naming one long field", func() request { return nothingAsked{} }, smallMaxBytes,
			func(l int64) []byte { return []byte(`{"` + strings.Repeat("A", int(l)-7) + `":0}`) }, 8, 159.6, 0.146},
		{"a declaration of one long value", func() request { return new(Declare) }, declareMaxBytes,
			func(l int64) []byte {
				return []byte(`{"provider":"builtin","value":"` + strings.Repeat("A", int(l)-33) + `"}`)
			}, 2, 6.0, 4.667},
	} {
		body := c.body(c.limit)
		if int64(len(body)) > c.limit || int64(len(body)) < c.limit-64 {
			t.Fatalf("%s is %d bytes, and it is meant to fill its cap of %d", c.name, len(body), c.limit)
		}
		for how, ask := range map[string]func() *http.Request{"declaring its length": declaring(body), "in chunks": chunked(body)} {
			n, _ := spent(ask, c.into, c.limit)
			cost := float64(n) / float64(c.limit)
			t.Logf("%s, %s: %.3f MiB at a cap of %.3f MiB (x%.1f), recorded as %.3f; encoding/json spent %.1f MiB at %v MiB (x%.1f)",
				c.name, how, float64(n)/mib, float64(c.limit)/mib, cost, c.after, c.before, c.was, c.before/c.was)
			if cost > costsAtMost {
				t.Errorf("%s, %s, costs %.1f MiB to read, %.1f times its cap of %.3f MiB", c.name, how, float64(n)/mib, cost, float64(c.limit)/mib)
			}
		}
	}
}

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

		// Every number a float holds but zero, and nothing that reaches further from the point
		// than a float does, which PostgreSQL would write back in full or refuse to hold.
		"inputs holding a number a float holds as zero":   {`{"inputs":{"n":1e-400}}`, new(starting)},
		"inputs holding one written 16,383 digits out":    {`{"inputs":{"n":1e-16383}}`, new(starting)},
		"inputs holding a zero written 16,383 digits out": {`{"inputs":{"n":0e-16383}}`, new(starting)},
		"inputs holding a zero written 341 digits out":    {`{"inputs":{"n":0e-341}}`, new(starting)},
		"inputs holding a zero past any exponent":         {`{"inputs":{"n":0e999999999999999999999}}`, new(starting)},
		"inputs holding 341 digits after the point":       {`{"inputs":{"n":1.` + strings.Repeat("0", 341) + `}}`, new(starting)},
		"inputs holding U+0000 in a string":               {`{"inputs":{"n":["a\u0000"]}}`, new(starting)},
		"inputs holding U+0000 in a name":                 {`{"inputs":{"n":{"\u0000":1}}}`, new(starting)},
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

	// One in chunks exactly as long as the cap is read, and one a byte longer is not, which is
	// told by reading one byte past the cap rather than by a buffer a byte larger than it.
	for extra, want := range map[int]int{0: 0, 1: http.StatusRequestEntityTooLarge} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(`{"token":"`+strings.Repeat("x", smallMaxBytes-12+extra)+`"}`))
		r.ContentLength = -1
		got := 0
		if err := readAtMost(r, new(Join), smallMaxBytes); err != nil {
			got = statusOf(err)
		}
		if got != want {
			t.Errorf("a body in chunks %d bytes past the cap is answered %d, want %d", extra, got, want)
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

// A body is held as it arrives rather than as it declares, since a length is only what its caller
// says: one declaring the most a push may be and sending none of it holds a few kibibytes, and one
// cut short a few times what it sent. Before, the declaration alone held 16 MiB until the caller
// hung up, so that a few hundred bytes of headers on each of 64 connections held a gibibyte.
func TestABodyIsHeldAsItArrivesRatherThanAsItDeclares(t *testing.T) {
	// Nothing, a little, and two sizes a buffer fills exactly, which is when one four times
	// larger is made.
	for _, sent := range []int{0, 100, 64 << 10, 1 << 20} {
		n, err := spent(func() *http.Request {
			r := httptest.NewRequest("PUT", "/", bytes.NewReader(bytes.Repeat([]byte(" "), sent)))
			r.ContentLength = pushMaxBytes
			return r
		}, func() request { return new(Push) }, pushMaxBytes)
		if err == nil || !strings.Contains(err.Error(), "ends before") {
			t.Errorf("a push declaring %d bytes and sending %d is refused with %v", pushMaxBytes, sent, err)
		}
		// The buffer it holds is at most slurpGrowth times what arrived, and those it grew
		// through a third of that again.
		if most := uint64(2*slurpFirstBytes + sent*slurpGrowth*slurpGrowth/(slurpGrowth-1)); n > most {
			t.Errorf("a push declaring %d bytes and sending %d allocates %d bytes to read, more than %d", pushMaxBytes, sent, n, most)
		}
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

	// And every number a float holds, as far from the point as a float reaches, and the six
	// characters \u0000 written with their backslash escaped, which are not U+0000.
	edges := `{"n":[1e308,-1.7976931348623157e308,5e-324,4.9406564584124654e-324,0e-340,-0.0,"\\u0000"]}`
	var s starting
	if err := readAtMost(httptest.NewRequest("POST", "/", strings.NewReader(`{"inputs":`+edges+`}`)), &s, startMaxBytes); err != nil || string(s.inputs) != edges {
		t.Errorf("the inputs %s are kept as %s: %v", edges, s.inputs, err)
	}
}
