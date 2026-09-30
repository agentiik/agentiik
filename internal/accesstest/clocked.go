package accesstest

import "time"

// clocked are the routes whose answer follows the clock where the query leaves it to: a series
// covers the last 24 hours by default, which is another 24 hours at every asking. Two answers unlike
// for the range they cover are unlike for no reason that has to do with access, so a clocked route
// is asked over one range written out.
var clocked = map[string]bool{
	"GET /api/v1/{namespace}/stats/runs": true,
}

// Pinned is the query a route is asked with so that what it answers does not follow the clock: for a
// clocked route, the two days around at, which hold every run a fixture started when at was read,
// and for any other, none. at is read once for every asking that is compared, so that the day it
// falls in is the same for all of them.
func Pinned(method, pattern string, at time.Time) string {
	if !clocked[method+" "+pattern] {
		return ""
	}
	day := at.UTC().Truncate(24 * time.Hour)
	return "?from=" + day.Add(-24*time.Hour).Format(time.RFC3339) + "&to=" + day.Add(24*time.Hour).Format(time.RFC3339)
}
