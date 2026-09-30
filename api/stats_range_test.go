package api

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/db"
)

// A range is read as openapi.json describes it: from included and to excluded, the last 24 hours
// where both are left out, a bucket following the range where none is asked for, and buckets on
// whole minutes, quarters, hours and days in UTC, the first holding from and the last the instant
// before to.
func TestAStatsRangeIsReadAsDocumented(t *testing.T) {
	now := time.Date(2026, 9, 30, 8, 7, 30, 0, time.UTC)
	at := func(s string) time.Time {
		t.Helper()
		v, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return v.UTC()
	}
	for _, c := range []struct {
		query    string
		from, to string
		bucket   string
		first    string
		count    int
	}{
		// The last 24 hours, in quarters, the first holding from and the last the instant before now.
		{"", "2026-09-29T08:07:30Z", "2026-09-30T08:07:30Z", "15m", "2026-09-29T08:00:00Z", 97},
		// 24 hours before to, where from is left out.
		{"to=2026-09-30T06:00:00Z", "2026-09-29T06:00:00Z", "2026-09-30T06:00:00Z", "15m", "2026-09-29T06:00:00Z", 96},
		// Up to now, where to is left out, and a bucket a minute up to two hours.
		{"from=2026-09-30T06:07:30Z", "2026-09-30T06:07:30Z", "2026-09-30T08:07:30Z", "1m", "2026-09-30T06:07:00Z", 121},
		{"from=2026-09-30T06:00:00Z&to=2026-09-30T08:00:00Z", "2026-09-30T06:00:00Z", "2026-09-30T08:00:00Z", "1m", "2026-09-30T06:00:00Z", 120},
		{"from=2026-09-28T08:00:00Z&to=2026-09-30T08:00:00Z", "2026-09-28T08:00:00Z", "2026-09-30T08:00:00Z", "15m", "2026-09-28T08:00:00Z", 192},
		{"from=2026-09-28T08:00:00Z&to=2026-09-30T08:00:01Z", "2026-09-28T08:00:00Z", "2026-09-30T08:00:01Z", "1h", "2026-09-28T08:00:00Z", 49},
		{"from=2026-09-16T00:00:00Z&to=2026-09-30T00:00:00Z", "2026-09-16T00:00:00Z", "2026-09-30T00:00:00Z", "1h", "2026-09-16T00:00:00Z", 336},
		{"from=2026-08-31T12:00:00Z&to=2026-09-30T12:00:00Z", "2026-08-31T12:00:00Z", "2026-09-30T12:00:00Z", "1d", "2026-08-31T00:00:00Z", 31},
		// A bucket asked for wins over the range, and an offset is read as the instant it names.
		{"from=2026-09-30T08:00:00%2B02:00&to=2026-09-30T08:00:00Z&bucket=1h", "2026-09-30T06:00:00Z", "2026-09-30T08:00:00Z", "1h", "2026-09-30T06:00:00Z", 2},
		// A day falls on midnight in UTC.
		{"from=2026-09-29T23:30:00Z&to=2026-09-30T00:30:00Z&bucket=1d", "2026-09-29T23:30:00Z", "2026-09-30T00:30:00Z", "1d", "2026-09-29T00:00:00Z", 2},
		// The most buckets a series takes.
		{"from=2026-09-30T00:00:00Z&to=2026-09-30T16:40:00Z&bucket=1m", "2026-09-30T00:00:00Z", "2026-09-30T16:40:00Z", "1m", "2026-09-30T00:00:00Z", 1000},
	} {
		query, err := url.ParseQuery(c.query)
		if err != nil {
			t.Fatal(err)
		}
		got, err := readStatsRange(query, now, true)
		if err != nil {
			t.Errorf("%q was refused: %v", c.query, err)
			continue
		}
		want := statsRange{From: at(c.from), To: at(c.to), Bucket: c.bucket,
			Buckets: db.Buckets{First: at(c.first), Width: statsBuckets[c.bucket], Count: c.count}}
		if !got.From.Equal(want.From) || !got.To.Equal(want.To) || got.Bucket != want.Bucket ||
			!got.Buckets.First.Equal(want.Buckets.First) || got.Buckets.Width != want.Buckets.Width || got.Buckets.Count != want.Buckets.Count {
			t.Errorf("%q was read as %+v, want %+v", c.query, got, want)
		}
	}
}

