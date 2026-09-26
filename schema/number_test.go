package schema

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

// An exponent is written out with a point, digit for digit, so that PostgreSQL, which writes an
// exponent out without one, hands back a double; every other number is left as it was written.
func TestAnExponentIsWrittenOutAndEveryOtherNumberLeftAsWritten(t *testing.T) {
	for written, want := range map[json.Number]json.Number{
		"3":                        "3",
		"-0":                       "-0",
		"2.0":                      "2.0",
		"12.50":                    "12.50",
		"9007199254740993":         "9007199254740993",
		"1e3":                      "1000.0",
		"1E+3":                     "1000.0",
		"-2.5e1":                   "-25.0",
		"1.25e1":                   "12.5",
		"15e-1":                    "1.5",
		"1e-3":                     "0.001",
		"-1.5e-2":                  "-0.015",
		"1.00000000000000000001e3": "1000.00000000000000001",
		"0e5":                      "0.0",
		"-0.0":                     "0.0",
		"-0.00e3":                  "0.0",
		"-0.5":                     "-0.5",
	} {
		if got := Canonical(written); got != want {
			t.Errorf("%s is written %s, want %s", written, got, want)
		}
	}
}

// A map or a list is walked and copied, so that the value a caller handed in is not written into.
func TestCanonicalCopiesWhatItWalks(t *testing.T) {
	in := map[string]any{"n": json.Number("1e1"), "list": []any{json.Number("2e0"), "text", true, nil}}
	got := Canonical(in)
	want := map[string]any{"n": json.Number("10.0"), "list": []any{json.Number("2.0"), "text", true, nil}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the value is written %#v, want %#v", got, want)
	}
	if in["n"] != json.Number("1e1") {
		t.Errorf("the value handed in was written into: %#v", in)
	}
}

// A float64, which is how a YAML loader answers a number written with a point, is a double
// however whole it is.
func TestAFloatIsADoubleHoweverWholeItIs(t *testing.T) {
	for f, want := range map[float64]json.Number{
		1:      "1.0",
		2.5:    "2.5",
		1e21:   "1000000000000000000000.0",
		1.5e-7: "0.00000015",
		// Minus zero is written as PostgreSQL gives it back, so that a local run and a server
		// divide by the same zero.
		math.Copysign(0, -1): "0.0",
	} {
		if got := Double(f); got != want {
			t.Errorf("%v is written %s, want %s", f, got, want)
		}
		if _, err := Double(f).Int64(); err == nil {
			t.Errorf("%v is written %s, which an expression would take as an int", f, Double(f))
		}
	}
	if got := Double(math.Inf(1)); got != "+Inf" {
		t.Errorf("infinity is written %s, and it is left for whatever writes it down to refuse", got)
	}
}
