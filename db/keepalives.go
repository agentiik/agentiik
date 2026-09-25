package db

import "strings"

// Keepalives are the settings every connection of the API and the controller asks its server for,
// as run-time parameters of the session, unless the URL sets them itself.
//
// Two things are held by a session and released only when PostgreSQL notices it is gone: the
// controller's advisory lock, and the head of the audit log's chain, which an act locks from its
// append until it commits. A program that died or was cut off without a reset leaves nothing on the
// wire to notice, and the server's own default is the operating system's, which on Linux probes an
// idle connection after two hours. For all that time no standby could take over, and every audited
// act of the installation, a runner's revocation among them, would wait behind the one whose commit
// never came. With these the server probes a silent session after ten seconds and drops it after
// three unanswered probes five seconds apart, so both are released within half a minute.
var Keepalives = []struct{ Name, Value string }{
	{"tcp_keepalives_idle", "10"},
	{"tcp_keepalives_interval", "5"},
	{"tcp_keepalives_count", "3"},
}

// WithKeepalives adds the keepalives to a PostgreSQL URL that does not already set them.
//
// Added to the text rather than through net/url, which drops a parameter holding a ; without a
// word and would lose a setting the operator wrote. pgx takes a parameter it has no use for as a
// run-time parameter of the session, which is how these reach the server.
func WithKeepalives(conn string) string {
	_, query, _ := strings.Cut(conn, "?")
	set := map[string]bool{}
	for pair := range strings.SplitSeq(query, "&") {
		key, _, _ := strings.Cut(pair, "=")
		set[strings.TrimSpace(key)] = true
	}
	separator := "&"
	switch {
	case !strings.Contains(conn, "?"):
		separator = "?"
	case strings.HasSuffix(conn, "?") || strings.HasSuffix(conn, "&"):
		separator = ""
	}
	for _, k := range Keepalives {
		if !set[k.Name] {
			conn += separator + k.Name + "=" + k.Value
			separator = "&"
		}
	}
	return conn
}
