package main

import (
	"bytes"
	"os"
	"testing"
)

// palette.go is what palettegen writes from the token file vendored beside the web console's: a
// token file copied again and not generated from, or a palette edited by hand, fails here.
func TestThePaletteIsWhatTheTokenFileMakes(t *testing.T) {
	tokens, err := os.ReadFile("../../../../../console/vendor/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	want, err := Generate(tokens)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("../palette.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("cmd/agk/internal/console/palette.go is not what the token file makes: run go generate ./cmd/agk/internal/console")
	}
}

// The nearest of the 256 is the documentation's, for each state on each ground.
func TestTheNearestOfTheTwoHundredAndFiftySixAreTheDocumentations(t *testing.T) {
	for hex, want := range map[string]int{
		"#68AEF5": 75, "#015DA2": 25, // running
		"#5FA773": 72, "#2E7D4F": 29, // succeeded
		"#D9A441": 179, "#96660A": 94, // waiting
		"#E0705A": 167, "#B23C1E": 88, // failed, timed_out
		"#777777": 243, "#888888": 102, // queued, skipped, cancelled
	} {
		rgb, err := parse(hex)
		if err != nil {
			t.Fatal(err)
		}
		if got := nearest(rgb); got != want {
			t.Errorf("the nearest of the 256 to %s is %d, where the documentation gives %d", hex, got, want)
		}
	}
}

func TestATokenNotWrittenAsTheFileWritesThemIsRefused(t *testing.T) {
	for _, tokens := range []string{
		`{"colour":{"dark":{"bg":"#12121"},"light":{"bg":"#FFFFFF"}}}`,
		`{"colour":{"dark":{"bg":"121212"},"light":{"bg":"#FFFFFF"}}}`,
		`{"colour":{"dark":{"bg":"#12121G"},"light":{"bg":"#FFFFFF"}}}`,
		`{"colour":{"dark":{"bg":"#121212"}}}`,
	} {
		if _, err := Generate([]byte(tokens)); err == nil {
			t.Errorf("%s is taken", tokens)
		}
	}
}
