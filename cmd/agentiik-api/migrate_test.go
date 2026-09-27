package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/config"
	"github.com/jackc/pgx/v5"
)

// migrate keeps the bootstrap token's hash for an installation that runs no init, Homebrew's or one
// put together by hand: the token its settings set, as init does, or else, once, the hash a v0.2
// installation kept in the file AGK_OPERATOR_TOKEN_FILE names.

// bootstrapState is a fresh database and what reads its bootstrap state and ends it, as the
// first administrator's enrolment does.
type bootstrapState struct {
	t        *testing.T
	database config.Migration
	admin    *pgx.Conn
}

func aBootstrapState(t *testing.T) *bootstrapState {
	t.Helper()
	database := freshDatabase(t)
	admin, err := pgx.Connect(t.Context(), database.Admin.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.WithoutCancel(t.Context())) })
	return &bootstrapState{t: t, database: database, admin: admin}
}

// kept is the hash the database keeps, nil where it keeps none.
func (b *bootstrapState) kept() []byte {
	b.t.Helper()
	var hash []byte
	if err := b.admin.QueryRow(b.t.Context(), `select token_hash from bootstrap`).Scan(&hash); err != nil {
		b.t.Fatal(err)
	}
	return hash
}

// end ends the bootstrap, as the first administrator's enrolment does.
func (b *bootstrapState) end() {
	b.t.Helper()
	pool, err := db.Open(b.t.Context(), b.database.Application.ConnString())
	if err != nil {
		b.t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Installation(b.t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		_, err := w.EndBootstrap(ctx, time.Now())
		return err
	}); err != nil {
		b.t.Fatal(err)
	}
}

// migrated runs migrate with token set and the v0.2 file at file, failing the test where it fails,
// and answers what it said.
func (b *bootstrapState) migrated(token config.Secret, file string) string {
	b.t.Helper()
	out, err := b.migrate(token, file)
	if err != nil {
		b.t.Fatalf("migrate failed: %s\n%s", err, out)
	}
	return out
}

func (b *bootstrapState) migrate(token config.Secret, file string) (string, error) {
	b.t.Helper()
	c := b.database
	c.OperatorToken = token
	c.OperatorTokenFile = file
	var out bytes.Buffer
	err := migrateAndBootstrap(b.t.Context(), c, &out)
	return out.String(), err
}

