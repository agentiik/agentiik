package api_test

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/agentiik/agentiik/internal/fixtures"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// A shipment is held to the wire whole: what a runner sends while its container runs and all of
// what it is answered, against $defs/logShipment.

// The corpus first, so that a failure below is this package's and not the compiler's.
func TestTheVendoredLogShipmentCorpusIsWhatItSaysItIs(t *testing.T) {
	s := wire(t, "/$defs/logShipment")
	cases, err := fixtures.LogShipments()
	if err != nil {
		t.Fatal(err)
	}
	valid, invalid := 0, 0
	for _, c := range cases {
		body, err := fs.ReadFile(fixtures.FS, c.File)
		if err != nil {
			t.Fatal(err)
		}
		v, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		err = s.Validate(v)
		switch {
		case c.Valid && err != nil:
			t.Errorf("%s should be accepted: %s", c.File, err)
		case !c.Valid && err == nil:
			t.Errorf("%s should be refused: %s", c.File, c.Rule)
		}
		if c.Valid {
			valid++
		} else {
			invalid++
		}
	}
	if valid == 0 || invalid == 0 {
		t.Fatalf("the vendored log shipment corpus holds %d valid and %d invalid documents", valid, invalid)
	}
}

// shipmentOf is the request half of one document of the corpus, as the wire writes it.
func shipmentOf(t *testing.T, file string) map[string]any {
	t.Helper()
	body, err := fs.ReadFile(fixtures.FS, file)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Request map[string]any `json:"request"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Request
}

// shipped sends one shipment with the task's grant where the wire puts it, beside the runner
// credential, and never in the body.
func shipped(t *testing.T, h http.Handler, credential, grant string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/v1/tasks/logs", bytes.NewReader(encoded))
	r.Header.Set("Content-Type", "application/json")
	if credential != "" {
		r.Header.Set("Authorization", "Bearer "+credential)
	}
	if grant != "" {
		r.Header.Set("Agentiik-Grant", grant)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var answer map[string]any
	if w.Body.Len() > 0 {
		json.Unmarshal(w.Body.Bytes(), &answer)
	}
	return w, answer
}

// A runner written from the wire is understood and answered as the wire describes, whether its
// chunk is the one the API expects or one past a gap; and one that writes its grant into the body,
// as the invalid fixture does, is refused rather than having the token land in the log.
func TestALogShipmentAndItsAnswerAreWhatTheWireDescribes(t *testing.T) {
	cases, err := fixtures.LogShipments()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		g := withGrants(t, held{})
		credential := g.joined(t)
		clear, _, _ := g.dispatched(t, nil)
		g.redeemed(t, credential, asking(clear))

		request := shipmentOf(t, c.File)
		request["idempotency_key"] = string(grantKey)
		if !c.Valid {
			if w, _ := shipped(t, g.handler, credential, clear, request); w.Code != http.StatusBadRequest {
				t.Errorf("%s answered %d, and %s: %s", c.File, w.Code, c.Rule, w.Body)
			}
			continue
		}

		// As the fixture has it, chunk seven of a log the API has had nothing of: a chunk past
		// a gap, answered with where the gap is.
		w, answer := shipped(t, g.handler, credential, clear, request)
		if w.Code != http.StatusOK {
			t.Fatalf("%s answered %d: %s", c.File, w.Code, w.Body)
		}
		if err := conforms(t, "/$defs/logShipment", map[string]any{"request": request, "response": answer}); err != nil {
			t.Errorf("%s past a gap and its answer are not the exchange the wire describes: %s: %v", c.File, err, answer)
		}
		if answer["accepted"] != 0.0 || answer["next_seq"] != 1.0 || answer["lines"] != 0.0 {
			t.Errorf("chunk seven of an empty log was answered %v", answer)
		}

		// And as the first chunk, which is written.
		request["seq"], request["first_line"] = 1, 1
		w, answer = shipped(t, g.handler, credential, clear, request)
		if w.Code != http.StatusOK {
			t.Fatalf("%s as the first chunk answered %d: %s", c.File, w.Code, w.Body)
		}
		if err := conforms(t, "/$defs/logShipment/properties/response", answer); err != nil {
			t.Errorf("%s is not answered as the wire describes: %s: %v", c.File, err, answer)
		}
		if err := conforms(t, "/$defs/logShipment", map[string]any{"request": request, "response": answer}); err != nil {
			t.Errorf("%s and its answer are not the exchange the wire describes: %s", c.File, err)
		}
		if keys := slices.Sorted(maps.Keys(answer)); !slices.Equal(keys, []string{"accepted", "lines", "next_seq", "truncated", "uri"}) {
			t.Errorf("%s is answered with %v", c.File, keys)
		}
		lines := float64(len(request["lines"].([]any)))
		if answer["accepted"] != lines || answer["lines"] != lines || answer["next_seq"] != 2.0 || answer["truncated"] != false {
			t.Errorf("%s as the first chunk was answered %v", c.File, answer)
		}
		if answer["uri"] != "agk://log/"+grantRun+"/"+grantRun+"%2Frender%2F1" {
			t.Errorf("the log of %s is answered at %v", grantKey, answer["uri"])
		}
	}
}