// What a range adds is read too: the span before, shifted by whole buckets so that its buckets fall
// on the same boundaries and end where the range's first begins, and a histogram of 1 to 100 bins.
func TestAStatsRangeAddsTheSpanBeforeAndAHistogram(t *testing.T) {
	now := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		query          string
		from, to       string
		first          string
		histogram      int
		previous       bool
		previousBucket string
	}{
		{"from=2026-09-30T06:00:00Z&to=2026-09-30T08:00:00Z&bucket=1h&compare=previous&histogram=12",
			"2026-09-30T04:00:00Z", "2026-09-30T06:00:00Z", "2026-09-30T04:00:00Z", 12, true, "1h"},
		// A range off the boundaries is shifted by its three buckets rather than by its length, so
		// that the span before ends at or before from and never shares a bucket with the range.
		{"from=2026-09-30T06:30:00Z&to=2026-09-30T08:10:00Z&bucket=1h&compare=previous&histogram=100",
			"2026-09-30T03:30:00Z", "2026-09-30T05:10:00Z", "2026-09-30T03:00:00Z", 100, true, "1h"},
		{"from=2026-09-30T06:00:00Z&to=2026-09-30T08:00:00Z&histogram=1", "", "", "", 1, false, ""},
	} {
		query, err := url.ParseQuery(c.query)
		if err != nil {
			t.Fatal(err)
		}
		got, err := readStatsRange(query, now, true)
		if err != nil {
			t.Fatalf("%q was refused: %v", c.query, err)
		}
		bins, err := readHistogram(query)
		if err != nil {
			t.Fatalf("%q was refused: %v", c.query, err)
		}
		if bins != c.histogram || got.Previous != c.previous {
			t.Errorf("%q asked for a histogram of %d and the span before %v, want %d and %v", c.query, bins, got.Previous, c.histogram, c.previous)
		}
		if !c.previous {
			continue
		}
		from, to, b := got.before()
		if stamp(from) != c.from || stamp(to) != c.to || stamp(b.First) != c.first || b.Count != got.Buckets.Count || b.Width != got.Buckets.Width {
			t.Errorf("%q had the span before from %s to %s in %d buckets from %s, want from %s to %s in %d from %s",
				c.query, stamp(from), stamp(to), b.Count, stamp(b.First), c.from, c.to, got.Buckets.Count, c.first)
		}
		if !b.End().Equal(got.Buckets.First) {
			t.Errorf("%q had the span before end at %s, and the range's first bucket begins at %s", c.query, stamp(b.End()), stamp(got.Buckets.First))
		}
	}
}

// A query openapi.json refuses is refused with a reason naming what was wrong.
func TestAStatsRangeThatIsNoneIsRefused(t *testing.T) {
	now := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		query, names string
	}{
		{"from=yesterday", "from"},
		{"to=2026-09-30", "to"},
		{"from=2026-09-30T08:00:00Z&to=2026-09-30T08:00:00Z", "later one"},
		{"from=2026-09-30T09:00:00Z&to=2026-09-30T08:00:00Z", "later one"},
		{"from=2026-09-30T09:00:00Z", "later one"},
		{"bucket=5m", "bucket"},
		{"bucket=1H", "bucket"},
		{"from=2026-09-30T00:00:00Z&to=2026-09-30T16:40:00.000000001Z&bucket=1m", "1001 buckets"},
		{"from=2026-09-01T00:00:00Z&to=2026-09-30T00:00:00Z&bucket=1m", "at most 1000"},
		{"from=0001-01-01T00:00:00Z&to=9999-12-31T00:00:00Z", "at most 1000"},
		{"compare=next", "compare"},
		{"histogram=0", "histogram"},
		{"histogram=101", "histogram"},
		{"histogram=twelve", "histogram"},
		{"histogram=-3", "histogram"},
	} {
		query, err := url.ParseQuery(c.query)
		if err != nil {
			t.Fatal(err)
		}
		_, err = readStatsRange(query, now, true)
		if err == nil {
			_, err = readHistogram(query)
		}
		switch {
		case err == nil:
			t.Errorf("%q was read as a range", c.query)
		case !strings.Contains(err.Error(), c.names):
			t.Errorf("%q was refused with %q, which does not say %q", c.query, err, c.names)
		}
	}
}

// CSV is answered where Accept ranks text/csv above application/json, each weighed by the most
// specific range naming it, and JSON everywhere else, a tie and no Accept at all included.
func TestAStatsSeriesIsCSVOnlyWhereAcceptPrefersIt(t *testing.T) {
	for accept, csv := range map[string]bool{
		"":                                   false,
		"text/csv":                           true,
		"text/csv; charset=utf-8":            true,
		"TEXT/CSV":                           true,
		"application/json":                   false,
		"*/*":                                false,
		"text/*":                             true,
		"text/csv, application/json":         false,
		"text/csv, application/json;q=0.9":   true,
		"text/csv;q=0.5, application/json":   false,
		"text/csv;q=0, */*":                  false,
		"text/*, application/json;q=0.2":     true,
		"*/*;q=0.1, text/csv":                true,
		"text/html, application/xhtml+xml":   false,
		"text/csv;q=nonsense, */*;q=0.5":     true,
		"application/json;q=0, text/csv;q=0": false,
		"not a media type, text/csv":         true,
	} {
		if got := wantsCSV(accept); got != csv {
			t.Errorf("Accept %q was answered in CSV %v, want %v", accept, got, csv)
		}
	}
}

// A range read for a heatmap of the hours of a week takes no buckets: a bucket is refused where it
// is none and bounds nothing where it is one, so a year of minutes is a range, and the span before
// is not taken.
func TestAHeatmapsRangeTakesNoBuckets(t *testing.T) {
	now := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	query, err := url.ParseQuery("from=2025-09-30T00:00:00Z&to=2026-09-30T00:00:00Z&bucket=1m&compare=previous")
	if err != nil {
		t.Fatal(err)
	}
	got, err := readStatsRange(query, now, false)
	if err != nil {
		t.Fatalf("a year of minutes was refused for a heatmap: %v", err)
	}
	if got.Bucket != "" || got.Buckets.Count != 0 || got.Previous {
		t.Errorf("a heatmap's range was read as %+v, with buckets or the span before", got)
	}
	for _, refused := range []string{"bucket=5m", "compare=next", "from=yesterday", "from=2026-09-30T09:00:00Z"} {
		query, err := url.ParseQuery(refused)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := readStatsRange(query, now, false); err == nil {
			t.Errorf("%q was read as a heatmap's range", refused)
		}
	}
}
