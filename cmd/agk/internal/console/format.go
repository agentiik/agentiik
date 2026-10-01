package console

import (
	"fmt"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
)

// Took is a length of time as a person reads it at a glance, as the web console writes it: 820ms,
// 22s, 1m 52s, 1h 04m, 3d 02h. The second unit is written on two digits, so that a column of them
// lines up.
func Took(d time.Duration) string {
	if d < 0 {
		return ""
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	s := int64(d / time.Second)
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	m := s / 60
	if m < 60 {
		return fmt.Sprintf("%dm %02ds", m, s%60)
	}
	h := m / 60
	if h < 24 {
		return fmt.Sprintf("%dh %02dm", h, m%60)
	}
	return fmt.Sprintf("%dd %02dh", h/24, h%24)
}

// lasted is how long a run took, or has been going: from its start to its end, or to now while it
// goes. One that never started has lasted nothing anybody would read, and is left empty.
func lasted(r db.RunSummary, now time.Time) string {
	if r.StartedAt.IsZero() {
		return ""
	}
	end := r.FinishedAt
	if end.IsZero() {
		end = now
	}
	return Took(end.Sub(r.StartedAt))
}

// clock is an instant as a screen writes it: the time of day where it is today, the date before it
// otherwise, in UTC, since a terminal reached over SSH is as often in another zone as not.
func clock(at, now time.Time) string {
	if at.IsZero() {
		return ""
	}
	at, now = at.UTC(), now.UTC()
	if at.Year() == now.Year() && at.YearDay() == now.YearDay() {
		return at.Format("15:04:05")
	}
	return at.Format("2006-01-02 15:04")
}

// failing says whether a run is one the runs view lifts into its band: it ended without
// succeeding, which is why the view is opened.
func failing(s agk.RunState) bool {
	return s == agk.Failed || s == agk.TimedOut
}
