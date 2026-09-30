// Package cron reads a schedule's five-field expression and finds its occurrences in a named
// time zone.
//
// The grammar is the one the Triggers chapter spells out: the minute, the hour, the day of
// the month, the month and the day of the week; a field is *, a value, a range a-b, * or a
// range stepped by /n, or a list of those joined by commas; a month may be written JAN to DEC
// and a day of the week SUN to SAT, with 0 and 7 both Sunday. Where both day fields are
// restricted, neither beginning with *, a day matching either one runs, as in every cron since
// Vixie's; where one begins with *, a day has to match both. There is no sixth field, no ? or
// L and no @daily.
//
// Occurrences are wall-clock times in the schedule's zone. One that a daylight saving change
// skips, 02:30 on the night clocks go forward, runs at the first instant after the gap; one a
// change repeats, 02:30 on the night they go back, runs once, at its first instance. Two wall
// times that come to the same instant, two skipped by one gap, are one occurrence.
//
// Standard library only: it is read by the graph package, which stays an importable library,
// and by the controller that fires schedules, and both have to read an expression alike.
package cron

import (
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"time"

	// The zone database is embedded rather than read from the host alone, so that a schedule
	// naming Europe/Paris is accepted by agk validate on a laptop, by the hook on a server and
	// by a controller in a distroless image alike. A host's own database still answers first
	// where it has one, as time.LoadLocation reads it.
	_ "time/tzdata"
)

// Schedule is a parsed expression: one set of accepted values per field.
type Schedule struct {
	minute, hour, dom, month, dow uint64

	// domStar and dowStar are whether each day field begins with *. They decide how the two
	// combine: both restricted is either, and otherwise both.
	domStar, dowStar bool

	text string
}

// field is one of the five, with the values it counts and the names it accepts.
type field struct {
	name     string
	min, max int
	names    []string
}

var fields = [5]field{
	{name: "minute", min: 0, max: 59},
	{name: "hour", min: 0, max: 23},
	{name: "day of the month", min: 1, max: 31},
	{name: "month", min: 1, max: 12, names: []string{"", "JAN", "FEB", "MAR", "APR", "MAY", "JUN", "JUL", "AUG", "SEP", "OCT", "NOV", "DEC"}},
	// 7 is read as Sunday as well as 0, which is what lets MON-SUN and 1-7 be written.
	{name: "day of the week", min: 0, max: 7, names: []string{"SUN", "MON", "TUE", "WED", "THU", "FRI", "SAT"}},
}

// Parse reads a five-field expression. A refusal says which field and why, in the words a
// person holding the file can act on.
func Parse(expr string) (*Schedule, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, fmt.Errorf("%q is %d fields, and a schedule is five: the minute, the hour, the day of the month, the month and the day of the week", expr, len(parts))
	}
	s := &Schedule{text: expr}
	sets := [5]*uint64{&s.minute, &s.hour, &s.dom, &s.month, &s.dow}
	for i, part := range parts {
		set, err := fields[i].parse(part)
		if err != nil {
			return nil, err
		}
		*sets[i] = set
	}
	// Sunday is one day whichever number wrote it.
	if s.dow&(1<<7) != 0 {
		s.dow = s.dow&^(1<<7) | 1
	}
	s.domStar = strings.HasPrefix(parts[2], "*")
	s.dowStar = strings.HasPrefix(parts[4], "*")
	if err := s.somedayExists(); err != nil {
		return nil, err
	}
	return s, nil
}

// String is the expression as written.
func (s *Schedule) String() string { return s.text }

func (f field) parse(text string) (uint64, error) {
	var set uint64
	for _, item := range strings.Split(text, ",") {
		bits, err := f.item(item)
		if err != nil {
			return 0, err
		}
		set |= bits
	}
	return set, nil
}

// item reads one element of a list: *, a value, or a range, * and a range optionally stepped.
func (f field) item(item string) (uint64, error) {
	body, stepText, stepped := strings.Cut(item, "/")
	lo, hi := f.min, f.max
	switch {
	case body == "*":
	case strings.Contains(body, "-"):
		a, b, _ := strings.Cut(body, "-")
		var err error
		if lo, err = f.value(a); err != nil {
			return 0, err
		}
		if hi, err = f.value(b); err != nil {
			return 0, err
		}
		if lo > hi {
			return 0, fmt.Errorf("the %s range %s runs backwards: a range is written from its low end to its high end", f.name, body)
		}
	default:
		if stepped {
			// 5/15 is read as 5-59/15 by some crons and refused by others, so it is refused
			// here and written as the range it means.
			return 0, fmt.Errorf("the %s %s is a value stepped by /%s: a step follows * or a range, so write %s-%d/%s for what it means", f.name, item, stepText, body, f.max, stepText)
		}
		v, err := f.value(body)
		if err != nil {
			return 0, err
		}
		lo, hi = v, v
	}
	step := 1
	if stepped {
		n, err := strconv.Atoi(stepText)
		if err != nil || n < 1 || n > f.max-f.min+1 {
			return 0, fmt.Errorf("the %s step /%s is not a step from 1 to %d, the values the field counts", f.name, stepText, f.max-f.min+1)
		}
		step = n
	}
	var set uint64
	for v := lo; v <= hi; v += step {
		set |= 1 << uint(v)
	}
	return set, nil
}

