package cron

import (
	"strings"
	"testing"
	"time"
)

func paris(t *testing.T) *time.Location {
	t.Helper()
	loc, err := Zone("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func next(t *testing.T, expr string, after string, loc *time.Location) time.Time {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("%q: %v", expr, err)
	}
	a, err := time.Parse(time.RFC3339, after)
	if err != nil {
		t.Fatal(err)
	}
	return s.Next(a, loc)
}

func is(t *testing.T, got time.Time, want string) {
	t.Helper()
	w, err := time.Parse(time.RFC3339, want)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(w) {
		t.Fatalf("the next occurrence is %s, want %s", got.UTC().Format(time.RFC3339), w.UTC().Format(time.RFC3339))
	}
}

func TestAnOccurrenceTheClocksSkipRunsAtTheFirstInstantAfterTheGap(t *testing.T) {
	// Paris goes from 02:00 CET to 03:00 CEST on 29 March 2026: 02:30 never comes.
	is(t, next(t, "30 2 * * *", "2026-03-28T12:00:00Z", paris(t)), "2026-03-29T01:00:00Z")
}

func TestOccurrencesOneGapSkipsAreOneRun(t *testing.T) {
	loc := paris(t)
	first := next(t, "*/15 2 * * *", "2026-03-28T12:00:00Z", loc)
	is(t, first, "2026-03-29T01:00:00Z")
	// The three other quarters of the missing hour come to the same instant, and are that one.
	is(t, next(t, "*/15 2 * * *", first.Format(time.RFC3339), loc), "2026-03-30T00:00:00Z")
}

func TestAnOccurrenceTheClocksRepeatRunsOnceAtItsFirstInstance(t *testing.T) {
	// Paris goes from 03:00 CEST back to 02:00 CET on 25 October 2026: 02:30 comes twice.
	loc := paris(t)
	first := next(t, "30 2 * * *", "2026-10-24T12:00:00Z", loc)
	is(t, first, "2026-10-25T00:30:00Z")
	is(t, next(t, "30 2 * * *", first.Format(time.RFC3339), loc), "2026-10-26T01:30:00Z")
}

func TestALeapDayComesRound(t *testing.T) {
	is(t, next(t, "0 0 29 2 *", "2026-01-01T00:00:00Z", time.UTC), "2028-02-29T00:00:00Z")
}

func TestBothDayFieldsRestrictedRunOnEither(t *testing.T) {
	// 13 January 2027 is a Wednesday; the Friday before it is the 8th.
	is(t, next(t, "0 0 13 * FRI", "2027-01-06T00:00:00Z", time.UTC), "2027-01-08T00:00:00Z")
	is(t, next(t, "0 0 13 * FRI", "2027-01-11T00:00:00Z", time.UTC), "2027-01-13T00:00:00Z")
}

func TestADayFieldBeginningWithAStarNarrowsTheOther(t *testing.T) {
	is(t, next(t, "0 9 * * MON-FRI", "2026-10-03T00:00:00Z", time.UTC), "2026-10-05T09:00:00Z")
	is(t, next(t, "0 9 * * 7", "2026-10-01T00:00:00Z", time.UTC), "2026-10-04T09:00:00Z")
}

func TestStepsAndListsAndNames(t *testing.T) {
	is(t, next(t, "0 0 1,15 jan-mar *", "2026-02-01T00:00:00Z", time.UTC), "2026-02-15T00:00:00Z")
	is(t, next(t, "10-50/20 * * * *", "2026-02-01T00:30:00Z", time.UTC), "2026-02-01T00:50:00Z")
}

func TestAScheduleReadWithoutAZoneIsReadInUTC(t *testing.T) {
	loc, err := Zone("")
	if err != nil || loc != time.UTC {
		t.Fatalf("an empty zone is %v, %v, want UTC", loc, err)
	}
	if _, err := Zone("Local"); err == nil {
		t.Fatal("Local was accepted, and it is the installation's own zone")
	}
	if _, err := Zone("Mars/Olympus_Mons"); err == nil {
		t.Fatal("a zone the database does not hold was accepted")
	}
}

func TestWhatIsNotFiveCronFieldsIsRefusedSayingWhy(t *testing.T) {
	for expr, says := range map[string]string{
		"0 6 1 *":         "five",
		"0 6 ? * *":       "day of the month ?",
		"60 * * * *":      "minute 60 is outside 0 to 59",
		"0 0 30 2 *":      "names no day that comes",
		"5/15 * * * *":    "write 5-59/15",
		"0 0 * * 8":       "day of the week 8",
		"*/0 * * * *":     "step /0",
		"10-5 * * * *":    "runs backwards",
		"0 0 * JANUARY *": "JANUARY is neither a number nor one of JAN",
		"0 0 1 * * 2027":  "is 6 fields",
	} {
		_, err := Parse(expr)
		if err == nil {
			t.Errorf("%q was accepted", expr)
			continue
		}
		if !strings.Contains(err.Error(), says) {
			t.Errorf("%q was refused with %q, which does not say %q", expr, err, says)
		}
	}
}
