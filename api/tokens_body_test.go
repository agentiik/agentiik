package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A token's request is read as every other body is: closed, counted, and at no more than two and a
// half times its cap, which is the cap of the API's small bodies.

// A request for a token costs what any small body costs to read, however it fills its cap: with
// empty entries in either half of its scope, or with one long label.
func TestATokensRequestCostsNoMoreThanASmallBody(t *testing.T) {
	const costsAtMost = 2.5
	empty := func(int) string { return `""` }
	for _, c := range []struct {
		name string
		body []byte
	}{
		{"a scope of empty permissions", filled(`{"scope":{"permissions":[`, `]}}`, smallMaxBytes, empty)},
		{"a scope reaching empty names", filled(`{"scope":{"within":[`, `]}}`, smallMaxBytes, empty)},
		{"a label as long as the body", []byte(`{"device_label":"` + strings.Repeat("A", smallMaxBytes-20) + `"}`)},
	} {
		if int64(len(c.body)) > smallMaxBytes || int64(len(c.body)) < smallMaxBytes-64 {
			t.Fatalf("%s is %d bytes, and it is meant to fill its cap of %d", c.name, len(c.body), smallMaxBytes)
		}
		for how, ask := range map[string]func() *http.Request{"declaring its length": declaring(c.body), "in chunks": chunked(c.body)} {
			n, _ := spent(ask, func() request { return new(TokenRequest) }, smallMaxBytes)
			if cost := float64(n) / smallMaxBytes; cost > costsAtMost {
				t.Errorf("%s, %s, costs %d bytes to read, %.1f times its cap", c.name, how, n, cost)
			}
		}
	}
}

// A request reads back what encoding/json writes of it, field for field, as agk writes it.
func TestATokensRequestReadsWhatEncodingJSONWrites(t *testing.T) {
	expires := time.Date(2027, 3, 26, 9, 10, 0, 0, time.UTC)
	for _, want := range []*TokenRequest{
		{},
		{DeviceLabel: "agk on alice-laptop"},
		{
			Principal: "finance/nightly-sync", DeviceLabel: "terraform, finance environment", ExpiresAt: &expires,
			Scope: &TokenScope{Permissions: []string{"workflow:run"}, Within: []string{"finance/monthly-invoicing"}},
		},
		{Scope: &TokenScope{Within: []string{"finance"}}},
	} {
		encoded, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		got := new(TokenRequest)
		if err := readAtMost(httptest.NewRequest("POST", "/", bytes.NewReader(encoded)), got, smallMaxBytes); err != nil {
			t.Errorf("%s is refused: %s", encoded, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s reads back as %+v", encoded, got)
		}
	}
}