// value reads a number, or a name where the field has names, and holds it to the field.
func (f field) value(text string) (int, error) {
	if text == "" {
		return 0, fmt.Errorf("the %s field holds an empty value", f.name)
	}
	for i, name := range f.names {
		if name != "" && strings.EqualFold(text, name) {
			return i, nil
		}
	}
	v, err := strconv.Atoi(text)
	if err != nil {
		if f.names != nil {
			return 0, fmt.Errorf("the %s %s is neither a number nor one of %s", f.name, text, strings.Join(nonEmpty(f.names), ", "))
		}
		return 0, fmt.Errorf("the %s %s is not a number", f.name, text)
	}
	if v < f.min || v > f.max {
		return 0, fmt.Errorf("the %s %d is outside %d to %d, the values the field counts", f.name, v, f.min, f.max)
	}
	return v, nil
}

func nonEmpty(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}

// longestMonth is the days each month can have, February's leap day included.
var longestMonth = [13]int{0, 31, 29, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}

// somedayExists refuses an expression that names no day that comes, the 30th of February
// alone, which would be accepted and never fire. It only arises where the day of the week
// does not widen the days, since a restricted weekday matches some day of every month.
func (s *Schedule) somedayExists() error {
	if !(s.domStar || s.dowStar) {
		return nil // either field matching is enough, and some weekday falls in every month
	}
	if !s.dowStar {
		return nil // the day of the month is *, and the weekday picks days of every month
	}
	for m := 1; m <= 12; m++ {
		if s.month&(1<<uint(m)) == 0 {
			continue
		}
		reachable := uint64(1)<<uint(longestMonth[m]+1) - 2 // days 1..longest
		if s.dom&reachable != 0 {
			return nil
		}
	}
	return fmt.Errorf("%q names no day that comes: none of the days of the month it names is in any of the months it names", s.text)
}

// dayMatches says whether the schedule runs on the date d.
func (s *Schedule) dayMatches(d time.Time) bool {
	dom := s.dom&(1<<uint(d.Day())) != 0
	dow := s.dow&(1<<uint(d.Weekday())) != 0
	if s.domStar || s.dowStar {
		return dom && dow
	}
	return dom || dow
}

// horizon bounds how far Next looks, in days. Twenty-nine years is past the longest wait an
// expression Parse accepts can have: a leap day on one weekday comes round every twenty-eight
// years, and a century's missing leap day adds four.
const horizon = 29 * 366

// Next is the first occurrence strictly after after, read in loc. The zero time means none in
// the horizon, which Parse does not let happen.
func (s *Schedule) Next(after time.Time, loc *time.Location) time.Time {
	local := after.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	for i := 0; i < horizon; i++ {
		d := day.AddDate(0, 0, i)
		if s.month&(1<<uint(d.Month())) == 0 || !s.dayMatches(d) {
			continue
		}
		for h := bits.TrailingZeros64(s.hour); h < 24; h++ {
			if s.hour&(1<<uint(h)) == 0 {
				continue
			}
			for m := bits.TrailingZeros64(s.minute); m < 60; m++ {
				if s.minute&(1<<uint(m)) == 0 {
					continue
				}
				if at := Resolve(d.Year(), d.Month(), d.Day(), h, m, loc); at.After(after) {
					return at
				}
			}
		}
	}
	return time.Time{}
}

// Resolve is the instant a wall-clock time in loc stands for: itself where it exists once;
// its first instance where a change back repeats it; and the first instant after the gap
// where a change forward skips it.
func Resolve(year int, month time.Month, day, hour, minute int, loc *time.Location) time.Time {
	t := time.Date(year, month, day, hour, minute, 0, 0, loc)
	want := time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
	got := wall(t)
	switch {
	case got.Equal(want):
		// It exists. time.Date may have given either instance of a repeated time, so look
		// for an earlier one in the zone before this one.
		start, _ := t.ZoneBounds()
		if start.IsZero() {
			return t
		}
		_, before := start.Add(-time.Nanosecond).Zone()
		_, now := t.Zone()
		if before > now {
			earlier := t.Add(-time.Duration(before-now) * time.Second)
			if earlier.Before(start) && wall(earlier).Equal(want) {
				return earlier
			}
		}
		return t
	case got.After(want):
		// Normalised forward, into the zone after the gap: the gap ends where it starts.
		start, _ := t.ZoneBounds()
		return start
	default:
		// Normalised backward, into the zone before the gap: the gap ends where it ends.
		_, end := t.ZoneBounds()
		return end
	}
}

// wall is t's clock reading in its own zone, as an instant in UTC so that two readings compare.
func wall(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

// Zone reads a schedule's time zone: UTC where none is written, and never the installation's
// own, since one file would then fire at different instants on two installations.
func Zone(name string) (*time.Location, error) {
	switch name {
	case "":
		return time.UTC, nil
	case "Local":
		return nil, fmt.Errorf("the zone Local is the installation's own, and one file would fire at different instants on two installations: name the zone, Europe/Paris, or write UTC")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("the zone %s is not in the IANA time zone database, which names zones as Europe/Paris and America/Argentina/Buenos_Aires", name)
	}
	return loc, nil
}
