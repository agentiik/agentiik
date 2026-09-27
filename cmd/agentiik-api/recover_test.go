package main

import (
	"bytes"
	"crypto/sha256"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/internal/config"
)

// recoveryLink is the link recover prints, on the public URL it read, alone on its line.
var recoveryLink = regexp.MustCompile(`(?m)^https://agentiik\.example\.com/auth/enrol#(agkenrol_[A-Za-z0-9_-]{43,})$`)

// The break-glass path: recover issues an administrator a recovery code with no credential, as the
// installation, prints the link that carries it once, records it as enrolment.issue by installation,
// and tells every administrator, saying so; the code is kept as its SHA-256, of the kind recovery,
// good for an hour. Run again, it prints another, and the one before opens nothing. A user who is
// not an administrator, and a login no user has, are refused, and nothing is issued, recorded or
// told for either.
func TestRecoverIssuesAnAdministratorARecoveryCodeWithNoCredential(t *testing.T) {
	database, admin := namespaced(t)
	for _, stmt := range []string{
		`insert into principals (id, kind) values ('carol', 'user'), ('alice', 'user')`,
		`insert into users (login, display_name, admin, suspended) values ('carol', 'Carol', true, true), ('alice', 'Alice', false, false)`,
	} {
		if _, err := admin.Exec(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
	c := config.Recovery{Database: database.Application, PublicURL: "https://agentiik.example.com"}
	now := time.Now().UTC().Truncate(time.Second)

	var out bytes.Buffer
	if err := recoverAdministrator(t.Context(), c, "carol", now, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	m := recoveryLink.FindStringSubmatch(out.String())
	if len(lines) != 2 || m == nil || lines[1] != m[0] ||
		!strings.HasPrefix(lines[0], "carol, an administrator, may open this link once, before "+now.Add(time.Hour).Format("15:04 UTC")+", ") ||
		!strings.Contains(lines[0], "every administrator is told of this one in agk whoami") {
		t.Fatalf("recover printed:\n%s", out.String())
	}
	sum := sha256.Sum256([]byte(m[1]))
	var kind, by string
	var expires time.Time
	if err := admin.QueryRow(t.Context(), `select kind, issued_by, expires_at from enrolment_codes where hash = $1 and login = 'carol' and revoked_at is null`, sum[:]).Scan(&kind, &by, &expires); err != nil {
		t.Fatalf("the code printed is not kept as open: %s", err)
	}
	if kind != "recovery" || by != "installation" || !expires.Equal(now.Add(time.Hour)) {
		t.Errorf("the code is kept as a %s code issued by %s, lapsing at %s", kind, by, expires)
	}

	out.Reset()
	if err := recoverAdministrator(t.Context(), c, "carol", now, &out); err != nil {
		t.Fatal(err)
	}
	if again := recoveryLink.FindStringSubmatch(out.String()); again == nil || again[1] == m[1] {
		t.Errorf("recover run again printed:\n%s", out.String())
	}
	var open int
	if err := admin.QueryRow(t.Context(), `select count(*) from enrolment_codes where login = 'carol' and revoked_at is null`).Scan(&open); err != nil || open != 1 {
		t.Errorf("%d of carol's codes are open, %v", open, err)
	}

	for login, says := range map[string]string{"alice": "alice is not an administrator", "nobody": "there is no user nobody"} {
		out.Reset()
		if err := recoverAdministrator(t.Context(), c, login, now, &out); err == nil || !strings.Contains(err.Error(), says) || out.Len() > 0 {
			t.Errorf("recovering %s answered %v, printed %q", login, err, out.String())
		}
	}
	var told, untold int
	if err := admin.QueryRow(t.Context(), `select count(*) filter (where recipient = 'carol' and kind = 'break_glass_recovery' and login = 'carol'),
		count(*) filter (where recipient <> 'carol') from notifications`).Scan(&told, &untold); err != nil || told != 2 || untold != 0 {
		t.Errorf("carol is told of her recovery %d times and others %d, %v", told, untold, err)
	}
	want := []string{"installation enrolment.issue carol done", "installation enrolment.issue carol done"}
	if got := audited(t, admin); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the audit log holds %q, want %q", got, want)
	}
}

// recover reads the API's database setting and its public URL, AGK_PROXY_URL where one is named
// and AGK_PUBLIC_URL otherwise, and never the privileged role migrate connects as; and it refuses a
// login no user can have before it reads any setting, printing nothing.
func TestRecoverReadsTheAPIsSettingsAlone(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"recover", "carol"}, empty, &stdout, &stderr); code != exitFailed || stdout.Len() > 0 {
		t.Fatalf("recover with nothing configured exited %d and printed %q", code, stdout.String())
	}
	said := stderr.String()
	for _, variable := range []string{config.DatabaseURL, config.PublicURL} {
		if !strings.Contains(said, variable) {
			t.Errorf("recover's refusal does not name %s:\n%s", variable, said)
		}
	}
	if strings.Contains(said, config.MigrateDatabaseURL) {
		t.Errorf("recover's refusal names the role migrate connects as:\n%s", said)
	}

	behind := func(name string) (string, bool) {
		if name == config.ProxyURL {
			return "https://agentiik.example.com", true
		}
		return "", false
	}
	stderr.Reset()
	run(t.Context(), []string{"recover", "carol"}, behind, &stdout, &stderr)
	if strings.Contains(stderr.String(), config.PublicURL) || !strings.Contains(stderr.String(), config.DatabaseURL) {
		t.Errorf("behind a proxy, recover's refusal reads:\n%s", stderr.String())
	}

	stderr.Reset()
	if code := run(t.Context(), []string{"recover", "Carol"}, empty, &stdout, &stderr); code != exitFailed ||
		!strings.Contains(stderr.String(), "is not a login") || strings.Contains(stderr.String(), config.DatabaseURL) || stdout.Len() > 0 {
		t.Errorf("recovering a login no user can have exited %d and said:\n%s", code, stderr.String())
	}
}
