package graph

import (
	"testing"
	"time"
)

// TestADurationIsOneNumberAndOneUnit holds the grammar the language states: "a duration,
// anywhere in the file, is one number and one unit taken from ms, s, m, h and d".
func TestADurationIsOneNumberAndOneUnit(t *testing.T) {
	for text, want := range map[string]time.Duration{
		"500ms": 500 * time.Millisecond,
		"2s":    2 * time.Second,
		"10m":   10 * time.Minute,
		"24h":   24 * time.Hour,
		"90d":   90 * 24 * time.Hour,
		"0s":    0,
	} {
		got, err := ParseDuration(text)
		if err != nil {
			t.Errorf("%s was refused: %v", text, err)
			continue
		}
		if time.Duration(got) != want {
			t.Errorf("%s read as %s, want %s", text, time.Duration(got), want)
		}
	}
}

// TestACompoundAndABareNumberAreRefused holds the other half of the same sentence: "a
// compound such as 1h30m is refused, and so is a bare number".
func TestACompoundAndABareNumberAreRefused(t *testing.T) {
	for _, text := range []string{"1h30m", "600", "", "10", "2w", "10 m", "-5s", "s", "1.5h", "10M"} {
		if _, err := ParseDuration(text); err == nil {
			t.Errorf("%q was read as a duration", text)
		}
	}
}

// TestADurationIsWrittenInASCIIDigits holds the number to the grammar the released schema writes
// a duration on, ^[0-9]+(ms|s|m|h|d)$: digits and nothing else, so no sign, which a server took
// while workflow.schema refused it, and leading zeros, which the schema takes too.
func TestADurationIsWrittenInASCIIDigits(t *testing.T) {
	for _, text := range []string{"+5m", "+5s", "-0s", "+0s", "\u0665s", "5_0s", "0x10s"} {
		if _, err := ParseDuration(text); err == nil {
			t.Errorf("%q was read as a duration, and the schema refuses it", text)
		}
	}
	if d, err := ParseDuration("0120s"); err != nil || time.Duration(d) != 120*time.Second {
		t.Errorf("0120s, which the schema takes, was read as %s: %v", time.Duration(d), err)
	}
}

// TestADurationIsWrittenBackTheWayItWasWritten keeps a file readable after a round trip:
// 90d comes back as 90d and not as 2160h.
func TestADurationIsWrittenBackTheWayItWasWritten(t *testing.T) {
	for _, text := range []string{"500ms", "2s", "10m", "4h", "90d"} {
		d, err := ParseDuration(text)
		if err != nil {
			t.Fatal(err)
		}
		if got := d.String(); got != text {
			t.Errorf("%s was written back as %s", text, got)
		}
	}
}
