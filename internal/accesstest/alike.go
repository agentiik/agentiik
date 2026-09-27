package accesstest

import (
	"bytes"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"
)

// How two answers are held alike, in what they say and in how long they take to say it: an answer
// about something that does not exist and one about something the asker may not reach are to be the
// same, or the difference is the oracle a 403 would be.

// Difference says how an answer differs from the one absence got, and is empty where it does not:
// status, headers and body, byte for byte and header for header. The one header left out is Date,
// which net/http writes on every answer it serves and which reads the second it was served in.
func Difference(absence, a Answer) string {
	switch {
	case absence.Status != a.Status:
		return fmt.Sprintf("%d, and absence %d", a.Status, absence.Status)
	case !maps.EqualFunc(asWritten(absence.Header), asWritten(a.Header), slices.Equal):
		return fmt.Sprintf("the headers %v, and absence %v", a.Header, absence.Header)
	case !bytes.Equal(absence.Body, a.Body):
		return fmt.Sprintf("%q, and absence %q", a.Body, absence.Body)
	}
	return ""
}

// asWritten is a header as the API wrote it, without the date net/http adds.
func asWritten(h http.Header) http.Header {
	h = h.Clone()
	h.Del("Date")
	return h
}

// TimingVariable asks for timings to be measured: a machine under other load measures the load as
// much as the answers, so a test measures where somebody asks for numbers, and says it skipped
// otherwise.
const TimingVariable = "AGENTIIK_TEST_TIMING"

// Timing is how long each asking about what does not exist and about what does took, each sorted.
type Timing struct {
	Absent, Present []time.Duration
}

// warming is how many askings of each come before those measured, which find caches and
// connections as every later one finds them.
const warming = 20

// TakeAsLong asks absent and present in turn, so that the two meet the same machine, warming times
// and then rounds times, and answers how long the measured ones took.
func TakeAsLong(rounds int, absent, present func()) Timing {
	var m Timing
	for i := range rounds + warming {
		for j, ask := range []func(){absent, present} {
			began := time.Now()
			ask()
			if i < warming {
				continue
			}
			if j == 0 {
				m.Absent = append(m.Absent, time.Since(began))
			} else {
				m.Present = append(m.Present, time.Since(began))
			}
		}
	}
	slices.Sort(m.Absent)
	slices.Sort(m.Present)
	return m
}

// quantile is the q-th quantile of sorted durations.
func quantile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(float64(len(sorted))*q)]
}

// Medians are the two medians, absence's first.
func (m Timing) Medians() (absent, present time.Duration) {
	return quantile(m.Absent, 0.5), quantile(m.Present, 0.5)
}

// Alike says whether the two medians are within a fifth of absence's of each other, or 200 µs,
// whichever is more: close enough that a caller measuring cannot tell the two apart over the noise
// of a network, and far enough apart to catch a question asked of one and not the other, which cost
// the refusal of a run half as much again, and a listing ten times as much, before they were
// asked alike.
func (m Timing) Alike() bool {
	absent, present := m.Medians()
	return (present - absent).Abs() <= max(200*time.Microsecond, absent/5)
}

func (m Timing) String() string {
	micro := func(d time.Duration) float64 { return float64(d) / float64(time.Microsecond) }
	return fmt.Sprintf("absent %6.0fµs p90 %6.0fµs | present %6.0fµs p90 %6.0fµs",
		micro(quantile(m.Absent, 0.5)), micro(quantile(m.Absent, 0.9)), micro(quantile(m.Present, 0.5)), micro(quantile(m.Present, 0.9)))
}