// aV02HashFile is the file a v0.2 installation kept the hash of token in, as its init and
// agentiik-setup wrote it: the hexadecimal and a newline, readable by its owner alone.
func aV02HashFile(t *testing.T, token string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(token))
	path := filepath.Join(t.TempDir(), "operator-token.sha256")
	if err := os.WriteFile(path, []byte(hex.EncodeToString(sum[:])+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// rewrite puts content in the file at path, with mode.
func rewrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func hashOf(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// A v0.2.5 installation that never ran init, upgraded by running this release's migrate with the
// settings it had: the migration adds the bootstrap state, migrate imports the hash from the file
// the settings name, and the operator token authenticates through the API's Principals as the
// bootstrap operator, an administrator. The file is read that once: a second run keeps the hash
// whatever the file holds since.
func TestMigrateImportsTheV02OperatorTokensHashOnAnUpgradedDatabase(t *testing.T) {
	b := aBootstrapState(t)
	if _, err := db.MigrateThrough(t.Context(), b.admin, v025); err != nil {
		t.Fatalf("the database could not be migrated as v0.2.5 migrated it: %s", err)
	}
	file := aV02HashFile(t, theToken)

	out := b.migrated("", file)
	if !strings.Contains(out, "applied 0032_") || !strings.Contains(out, "imported the hash of the v0.2 operator token") {
		t.Errorf("migrate said:\n%s", out)
	}
	if !bytes.Equal(b.kept(), hashOf(theToken)) {
		t.Fatalf("migrate kept %x", b.kept())
	}
	if strings.Contains(out, file) || strings.Contains(out, theHash) {
		t.Errorf("migrate printed the file's path or the hash:\n%s", out)
	}

	pool, err := db.Open(t.Context(), b.database.Application.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	principals, err := api.NewPrincipals(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(t.Context(), "GET", "/api/v1/runner-pools", nil)
	request.Header.Set("Authorization", "Bearer "+theToken)
	who, err := principals.Identify(request)
	if err != nil || who.Principal != api.BootstrapOperator {
		t.Fatalf("the operator token is identified as %+v: %v", who, err)
	}
	if allowed, err := principals.Allow(t.Context(), who.Principal, api.GrantManage, api.Target{}); err != nil || !allowed {
		t.Errorf("the operator token administers the installation %v: %v", allowed, err)
	}

	// The file is never read again: one that would be refused now is passed over.
	rewrite(t, file, "not a hash any more", 0o644)
	if out := b.migrated("", file); !strings.Contains(out, "kept the hash of the bootstrap token stored") || strings.Contains(out, "v0.2 operator token") {
		t.Errorf("a second run said:\n%s", out)
	}
	if !bytes.Equal(b.kept(), hashOf(theToken)) {
		t.Errorf("a second run kept %x", b.kept())
	}
}

// A v0.2.5 installation that never ran init has namespaces with no built-in identity, and migrate
// gives each its own, once, in the run that upgrades it, recorded in the namespace.
func TestMigrateGivesTheNamespacesOfV02TheirBuiltInIdentityOnce(t *testing.T) {
	b := aBootstrapState(t)
	if _, err := db.MigrateThrough(t.Context(), b.admin, v025); err != nil {
		t.Fatalf("the database could not be migrated as v0.2.5 migrated it: %s", err)
	}
	if _, err := b.admin.Exec(t.Context(), `insert into namespaces (name) values ('finance'), ('team-ops')`); err != nil {
		t.Fatal(err)
	}
	out := b.migrated(theToken, "")
	for _, ns := range []string{"finance", "team-ops"} {
		if !strings.Contains(out, "gave namespace "+ns+" its built-in identity, "+ns+"/agentiik") {
			t.Errorf("migrate said:\n%s", out)
		}
	}
	var entries int
	if err := b.admin.QueryRow(t.Context(),
		`select count(*) from audit_log where action = 'service_account.create' and actor = 'installation' and target = namespace || '/agentiik'`).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if entries != 2 {
		t.Errorf("%d built-in identities given are recorded in their namespace", entries)
	}
	if out := b.migrated(theToken, ""); strings.Contains(out, "built-in identity") {
		t.Errorf("a second run said:\n%s", out)
	}
}

// An installation of v0.2.5 may hold a namespace named stats, which v0.3.0 reserved for GET
// /api/v1/stats/pools. migrate keeps it, gives it its built-in identity as any other, and says at
// every run that the route will take its paths from v0.6.0 and what to do; it says nothing where no
// namespace carries the word.
func TestMigrateSaysWhereANamespaceIsNamedAfterAWordReservedSince(t *testing.T) {
	b := aBootstrapState(t)
	if _, err := db.MigrateThrough(t.Context(), b.admin, v025); err != nil {
		t.Fatalf("the database could not be migrated as v0.2.5 migrated it: %s", err)
	}
	if out := b.migrated(theToken, ""); strings.Contains(out, "named after a word") {
		t.Errorf("migrate, with no namespace named stats, said:\n%s", out)
	}
	if _, err := b.admin.Exec(t.Context(), `insert into namespaces (name) values ('stats')`); err != nil {
		t.Fatal(err)
	}
	for run := range 2 {
		out := b.migrated(theToken, "")
		if !strings.Contains(out, "namespace stats is named after a word the API routes on from v0.6.0, for GET /api/v1/stats/pools, and from then its own routes under /api/v1/stats/ will not reach it: create another namespace and move its workflows there. It is served as before until then, and migrate says so at every run while it exists\n") {
			t.Errorf("migrate's run %d said:\n%s", run+1, out)
		}
		if run == 0 && !strings.Contains(out, "gave namespace stats its built-in identity, stats/agentiik") {
			t.Errorf("migrate gave stats no built-in identity:\n%s", out)
		}
	}
}

// A hash kept is the installation's, from a token set since or from init, and the v0.2 file never
// replaces it. Once the bootstrap has ended, nothing is imported, nothing is said, and the file is
// not read at all.
func TestMigrateImportsNothingWhereAHashIsKeptOrTheBootstrapEnded(t *testing.T) {
	b := aBootstrapState(t)
	if err := migrate(t.Context(), b.database, io.Discard); err != nil {
		t.Fatal(err)
	}
	bootstrapped(t, b.database.Application)
	file := aV02HashFile(t, "agk_op_the_v02_token_of_this_installation")

	if out := b.migrated("", file); !strings.Contains(out, "kept the hash of the bootstrap token stored") || strings.Contains(out, "v0.2 operator token") {
		t.Errorf("with a hash kept, migrate said:\n%s", out)
	}
	if !bytes.Equal(b.kept(), hashOf(theToken)) {
		t.Errorf("with a hash kept, migrate kept %x", b.kept())
	}

	b.end()
	for what, content := range map[string]string{"a v0.2 hash": theHash + "\n", "anything": "not a hash"} {
		rewrite(t, file, content, 0o644)
		out := b.migrated("", file)
		if b.kept() != nil || strings.Contains(out, "bootstrap token") || strings.Contains(out, "v0.2 operator token") || strings.Contains(out, "administrator") {
			t.Errorf("once the bootstrap ended, with a file holding %s, migrate kept %x and said:\n%s", what, b.kept(), out)
		}
	}
}

// migrate takes AGK_OPERATOR_TOKEN as init does: a token set replaces the hash at every run, the
// v0.2 file unread, until the bootstrap ends, and from then on it is ignored, saying so, which is
// no error.
func TestMigrateKeepsTheBootstrapTokenSetAsInitDoes(t *testing.T) {
	b := aBootstrapState(t)
	file := aV02HashFile(t, theToken)
	chosen := config.Secret("a-token-of-my-own-at-least-32-characters")
	changed := config.Secret("another-token-of-my-own-32-characters")

	if out := b.migrated(chosen, file); !bytes.Equal(b.kept(), hashOf(string(chosen))) || !strings.Contains(out, "wrote the hash of the bootstrap token set") || strings.Contains(out, "v0.2 operator token") {
		t.Errorf("a token set was not kept, or the v0.2 file was read, keeping %x:\n%s", b.kept(), out)
	}
	if out := b.migrated(chosen, file); !bytes.Equal(b.kept(), hashOf(string(chosen))) || !strings.Contains(out, "kept the hash of the bootstrap token set") {
		t.Errorf("the same token set again was not kept:\n%s", out)
	}
	if out := b.migrated(changed, file); !bytes.Equal(b.kept(), hashOf(string(changed))) || !strings.Contains(out, "wrote the hash") {
		t.Errorf("a token changed did not replace the hash:\n%s", out)
	}
	if out := b.migrated("", file); !bytes.Equal(b.kept(), hashOf(string(changed))) || strings.Contains(out, "v0.2 operator token") {
		t.Errorf("a run with no token set did not keep the hash of the last one set:\n%s", out)
	}

	b.end()
	out := b.migrated(chosen, file)
	if b.kept() != nil || !strings.Contains(out, "ignored the bootstrap token set") || !strings.Contains(out, "no error") {
		t.Errorf("once the bootstrap ended, a token set was kept as %x, or not said to be ignored:\n%s", b.kept(), out)
	}
	if strings.Contains(out, string(chosen)) {
		t.Errorf("migrate printed the token:\n%s", out)
	}
}

// A v0.2 file in any shape but the one v0.2 wrote, or that anybody but its owner may read, fails
// the run naming the variable, never the path, and imports nothing. One that is not there is no
// error: nothing is imported, and migrate says so, and that nobody can create the first
// administrator.
func TestMigrateRefusesAV02FileItCannotTrustAndPassesOverOneThatIsNotThere(t *testing.T) {
	b := aBootstrapState(t)
	file := aV02HashFile(t, theToken)
	for what, c := range map[string]struct {
		content string
		mode    os.FileMode
	}{
		"readable by anybody":   {theHash + "\n", 0o644},
		"readable by its group": {theHash + "\n", 0o640},
		"holding the token":     {theToken + "\n", 0o600},
		"in uppercase":          {strings.ToUpper(theHash) + "\n", 0o600},
		"one character short":   {theHash[:63] + "\n", 0o600},
	} {
		rewrite(t, file, c.content, c.mode)
		out, err := b.migrate("", file)
		if err == nil || !strings.Contains(err.Error(), config.OperatorTokenFile) || !strings.Contains(err.Error(), "not imported") {
			t.Errorf("a file %s was not refused naming %s: %v\n%s", what, config.OperatorTokenFile, err, out)
			continue
		}
		if strings.Contains(err.Error(), file) || strings.Contains(err.Error(), theToken) || strings.Contains(err.Error(), theHash[:63]) {
			t.Errorf("the refusal of a file %s repeats what it should not: %s", what, err)
		}
		if b.kept() != nil {
			t.Errorf("a file %s was imported as %x", what, b.kept())
		}
	}

	out := b.migrated("", filepath.Join(t.TempDir(), "operator-token.sha256"))
	if b.kept() != nil || !strings.Contains(out, "imported no hash of the v0.2 operator token") || !strings.Contains(out, "nobody can create the first administrator") || !strings.Contains(out, "run migrate again") {
		t.Errorf("with no file there, migrate kept %x and said:\n%s", b.kept(), out)
	}
	if out := b.migrated("", ""); b.kept() != nil || strings.Contains(out, "v0.2 operator token") || !strings.Contains(out, "nobody can create the first administrator") {
		t.Errorf("with no file named, migrate kept %x and said:\n%s", b.kept(), out)
	}
}

// A hash another run keeps between migrate's read and its import stands, rather than being
// overwritten by the v0.2 file's.
func TestAHashKeptWhileMigrateImportsStands(t *testing.T) {
	b := aBootstrapState(t)
	if err := migrate(t.Context(), b.database, io.Discard); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(t.Context(), b.database.Application.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	meanwhile := func() ([]byte, error) {
		// Another run keeps its token's hash, and commits, while this one reads the file.
		if _, err := b.admin.Exec(t.Context(), `update bootstrap set token_hash = $1`, hashOf(theToken)); err != nil {
			t.Fatal(err)
		}
		return hashOf("agk_op_the_v02_token_of_this_installation"), nil
	}
	var out bytes.Buffer
	if err := bootstrapToken(t.Context(), pool, "migrate", "", meanwhile, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b.kept(), hashOf(theToken)) || !strings.Contains(out.String(), "another run") {
		t.Errorf("the hash kept meanwhile reads as %x, and migrate said:\n%s", b.kept(), out.String())
	}
}
