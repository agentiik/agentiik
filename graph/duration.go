package graph

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Duration is a duration as the workflow language writes it: "one number and one unit
// taken from ms, s, m, h and d". It is a time.Duration underneath, so that a caller
// computing a deadline converts and does not parse.
//
// The grammar is narrow on purpose. "A compound such as 1h30m is refused, and so is a
// bare number", because a file that says 10m where a reader would otherwise have to work
// out what 600 means is the whole reason the unit is written at all. timeout, retain,
// the schedule jitter and both backoff values are all written that way.
type Duration time.Duration

// The units the language carries, longest spelling first so that ms is read before s.
var durationUnits = []struct {
	suffix string
	unit   time.Duration
}{
	{"ms", time.Millisecond},
	{"s", time.Second},
	{"m", time.Minute},
	{"h", time.Hour},
	{"d", 24 * time.Hour},
}

// ParseDuration reads a duration written the way the language writes it.
//
// A day is twenty-four hours here, which is what a retention of 90d and a run timeout of
// 4h mean beside each other. No calendar is involved: the engine is measuring how long
// something may take or stay, not what the date will be.
func ParseDuration(s string) (Duration, error) {
	for _, u := range durationUnits {
		digits, ok := strings.CutSuffix(s, u.suffix)
		if !ok || digits == "" {
			continue
		}
		n, err := strconv.ParseInt(digits, 10, 64)
		if err != nil || n < 0 {
			return 0, durationRefusal(s)
		}
		return Duration(time.Duration(n) * u.unit), nil
	}
	return 0, durationRefusal(s)
}

func durationRefusal(s string) error {
	return fmt.Errorf("%q is not a duration: a duration is one number and one unit taken from ms, s, m, h and d, so 500ms, 2s, 10m, 24h and 90d are durations and a compound such as 1h30m is refused, and so is a bare number", s)
}

// String writes the duration back the way the file would write it: the largest unit it
// divides exactly by, so that a duration read from a file and written back out is the
// text the author wrote.
func (d Duration) String() string {
	v := time.Duration(d)
	if v == 0 {
		return "0s"
	}
	for i := len(durationUnits) - 1; i >= 0; i-- {
		u := durationUnits[i]
		if v%u.unit == 0 {
			return strconv.FormatInt(int64(v/u.unit), 10) + u.suffix
		}
	}
	// Nothing divides a duration below a millisecond, which the grammar cannot write.
	// It can only arrive from a caller that built one, and saying so is better than
	// rounding it away.
	return v.String()
}
