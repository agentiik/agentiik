package config_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/config"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
)

// An installation's configuration, held to the answer that settled it: AGK_* environment
// variables, every secret in a file an AGK_*_FILE variable names and never a value, and a setting
// that is missing, unreadable or malformed refusing the start, naming the variable.

// installation is one whole configuration, every setting written, and what each file holds.
type installation struct {
	env map[string]string
	dir string

	databasePassword, adminPassword string
	busJWT, busSeed                 string
	busExpires                      time.Time
	accountSeed                     string
	presignKey                      []byte
	masterKey                       []byte
	operatorToken                   string
	metricsToken                    string

	// secrets is every secret this installation holds and every path of a file holding one,
	// none of which a refusal may repeat.
	secrets []string
}

func anInstallation(t *testing.T) *installation {
	t.Helper()
	i := &installation{dir: t.TempDir()}
	objects := filepath.Join(i.dir, "objects")
	if err := os.Mkdir(objects, 0o700); err != nil {
		t.Fatal(err)
	}

	account, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatal(err)
	}
	seed, err := account.Seed()
	if err != nil {
		t.Fatal(err)
	}
	i.accountSeed = string(seed)
	i.busExpires = time.Now().Add(365 * 24 * time.Hour).Truncate(time.Second).UTC()
	i.busJWT, i.busSeed = aUserCredential(t, account, i.busExpires)
	creds, err := jwt.FormatUserConfig(i.busJWT, []byte(i.busSeed))
	if err != nil {
		t.Fatal(err)
	}

	i.presignKey = randomBytes(t, 48)
	i.masterKey = []byte("id: 2026-09\nkey: " + base64.StdEncoding.EncodeToString(randomBytes(t, 32)) + "\n")
	sum := sha256.Sum256([]byte("agkoperator_" + base64.RawURLEncoding.EncodeToString(randomBytes(t, 32))))
	i.operatorToken = hex.EncodeToString(sum[:])
	sum = sha256.Sum256(randomBytes(t, 32))
	i.metricsToken = hex.EncodeToString(sum[:])
	i.databasePassword = "p@ss:w/rd %20 and more"
	i.adminPassword = "the superuser's own"

	i.env = map[string]string{
		config.DatabaseURL:                 "postgres://agentiik@db:5432/agentiik?sslmode=verify-full",
		config.DatabasePasswordFile:        i.write(t, "database.password", []byte(i.databasePassword+"\n")),
		config.MigrateDatabaseURL:          "postgres://postgres@db:5432/agentiik?sslmode=verify-full",
		config.MigrateDatabasePasswordFile: i.write(t, "admin.password", []byte(i.adminPassword)),
		config.BusURL:                      "tls://nats-1:4222,tls://nats-2:4222",
		config.BusCredentialsFile:          i.write(t, "control-plane.creds", creds),
		config.BusAccountSeedFile:          i.write(t, "account.seed", []byte(i.accountSeed+"\n")),
		config.ObjectsDir:                  objects,
		config.PublicURL:                   "https://agentiik.example.com/",
		config.PresignKeyFile:              i.write(t, "presign.key", []byte(base64.StdEncoding.EncodeToString(i.presignKey)+"\n")),
		config.MasterKeyFile:               i.write(t, "master.key", i.masterKey),
		config.EnvPrefixes:                 "finance=AGK_DEV_FINANCE_,team-ops=AGK_DEV_TEAM_OPS_",
		config.Listen:                      "127.0.0.1:9090",
		config.MaxRequeues:                 "1",
		config.TaskCeiling:                 "2h",
		config.JoinRotation:                "240h",
		config.RevocationGrace:             "90m",
		config.OperatorTokenFile:           i.write(t, "operator.token", []byte(i.operatorToken+"\n")),
		config.MetricsListen:               "10.0.0.5:9464",
		config.MetricsTokenFile:            i.write(t, "metrics.token", []byte(i.metricsToken+"\n")),
		config.OTLPEndpoint:                "http://127.0.0.1:4318/",
	}
	i.secrets = append(i.secrets,
		i.databasePassword, i.adminPassword, i.busJWT, i.busSeed, i.accountSeed,
		base64.StdEncoding.EncodeToString(i.presignKey), string(i.masterKey), i.operatorToken, i.metricsToken)
	return i
}

// write puts a secret's file in the installation's directory, readable by its owner alone.
func (i *installation) write(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(i.dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	i.secrets = append(i.secrets, path)
	return path
}

// onlyWhatIsRequired leaves out every setting that has a default or can be left out.
func (i *installation) onlyWhatIsRequired() {
	for _, name := range []string{
		config.DatabasePasswordFile, config.MigrateDatabasePasswordFile, config.EnvPrefixes,
		config.Listen, config.MaxRequeues, config.TaskCeiling, config.JoinRotation,
		config.RevocationGrace, config.MetricsListen, config.MetricsTokenFile, config.OTLPEndpoint,
	} {
		delete(i.env, name)
	}
}

// aUserCredential is a NATS user credential signed by account, as bus.Issuer mints one.
func aUserCredential(t *testing.T, account nkeys.KeyPair, expires time.Time) (string, string) {
	t.Helper()
	user, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	public, err := user.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	seed, err := user.Seed()
	if err != nil {
		t.Fatal(err)
	}
	claims := jwt.NewUserClaims(public)
	claims.Name = "controller"
	if !expires.IsZero() {
		claims.Expires = expires.Unix()
	}
	token, err := claims.Encode(account)
	if err != nil {
		t.Fatal(err)
	}
	return token, string(seed)
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// program is one reading, and what its environment is given out of an installation's.
type program struct {
	name string
	read func(config.Lookup) error

	// leaves out what the installation gives this program no more than it would give it: the
	// controller is never given the master key, which it refuses.
	leaves []string
}

var (
	theAPI = program{name: "the API", read: func(l config.Lookup) error {
		_, err := config.ReadAPI(l)
		return err
	}}
	theController = program{name: "the controller", read: func(l config.Lookup) error {
		_, err := config.ReadController(l)
		return err
	}, leaves: []string{config.MasterKeyFile}}
	migrating = program{name: "migrating", read: func(l config.Lookup) error {
		_, err := config.ReadMigration(l)
		return err
	}}
	everyProgram = []program{theAPI, theController, migrating}
)

// environment is what p is given of the installation, and the lookup it is read through.
func (p program) environment(i *installation) config.Lookup {
	env := maps.Clone(i.env)
	for _, name := range p.leaves {
		delete(env, name)
	}
	return lookupIn(env, nil)
}

// lookupIn reads env, and records every name it is asked for in asked where that is not nil.
func lookupIn(env map[string]string, asked *[]string) config.Lookup {
	return func(name string) (string, bool) {
		if asked != nil {
			*asked = append(*asked, name)
		}
		v, ok := env[name]
		return v, ok
	}
}

// refused is every variable a refusal names, in the order it names them.
func refused(err error) []string {
	var names []string
	var walk func(error)
	walk = func(err error) {
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			for _, e := range joined.Unwrap() {
				walk(e)
			}
			return
		}
		var e *config.Error
		if errors.As(err, &e) {
			names = append(names, e.Variable)
		}
	}
	walk(err)
	return names
}

// Every setting written is read, each file an _FILE variable names as what it holds.
func TestAWholeInstallationIsRead(t *testing.T) {
	i := anInstallation(t)

	api, err := config.ReadAPI(theAPI.environment(i))
	if err != nil {
		t.Fatal(err)
	}
	wantDatabase := config.Database{URL: i.env[config.DatabaseURL], Role: "agentiik", Password: config.Secret(i.databasePassword)}
	if api.Database != wantDatabase {
		t.Errorf("the API's database reads %#v", api.Database)
	}
	wantBus := config.Bus{URL: i.env[config.BusURL], JWT: i.busJWT, Seed: config.Secret(i.busSeed), Expires: i.busExpires}
	if api.Bus != wantBus {
		t.Errorf("the API's bus reads %+v, and was written %+v", api.Bus, wantBus)
	}
	for what, c := range map[string]struct{ got, want any }{
		"the account seed":      {string(api.AccountSeed), i.accountSeed},
		"the objects":           {api.Objects, i.env[config.ObjectsDir]},
		"the public URL":        {api.PublicURL, "https://agentiik.example.com"},
		"the listen address":    {api.Listen, "127.0.0.1:9090"},
		"the join rotation":     {api.JoinRotation, 240 * time.Hour},
		"the revocation grace":  {api.RevocationGrace, 90 * time.Minute},
		"the operator token":    {api.OperatorToken, i.operatorToken},
		"the presign key":       {string(api.PresignKey), string(i.presignKey)},
		"the master key's file": {string(api.MasterKey), string(i.masterKey)},
	} {
		if c.got != c.want {
			t.Errorf("%s reads %v, and was written %v", what, c.got, c.want)
		}
	}
	if !maps.Equal(api.EnvPrefixes, map[string]string{"finance": "AGK_DEV_FINANCE_", "team-ops": "AGK_DEV_TEAM_OPS_"}) {
		t.Errorf("the env prefixes read %v", api.EnvPrefixes)
	}

	controller, err := config.ReadController(theController.environment(i))
	if err != nil {
		t.Fatal(err)
	}
	want := config.Controller{
		Database: wantDatabase, Bus: wantBus, Objects: i.env[config.ObjectsDir], MaxRequeues: 1, TaskCeiling: 2 * time.Hour,
		Metrics:      config.Metrics{Listen: "10.0.0.5:9464", TokenHash: i.metricsToken},
		OTLPEndpoint: "http://127.0.0.1:4318",
	}
	if controller != want {
		t.Errorf("the controller reads %+v", controller)
	}

	migration, err := config.ReadMigration(migrating.environment(i))
	if err != nil {
		t.Fatal(err)
	}
	wantMigration := config.Migration{
		Admin:       config.Database{URL: i.env[config.MigrateDatabaseURL], Role: "postgres", Password: config.Secret(i.adminPassword)},
		Application: wantDatabase,
	}
	if migration != wantMigration {
		t.Errorf("migrating reads %#v", migration)
	}
}

// A setting left out takes its default, and each default is the figure the documentation gives.
func TestEverySettingLeftOutTakesItsDefault(t *testing.T) {
	i := anInstallation(t)
	i.onlyWhatIsRequired()

	api, err := config.ReadAPI(theAPI.environment(i))
	if err != nil {
		t.Fatal(err)
	}
	controller, err := config.ReadController(theController.environment(i))
	if err != nil {
		t.Fatal(err)
	}
	migration, err := config.ReadMigration(migrating.environment(i))
	if err != nil {
		t.Fatal(err)
	}
	for what, c := range map[string]struct{ got, want any }{
		"the listen address, every interface on 8080": {api.Listen, ":8080"},
		"the join rotation, thirty days":              {api.JoinRotation, 30 * 24 * time.Hour},
		"the revocation grace, the task ceiling":      {api.RevocationGrace, time.Hour},
		"the task ceiling, an hour":                   {controller.TaskCeiling, time.Hour},
		"max_requeues, three":                         {controller.MaxRequeues, 3},
		"max_requeues, the evaluator's own default":   {controller.MaxRequeues, graph.DefaultMaxRequeues},
		"the collector, none, so nothing is traced":   {controller.OTLPEndpoint, ""},
		"the database password, none":                 {string(api.Database.Password), ""},
		"the admin's password, none":                  {string(migration.Admin.Password), ""},
		"the application's password, none":            {string(migration.Application.Password), ""},
		"the metrics, answered nowhere":               {controller.Metrics, config.Metrics{}},
	} {
		if c.got != c.want {
			t.Errorf("%s reads %v", what, c.got)
		}
	}
	if api.EnvPrefixes != nil {
		t.Errorf("an installation that opted no namespace in to env reads %v", api.EnvPrefixes)
	}

	// Set to nothing is unset, which is what an empty Environment= line in a unit writes.
	i.env[config.MaxRequeues] = ""
	if controller, err := config.ReadController(theController.environment(i)); err != nil || controller.MaxRequeues != 3 {
		t.Errorf("max_requeues set to nothing read %d, %v", controller.MaxRequeues, err)
	}

	// Zero is a number of requeues, and the one that requeues nothing.
	i.env[config.MaxRequeues] = "0"
	if controller, err := config.ReadController(theController.environment(i)); err != nil || controller.MaxRequeues != 0 {
		t.Errorf("max_requeues of zero read %d, %v", controller.MaxRequeues, err)
	}

	// The grace follows the ceiling the API is given, when the grace is not given itself.
	i.env[config.TaskCeiling] = "3h"
	if api, err := config.ReadAPI(theAPI.environment(i)); err != nil || api.RevocationGrace != 3*time.Hour {
		t.Errorf("with a ceiling of three hours, the grace read %v, %v", api.RevocationGrace, err)
	}
}

// Each fault of the file an _FILE variable names refuses the start of every program that reads
// it, naming the variable, and never repeating the path or what the file holds.
func TestAFileThatCannotBeReadRefusesTheStart(t *testing.T) {
	readBy := map[string][]program{
		config.DatabasePasswordFile:        everyProgram,
		config.MigrateDatabasePasswordFile: {migrating},
		config.BusCredentialsFile:          {theAPI, theController},
		config.BusAccountSeedFile:          {theAPI},
		config.PresignKeyFile:              {theAPI},
		config.MasterKeyFile:               {theAPI},
		config.OperatorTokenFile:           {theAPI},
	}
	faults := map[string]func(t *testing.T, i *installation, path string) string{
		// Relative to the directory the program starts in, where the file is, so that only the
		// path being relative can refuse it.
		"a relative path": func(t *testing.T, i *installation, path string) string {
			t.Chdir(i.dir)
			return filepath.Base(path)
		},
		"a file that is not there": func(_ *testing.T, _ *installation, path string) string {
			return path + ".gone"
		},
		"a directory": func(_ *testing.T, i *installation, _ string) string {
			return i.dir
		},
		"a named pipe, which would never finish opening": func(t *testing.T, _ *installation, path string) string {
			fifo := path + ".fifo"
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Fatal(err)
			}
			return fifo
		},
		"a file its group can read": func(t *testing.T, _ *installation, path string) string {
			return chmod(t, path, 0o640)
		},
		"a file anybody can read": func(t *testing.T, _ *installation, path string) string {
			return chmod(t, path, 0o644)
		},
		"a file nobody can read": func(t *testing.T, _ *installation, path string) string {
			if os.Geteuid() == 0 {
				t.Skip("root reads a file of mode 000")
			}
			return chmod(t, path, 0o000)
		},
		"an empty file": func(t *testing.T, i *installation, path string) string {
			return i.write(t, filepath.Base(path)+".empty", []byte(" \n"))
		},
		"a file larger than any secret": func(t *testing.T, i *installation, path string) string {
			return i.write(t, filepath.Base(path)+".large", bytes.Repeat([]byte("a"), 64<<10+1))
		},
	}
	for variable, programs := range readBy {
		for fault, apply := range faults {
			for _, p := range programs {
				t.Run(fmt.Sprintf("%s naming %s, for %s", variable, fault, p.name), func(t *testing.T) {
					i := anInstallation(t)
					i.env[variable] = apply(t, i, i.env[variable])
					err := p.read(p.environment(i))
					if names := refused(err); !slices.Equal(names, []string{variable}) {
						t.Fatalf("the start was refused naming %v: %v", names, err)
					}
					if fault == "a relative path" && !strings.Contains(err.Error(), "is not an absolute path") {
						t.Errorf("a relative path was refused for another reason: %v", err)
					}
					saysNothingOf(t, err, append(i.secrets, i.env[variable])...)
				})
			}
		}
	}
}

func chmod(t *testing.T, path string, mode os.FileMode) string {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// saysNothingOf fails if a refusal repeats any of values.
func saysNothingOf(t *testing.T, err error, values ...string) {
	t.Helper()
	said := err.Error()
	for _, v := range values {
		if v != "" && strings.Contains(said, strings.TrimSpace(v)) {
			t.Errorf("the refusal repeats %q: %s", v, said)
		}
	}
}

// Every setting refuses the start when it is missing, where it has no default, or written as
// nothing it could mean, and the refusal names the variable.
func TestASettingMissingOrMalformedRefusesTheStart(t *testing.T) {
	type fault struct {
		variable string
		value    func(t *testing.T, i *installation) string
		programs []program
	}
	unset := func(*testing.T, *installation) string { return "" }
	is := func(v string) func(*testing.T, *installation) string {
		return func(*testing.T, *installation) string { return v }
	}
	holding := func(content string) func(*testing.T, *installation) string {
		return func(t *testing.T, i *installation) string {
			return i.write(t, "holding", []byte(content))
		}
	}
	credentialExpired := func(t *testing.T, i *installation) string {
		account, _ := nkeys.CreateAccount()
		token, seed := aUserCredential(t, account, time.Now().Add(-time.Minute))
		creds, err := jwt.FormatUserConfig(token, []byte(seed))
		if err != nil {
			t.Fatal(err)
		}
		return i.write(t, "expired.creds", creds)
	}
	credentialOfTwoUsers := func(t *testing.T, i *installation) string {
		account, _ := nkeys.CreateAccount()
		token, _ := aUserCredential(t, account, time.Now().Add(time.Hour))
		_, seed := aUserCredential(t, account, time.Now().Add(time.Hour))
		// Written by hand, since FormatUserConfig refuses to write this for the same reason.
		decoratedJWT, err := jwt.DecorateJWT(token)
		if err != nil {
			t.Fatal(err)
		}
		decoratedSeed, err := jwt.DecorateSeed([]byte(seed))
		if err != nil {
			t.Fatal(err)
		}
		return i.write(t, "two.creds", append(decoratedJWT, decoratedSeed...))
	}
	aUsersSeed := func(t *testing.T, i *installation) string {
		return i.write(t, "user.seed", []byte(i.busSeed))
	}
	anAccountSeedAsACredential := func(t *testing.T, i *installation) string {
		return i.write(t, "account.creds", []byte(i.accountSeed))
	}
	objectsWhereTheProgramStarts := func(t *testing.T, i *installation) string {
		// The directory is there, so that only the path being relative can refuse it.
		t.Chdir(i.dir)
		return "objects"
	}
	aFile := func(t *testing.T, i *installation) string {
		// Not a secret's file, so not one of the paths a refusal may not repeat.
		path := filepath.Join(i.dir, "not-a-directory")
		if err := os.WriteFile(path, []byte("objects"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	aDirectoryNobodyWrites := func(t *testing.T, i *installation) string {
		if os.Geteuid() == 0 {
			t.Skip("root writes in a directory whatever its mode says")
		}
		path := filepath.Join(i.dir, "read-only")
		if err := os.Mkdir(path, 0o555); err != nil {
			t.Fatal(err)
		}
		return path
	}
	api, controller, both, all := []program{theAPI}, []program{theController}, []program{theAPI, theController}, everyProgram

	faults := map[string]fault{
		"no database":                           {config.DatabaseURL, unset, all},
		"a database that is not PostgreSQL":     {config.DatabaseURL, is("mysql://agentiik@db/agentiik"), all},
		"a database written as keywords":        {config.DatabaseURL, is("host=db user=agentiik dbname=agentiik"), all},
		"a database naming no role":             {config.DatabaseURL, is("postgres://db:5432/agentiik"), all},
		"a database at no port":                 {config.DatabaseURL, is("postgres://agentiik@db:postgres/agentiik"), all},
		"a database parameter with no value":    {config.DatabaseURL, is("postgres://agentiik@db/agentiik?sslmode"), all},
		"a database parameter with a space":     {config.DatabaseURL, is("postgres://agentiik@db/agentiik?application_name=a b"), all},
		"a database password of two lines":      {config.DatabasePasswordFile, holding("one\ntwo\n"), all},
		"no migration database":                 {config.MigrateDatabaseURL, unset, []program{migrating}},
		"a migration database that is not one":  {config.MigrateDatabaseURL, is("https://db/agentiik"), []program{migrating}},
		"a database with no sslmode":            {config.DatabaseURL, is("postgres://agentiik@db/agentiik"), all},
		"a database that prefers TLS":           {config.DatabaseURL, is("postgres://agentiik@db/agentiik?sslmode=prefer"), all},
		"a database that allows TLS":            {config.DatabaseURL, is("postgres://agentiik@db/agentiik?sslmode=allow"), all},
		"a database with TLS disabled":          {config.DatabaseURL, is("postgres://agentiik@db/agentiik?sslmode=disable"), all},
		"a database at a socket and a host":     {config.DatabaseURL, is("postgres://agentiik@/agentiik?host=/run/postgresql,db"), all},
		"a migration database in plaintext":     {config.MigrateDatabaseURL, is("postgres://postgres@db/agentiik?sslmode=disable"), []program{migrating}},
		"no bus":                                {config.BusURL, unset, both},
		"a bus that is not NATS":                {config.BusURL, is("http://nats:4222"), both},
		"a bus in plaintext":                    {config.BusURL, is("nats://nats:4222"), both},
		"a bus over a plaintext websocket":      {config.BusURL, is("ws://nats:8080"), both},
		"a bus one of whose servers is plain":   {config.BusURL, is("tls://nats-1:4222,nats://nats-2:4222"), both},
		"a bus with no host":                    {config.BusURL, is("nats://"), both},
		"a bus one of whose servers is not one": {config.BusURL, is("tls://nats-1:4222,amqp://rabbit:5672"), both},
		"no bus credential":                     {config.BusCredentialsFile, unset, both},
		"a bus credential that is not one":      {config.BusCredentialsFile, holding("-----BEGIN NATS USER JWT-----\nnot a jwt\n------END NATS USER JWT------\n"), both},
		"a bus credential that has expired":     {config.BusCredentialsFile, credentialExpired, both},
		"a bus credential of two users":         {config.BusCredentialsFile, credentialOfTwoUsers, both},
		"an account seed for a bus credential":  {config.BusCredentialsFile, anAccountSeedAsACredential, both},
		"no account seed":                       {config.BusAccountSeedFile, unset, api},
		"a user's seed for the account's":       {config.BusAccountSeedFile, aUsersSeed, api},
		"an account seed that is not a seed":    {config.BusAccountSeedFile, holding("SAnotaseed\n"), api},
		"no objects":                            {config.ObjectsDir, unset, both},
		"objects at a relative path":            {config.ObjectsDir, objectsWhereTheProgramStarts, both},
		"objects that are not there":            {config.ObjectsDir, is("/nonexistent/agentiik/objects"), both},
		"objects that are a file":               {config.ObjectsDir, aFile, both},
		"objects nobody may write":              {config.ObjectsDir, aDirectoryNobodyWrites, both},
		"no public URL":                         {config.PublicURL, unset, api},
		"a public URL with no scheme":           {config.PublicURL, is("agentiik.example.com"), api},
		"a public URL of another scheme":        {config.PublicURL, is("ftp://agentiik.example.com"), api},
		"a public URL in plaintext":             {config.PublicURL, is("http://agentiik.example.com"), api},
		"a public URL with a query":             {config.PublicURL, is("https://agentiik.example.com/?tenant=a"), api},
		"a public URL with a fragment":          {config.PublicURL, is("https://agentiik.example.com/#top"), api},
		"a public URL ending in a ?":            {config.PublicURL, is("https://agentiik.example.com?"), api},
		"a public URL ending in a #":            {config.PublicURL, is("https://agentiik.example.com/#"), api},
		"no presign key":                        {config.PresignKeyFile, unset, api},
		"a presign key that is not base64":      {config.PresignKeyFile, holding("not base64, clearly\n"), api},
		"a presign key of sixteen bytes":        {config.PresignKeyFile, holding(base64.StdEncoding.EncodeToString(make([]byte, 16))), api},
		"no master key":                         {config.MasterKeyFile, unset, api},
		"a master key given to the controller":  {config.MasterKeyFile, func(_ *testing.T, i *installation) string { return i.dir + "/master.key" }, controller},
		"no operator token":                     {config.OperatorTokenFile, unset, api},
		"the operator token itself":             {config.OperatorTokenFile, holding("agkoperator_" + strings.Repeat("A", 43) + "\n"), api},
		"an operator token hash in capitals":    {config.OperatorTokenFile, holding(strings.Repeat("AB", 32)), api},
		"an operator token hash cut short":      {config.OperatorTokenFile, holding(strings.Repeat("ab", 31) + "a"), api},
		"env prefixes with no prefix":           {config.EnvPrefixes, is("finance"), api},
		"env prefixes with an empty prefix":     {config.EnvPrefixes, is("finance="), api},
		"env prefixes with no namespace":        {config.EnvPrefixes, is("=AGK_DEV_FINANCE_"), api},
		"env prefixes giving one twice":         {config.EnvPrefixes, is("finance=AGK_DEV_A_,finance=AGK_DEV_B_"), api},
		"a listen address with no port":         {config.Listen, is("8080"), api},
		"a listen address past the last port":   {config.Listen, is(":65536"), api},
		"a listen address naming a service":     {config.Listen, is(":http"), api},
		"a negative max_requeues":               {config.MaxRequeues, is("-1"), controller},
		"a max_requeues that is not a number":   {config.MaxRequeues, is("three"), controller},
		"a max_requeues that is not whole":      {config.MaxRequeues, is("1.5"), controller},
		"a task ceiling of nothing":             {config.TaskCeiling, is("0s"), both},
		"a negative task ceiling":               {config.TaskCeiling, is("-1h"), both},
		"a task ceiling in days":                {config.TaskCeiling, is("1d"), both},
		"a task ceiling in words":               {config.TaskCeiling, is("an hour"), both},
		"a join rotation of nothing":            {config.JoinRotation, is("0"), api},
		"a join rotation in days":               {config.JoinRotation, is("30d"), api},
		"a revocation grace of nothing":         {config.RevocationGrace, is("0s"), api},
		"a revocation grace that is not a time": {config.RevocationGrace, is("soon"), api},
		"a metrics address with no port":        {config.MetricsListen, is("9464"), controller},
		"a metrics address naming a service":    {config.MetricsListen, is(":prometheus"), controller},
		"no metrics token":                      {config.MetricsTokenFile, unset, controller},
		"the metrics token itself":              {config.MetricsTokenFile, holding("s3cr3t-scrape-token\n"), controller},
		"a metrics token hash cut short":        {config.MetricsTokenFile, holding(strings.Repeat("ab", 31) + "a"), controller},
	}
	for what, f := range faults {
		for _, p := range f.programs {
			t.Run(fmt.Sprintf("%s, for %s", what, p.name), func(t *testing.T) {
				i := anInstallation(t)
				if v := f.value(t, i); v == "" {
					delete(i.env, f.variable)
				} else {
					i.env[f.variable] = v
				}
				env := maps.Clone(i.env)
				for _, name := range p.leaves {
					if name != f.variable {
						delete(env, name)
					}
				}
				err := p.read(lookupIn(env, nil))
				if names := refused(err); !slices.Equal(names, []string{f.variable}) {
					t.Fatalf("the start was refused naming %v: %v", names, err)
				}
				if !strings.HasPrefix(err.Error(), "config: "+f.variable+" ") {
					t.Errorf("the refusal does not begin by naming the variable: %s", err)
				}
				saysNothingOf(t, err, i.secrets...)
			})
		}
	}
}

// "No plaintext path anywhere, including between the control plane and the bus." Every way of
// reaching the database, the bus and the API over TLS is accepted, and a local socket, which
// crosses no network and which pgx speaks no TLS over, needs no sslmode.
func TestEveryPathOverTLSIsAccepted(t *testing.T) {
	for variable, values := range map[string][]string{
		config.DatabaseURL: {
			"postgres://agentiik@db/agentiik?sslmode=verify-full",
			"postgres://agentiik@db/agentiik?sslmode=verify-ca&sslrootcert=/etc/agentiik/db-ca.pem",
			"postgresql://agentiik@db/agentiik?sslmode=require",
			"postgres://agentiik@db/agentiik?ssl=true",
			"postgres://agentiik@/agentiik?host=/run/postgresql",
			"postgres://agentiik@/agentiik?host=/run/postgresql,/var/run/postgresql",
		},
		config.BusURL:    {"tls://nats:4222", "wss://nats.example.com", "wss://nats.example.com:443/bus", "tls://nats-1:4222, tls://nats-2:4222"},
		config.PublicURL: {"https://agentiik.example.com", "https://agentiik.example.com:8443/agentiik"},
	} {
		for _, v := range values {
			i := anInstallation(t)
			i.env[variable] = v
			if _, err := config.ReadAPI(theAPI.environment(i)); err != nil {
				t.Errorf("%s=%s: %v", variable, v, err)
			}
		}
	}
}

// The collector is reached over https, or over http where it is on this machine, which is where
// one usually is: a span names a namespace, a workflow and its steps, and nothing here crosses a
// network in plaintext.
func TestTheCollectorIsReachedOverHTTPSOrOnThisMachine(t *testing.T) {
	for written, kept := range map[string]string{
		"https://otel.example.com":         "https://otel.example.com",
		"https://otel.example.com:4318/":   "https://otel.example.com:4318",
		"https://example.com/otlp//":       "https://example.com/otlp",
		"http://localhost:4318":            "http://localhost:4318",
		"http://127.0.0.1:4318/":           "http://127.0.0.1:4318",
		"http://[::1]:4318":                "http://[::1]:4318",
		"http://127.0.0.2:4318/collector/": "http://127.0.0.2:4318/collector",
	} {
		i := anInstallation(t)
		i.env[config.OTLPEndpoint] = written
		c, err := config.ReadController(theController.environment(i))
		if err != nil || c.OTLPEndpoint != kept {
			t.Errorf("%s is kept as %q: %v", written, c.OTLPEndpoint, err)
		}
	}
	for _, v := range []string{
		"http://otel-collector:4318",
		"http://10.0.0.7:4318",
		"grpc://localhost:4317",
		"localhost:4318",
		"https://user:s3cr3t@otel.example.com",
		"https://otel.example.com?token=s3cr3t",
		"https://otel.example.com#",
		"https:///v1",
	} {
		i := anInstallation(t)
		i.env[config.OTLPEndpoint] = v
		_, err := config.ReadController(theController.environment(i))
		if names := refused(err); !slices.Equal(names, []string{config.OTLPEndpoint}) {
			t.Errorf("%s was refused naming %v: %v", v, names, err)
			continue
		}
		saysNothingOf(t, err, "s3cr3t")
	}
}

// The public URL is kept with no slash at its end, however many it was written with, since every
// URL minted on it adds a path beginning with one.
func TestAPublicURLIsKeptWithNoSlashAtItsEnd(t *testing.T) {
	for written, kept := range map[string]string{
		"https://agentiik.example.com":             "https://agentiik.example.com",
		"https://agentiik.example.com/":            "https://agentiik.example.com",
		"https://agentiik.example.com//":           "https://agentiik.example.com",
		"https://example.com/agentiik///":          "https://example.com/agentiik",
		"https://agentiik.example.com:8443/api/v2": "https://agentiik.example.com:8443/api/v2",
	} {
		i := anInstallation(t)
		i.env[config.PublicURL] = written
		api, err := config.ReadAPI(theAPI.environment(i))
		if err != nil || api.PublicURL != kept {
			t.Errorf("%s is kept as %s: %v", written, api.PublicURL, err)
		}
	}
}

// "A secret is only ever a file named by an _FILE variable, never a value in the environment."
// A secret written as a value is refused wherever it is written, by every program whether or not
// it reads that secret, and the refusal never repeats it.
func TestASecretPassedAsAValueIsRefused(t *testing.T) {
	type value struct {
		variable string
		set      func(i *installation) string
		programs []program
	}
	as := func(v string) func(*installation) string { return func(*installation) string { return v } }
	values := map[string]value{
		"the database password":      {"AGK_DATABASE_PASSWORD", as("hunter2"), everyProgram},
		"the migration password":     {"AGK_MIGRATE_DATABASE_PASSWORD", as("hunter3"), everyProgram},
		"the bus credential":         {"AGK_BUS_CREDENTIALS", func(i *installation) string { return i.busJWT }, everyProgram},
		"the account seed":           {"AGK_BUS_ACCOUNT_SEED", func(i *installation) string { return i.accountSeed }, everyProgram},
		"the presign key":            {"AGK_PRESIGN_KEY", as("c2lnbmluZyBrZXkgb2YgdGhpcnR5IHR3byBieXRlcyE="), everyProgram},
		"the master key":             {"AGK_MASTER_KEY", as("id: 2026-09 key: c2VjcmV0"), everyProgram},
		"the operator token":         {"AGK_OPERATOR_TOKEN", as("agkoperator_" + strings.Repeat("Z", 43)), everyProgram},
		"the metrics token":          {"AGK_METRICS_TOKEN", as("s3cr3t-scrape-token"), everyProgram},
		"a password in the database": {config.DatabaseURL, as("postgres://agentiik:hunter2@db/agentiik"), everyProgram},

		// Not the installation's variables, but pgx's, which it signs in with where the URL
		// gives no password, as libpq does.
		"the database password as libpq takes it":     {"PGPASSWORD", as("hunter2"), everyProgram},
		"the client key's password as libpq takes it": {"PGSSLPASSWORD", as("hunter6"), everyProgram},

		"a password as a parameter":     {config.DatabaseURL, as("postgres://agentiik@db/agentiik?password=hunter2"), everyProgram},
		"a key password as a parameter": {config.DatabaseURL, as("postgres://agentiik@db/agentiik?sslpassword=hunter2"), everyProgram},
		"a key password holding a ;":    {config.DatabaseURL, as("postgres://agentiik@db/agentiik?sslpassword=hunter;2"), everyProgram},
		"a password in the migration":   {config.MigrateDatabaseURL, as("postgres://postgres:hunter3@db/agentiik"), []program{migrating}},
		"a password in the bus":         {config.BusURL, as("nats://controller:hunter4@nats:4222"), []program{theAPI, theController}},
		"a token in the bus":            {config.BusURL, as("nats://s3cr3tt0k3n@nats:4222"), []program{theAPI, theController}},
		"a user in the public URL":      {config.PublicURL, as("https://admin:hunter5@agentiik.example.com"), []program{theAPI}},

		// The secret pasted into the variable that should name its file. A seed or a hash is
		// not an absolute path, and base64 that begins with a slash names no file there is.
		"a seed for its file":              {config.BusAccountSeedFile, func(i *installation) string { return i.accountSeed }, []program{theAPI}},
		"a hash for its file":              {config.OperatorTokenFile, func(i *installation) string { return i.operatorToken }, []program{theAPI}},
		"a key beginning with a slash":     {config.PresignKeyFile, as("/k3yM4t3r1al+0f/th1rty/tw0/byt3s+w0rth="), []program{theAPI}},
		"a password for its file":          {config.DatabasePasswordFile, as("hunter2"), everyProgram},
		"a credential for its file":        {config.BusCredentialsFile, func(i *installation) string { return i.busJWT }, []program{theAPI, theController}},
		"a master key's line for its file": {config.MasterKeyFile, as("key: c2VjcmV0"), []program{theAPI}},
	}
	for what, v := range values {
		for _, p := range v.programs {
			t.Run(fmt.Sprintf("%s, for %s", what, p.name), func(t *testing.T) {
				i := anInstallation(t)
				secret := v.set(i)
				i.env[v.variable] = secret
				err := p.read(p.environment(i))
				if names := refused(err); !slices.Equal(names, []string{v.variable}) {
					t.Fatalf("the start was refused naming %v: %v", names, err)
				}
				saysNothingOf(t, err, append(i.secrets, secret, "hunter")...)
			})
		}
	}
}

// A password holding a /, a ?, a # or a % is where parsers part ways. net/url reads the user and
// the start of the password as a host and a port and quotes them in its error, quotes a % and what
// follows it, or, where the password begins with digits, reads no user at all and takes the rest
// for a path. Whichever happens, the URL is refused as carrying a secret, pointing at the file the
// secret belongs in, and no piece of the password is repeated.
func TestAPasswordAParserMisreadsIsRefusedAndNotRepeated(t *testing.T) {
	type misread struct {
		variable string
		value    string
		pieces   []string
		instead  string
		programs []program
	}
	database, migration := config.DatabasePasswordFile, config.MigrateDatabasePasswordFile
	both := []program{theAPI, theController}
	cases := map[string]misread{
		"a database password holding a /":             {config.DatabaseURL, "postgres://agentiik:Xy9Qk/Lm2+Zt@db:5432/agentiik", []string{"Xy9Qk", "Lm2+Zt"}, database, everyProgram},
		"a database password holding a ?":             {config.DatabaseURL, "postgres://agentiik:Xy9Qk?Lm2+Zt@db:5432/agentiik", []string{"Xy9Qk", "Lm2+Zt"}, database, everyProgram},
		"a database password holding a #":             {config.DatabaseURL, "postgres://agentiik:Xy9Qk#Lm2+Zt@db:5432/agentiik", []string{"Xy9Qk", "Lm2+Zt"}, database, everyProgram},
		"a database password holding a stray %":       {config.DatabaseURL, "postgres://agentiik:Xy9%Qk@db:5432/agentiik", []string{"Xy9", "%Qk"}, database, everyProgram},
		"a database password of digits, then a /":     {config.DatabaseURL, "postgres://agentiik:2718/28Qk@db:5432/agentiik", []string{"2718", "28Qk"}, database, everyProgram},
		"a migration password of digits, then a /":    {config.MigrateDatabaseURL, "postgres://postgres:1234/5678Qk@db:5432/agentiik", []string{"1234", "5678Qk"}, migration, []program{migrating}},
		"a migration password holding a /":            {config.MigrateDatabaseURL, "postgres://postgres:Xy9Qk/Lm2@db:5432/agentiik", []string{"Xy9Qk", "Lm2"}, migration, []program{migrating}},
		"a bus password holding a /":                  {config.BusURL, "tls://controller:Xy9Qk/Lm2@nats:4222", []string{"Xy9Qk", "Lm2"}, config.BusCredentialsFile, both},
		"a bus password of digits, then a /":          {config.BusURL, "tls://controller:2718/28Qk@nats:4222", []string{"2718", "28Qk"}, config.BusCredentialsFile, both},
		"a bus token holding a /":                     {config.BusURL, "tls://s3cr/3tt0k3n@nats:4222", []string{"s3cr", "3tt0k3n"}, config.BusCredentialsFile, both},
		"a bus password holding a stray %":            {config.BusURL, "tls://controller:Xy9%Qk@nats:4222", []string{"Xy9", "%Qk"}, config.BusCredentialsFile, both},
		"a bus password holding a ,":                  {config.BusURL, "tls://controller:Xy9Qk,Lm2@nats:4222", []string{"Xy9Qk", "Lm2"}, config.BusCredentialsFile, both},
		"a second bus server's password holding a ?":  {config.BusURL, "tls://nats-1:4222,tls://controller:Xy9Qk?Lm2@nats-2:4222", []string{"Xy9Qk", "Lm2"}, config.BusCredentialsFile, both},
		"a public URL's password holding a /":         {config.PublicURL, "https://admin:Xy9Qk/Lm2@agentiik.example.com", []string{"Xy9Qk", "Lm2"}, "", []program{theAPI}},
		"a public URL's password holding a stray %":   {config.PublicURL, "https://admin:Xy9%Qk@agentiik.example.com", []string{"Xy9", "%Qk"}, "", []program{theAPI}},
		"a public URL's password of digits, then a ?": {config.PublicURL, "https://admin:2718?28Qk@agentiik.example.com", []string{"2718", "28Qk"}, "", []program{theAPI}},
	}
	for what, c := range cases {
		for _, p := range c.programs {
			t.Run(fmt.Sprintf("%s, for %s", what, p.name), func(t *testing.T) {
				i := anInstallation(t)
				i.env[c.variable] = c.value
				err := p.read(p.environment(i))
				if names := refused(err); !slices.Equal(names, []string{c.variable}) {
					t.Fatalf("the start was refused naming %v: %v", names, err)
				}
				saysNothingOf(t, err, append(i.secrets, c.pieces...)...)
				if c.instead != "" && !strings.Contains(err.Error(), c.instead) {
					t.Errorf("the refusal does not say the secret belongs in %s: %s", c.instead, err)
				}
			})
		}
	}
}

// Every setting that refuses the start is named on that one start, rather than one per restart.
func TestEverySettingThatRefusesTheStartIsNamedOnIt(t *testing.T) {
	i := anInstallation(t)
	delete(i.env, config.DatabaseURL)
	i.env[config.MaxRequeues] = "-1"
	i.env[config.TaskCeiling] = "forever"
	i.env["AGK_MASTER_KEY"] = "id: 2026-09"
	chmod(t, i.env[config.DatabasePasswordFile], 0o644)

	_, err := config.ReadController(theController.environment(i))
	names := refused(err)
	slices.Sort(names)
	want := []string{"AGK_MASTER_KEY", config.DatabaseURL, config.DatabasePasswordFile, config.MaxRequeues, config.TaskCeiling}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Errorf("the start was refused naming %v, and %v all refuse it: %v", names, want, err)
	}

	// A database URL refused for what it is still leaves its password's file to be read.
	i = anInstallation(t)
	i.env[config.MigrateDatabaseURL] = "mysql://postgres@db/agentiik"
	chmod(t, i.env[config.MigrateDatabasePasswordFile], 0o640)
	i.env[config.DatabaseURL] = "postgres://agentiik:hunter2@db/agentiik"
	chmod(t, i.env[config.DatabasePasswordFile], 0o644)

	_, err = config.ReadMigration(migrating.environment(i))
	names = refused(err)
	slices.Sort(names)
	want = []string{config.DatabaseURL, config.DatabasePasswordFile, config.MigrateDatabaseURL, config.MigrateDatabasePasswordFile}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Errorf("the start was refused naming %v, and %v all refuse it: %v", names, want, err)
	}
}

// Each program reads its own settings, and asks its environment for nothing else: not a file only
// another program holds, and nothing under AGK_DEV_, where the API reads nothing for itself and the
// namespaces opted in to env keep their variables. The two libpq variables pgx takes a secret from
// are asked for too, and only to be refused.
func TestEachProgramReadsOnlyWhatItNeeds(t *testing.T) {
	neverAsked := map[string][]string{
		theAPI.name: {
			config.MaxRequeues, config.MigrateDatabaseURL, config.MigrateDatabasePasswordFile,
			config.AuditExportURL, config.AuditExportTokenFile, config.MetricsListen, config.MetricsTokenFile,
			config.OTLPEndpoint,
		},
		theController.name: {
			config.PublicURL, config.PresignKeyFile, config.BusAccountSeedFile, config.OperatorTokenFile,
			config.EnvPrefixes, config.Listen, config.JoinRotation, config.RevocationGrace,
			config.MigrateDatabaseURL, config.MigrateDatabasePasswordFile,
		},
		migrating.name: {
			config.BusURL, config.BusCredentialsFile, config.BusAccountSeedFile, config.ObjectsDir,
			config.PublicURL, config.PresignKeyFile, config.MasterKeyFile, config.OperatorTokenFile,
			config.EnvPrefixes, config.Listen, config.MaxRequeues, config.TaskCeiling,
			config.JoinRotation, config.RevocationGrace, config.AuditExportURL, config.AuditExportTokenFile,
			config.MetricsListen, config.MetricsTokenFile, config.OTLPEndpoint,
		},
	}
	i := anInstallation(t)
	for _, p := range everyProgram {
		env := maps.Clone(i.env)
		for _, name := range p.leaves {
			delete(env, name)
		}
		var asked []string
		if err := p.read(lookupIn(env, &asked)); err != nil {
			t.Fatalf("%s: %v", p.name, err)
		}
		for _, name := range asked {
			if slices.Contains(neverAsked[p.name], name) {
				t.Errorf("%s asked for %s", p.name, name)
			}
			if name == "PGPASSWORD" || name == "PGSSLPASSWORD" {
				continue
			}
			if !strings.HasPrefix(name, "AGK_") || strings.HasPrefix(name, "AGK_DEV_") {
				t.Errorf("%s asked for %s, which is not a variable of the installation's", p.name, name)
			}
		}
	}
}

// A file a secret is in may be a link to it, as a container platform mounts one, and a NATS
// credential or seed reads the way nats and nsc write it: decorated, or a seed on its own line.
func TestASecretsFileIsReadAsItsToolsWriteIt(t *testing.T) {
	i := anInstallation(t)

	// A link to the file, where the link's own mode is not the file's, holding a key of 32 bytes
	// written without the padding base64 gives it.
	key := randomBytes(t, 32)
	if !strings.HasSuffix(base64.StdEncoding.EncodeToString(key), "=") {
		t.Fatal("a key of 32 bytes has no padding to leave out, so the case proves nothing")
	}
	target := i.write(t, "..data-presign.key", []byte(base64.RawStdEncoding.EncodeToString(key)))
	link := filepath.Join(i.dir, "linked-presign.key")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	i.env[config.PresignKeyFile] = link

	// An account seed between the lines nsc writes it in.
	decorated, err := jwt.DecorateSeed([]byte(i.accountSeed))
	if err != nil {
		t.Fatal(err)
	}
	i.env[config.BusAccountSeedFile] = i.write(t, "account.nk", decorated)

	api, err := config.ReadAPI(theAPI.environment(i))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal([]byte(api.PresignKey), key) {
		t.Error("a presign key read through a link, in base64 without padding, is not the key written")
	}
	if string(api.AccountSeed) != i.accountSeed {
		t.Error("a decorated account seed is not the seed written")
	}

	// A credential that never expires reads as one, rather than as one that expired in 1970.
	account, _ := nkeys.CreateAccount()
	token, seed := aUserCredential(t, account, time.Time{})
	creds, err := jwt.FormatUserConfig(token, []byte(seed))
	if err != nil {
		t.Fatal(err)
	}
	i.env[config.BusCredentialsFile] = i.write(t, "forever.creds", creds)
	controller, err := config.ReadController(theController.environment(i))
	if err != nil {
		t.Fatal(err)
	}
	if !controller.Bus.Expires.IsZero() || controller.Bus.JWT != token || string(controller.Bus.Seed) != seed {
		t.Errorf("a credential that never expires reads %+v", controller.Bus)
	}
}

// The files bus.NewInstallation writes are the ones the API and the controller are given, so each is
// read as its program reads it: the account seed by the API, and the control plane's credential by
// both. A bus identity written in a form its own programs refuse is one an installation cannot start
// on.
func TestTheBusFilesAnInstallationIsCreatedWithAreReadByTheirPrograms(t *testing.T) {
	until := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	created, err := bus.NewInstallation(filepath.Join(t.TempDir(), "bus"), until)
	if err != nil {
		t.Fatal(err)
	}
	i := anInstallation(t)
	i.env[config.BusAccountSeedFile] = created.AccountSeed
	i.env[config.BusCredentialsFile] = created.ControlPlane

	api, err := config.ReadAPI(theAPI.environment(i))
	if err != nil {
		t.Fatalf("the API refuses the files an installation is created with: %s", err)
	}
	account, err := nkeys.FromSeed([]byte(api.AccountSeed))
	if err != nil {
		t.Fatal(err)
	}
	if public, _ := account.PublicKey(); public != created.Account {
		t.Errorf("the API reads the seed of %s, and the installation's account is %s", public, created.Account)
	}
	controller, err := config.ReadController(theController.environment(i))
	if err != nil {
		t.Fatalf("the controller refuses the credential an installation is created with: %s", err)
	}
	for program, b := range map[string]config.Bus{"the API": api.Bus, "the controller": controller.Bus} {
		claims, err := jwt.DecodeUserClaims(b.JWT)
		if err != nil {
			t.Fatal(err)
		}
		if claims.Issuer != created.Account || !b.Expires.Equal(until) {
			t.Errorf("%s reads a credential issued by %s until %s", program, claims.Issuer, b.Expires)
		}
	}
}

// The database's password reaches the connection db.Open makes, escaped however it is written, and
// nothing that prints a Database.
func TestADatabasePasswordReachesTheConnectionAndNothingElse(t *testing.T) {
	i := anInstallation(t)
	api, err := config.ReadAPI(theAPI.environment(i))
	if err != nil {
		t.Fatal(err)
	}

	u, err := url.Parse(api.Database.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	if password, _ := u.User.Password(); password != i.databasePassword || u.User.Username() != "agentiik" {
		t.Errorf("the connection signs in as %q with %q", u.User.Username(), password)
	}
	if u.Host != "db:5432" || u.Path != "/agentiik" || u.Query().Get("sslmode") != "verify-full" {
		t.Errorf("the connection lost part of its URL: %s", u.Redacted())
	}
	for _, verb := range verbs {
		printsNoSecret(t, fmt.Sprintf(verb, api.Database), i.databasePassword)
	}

	// Without a password, the connection is the URL as it was written.
	d := config.Database{URL: "postgres://agentiik@db/agentiik", Role: "agentiik"}
	if d.ConnString() != d.URL {
		t.Errorf("a database with no password connects to %s", d.ConnString())
	}
}

// A database URL is read the way pgx, which connects with it, reads it, and not the way net/url
// does: the role is the one pgx signs in as, and a password is refused wherever pgx would find one.
// Every case here is checked against pgx itself, so the two readings cannot drift apart unseen.
func TestADatabaseURLIsReadAsPgxReadsIt(t *testing.T) {
	// Nothing of the environment the test runs in is pgx's to sign in with.
	t.Setenv("PGUSER", "")
	t.Setenv("PGPASSWORD", "")
	t.Setenv("PGPASSFILE", filepath.Join(t.TempDir(), "no.pgpass"))

	signsInAs := map[string]string{
		"postgres://agentiik@db:5432/agentiik?sslmode=require":                      "agentiik",
		"postgres://agentiik@db:5432/agentiik?user=app&sslmode=require":             "app",
		"postgres://db:5432/agentiik?user=app&sslmode=require":                      "app",
		"postgres://agentiik@db:5432/agentiik?sslmode=require&application_name=a;b": "agentiik",
	}
	for raw, role := range signsInAs {
		i := anInstallation(t)
		i.env[config.DatabaseURL] = raw
		api, err := config.ReadAPI(theAPI.environment(i))
		if err != nil {
			t.Errorf("%s: %v", raw, err)
			continue
		}
		read, err := pgconn.ParseConfig(api.Database.ConnString())
		if err != nil {
			t.Fatalf("%s: pgx cannot read the connection string: %v", raw, err)
		}
		if api.Database.Role != role || read.User != role || read.Password != i.databasePassword {
			t.Errorf("%s reads as the role %q, and pgx signs in as %q, with the password given: %t", raw, api.Database.Role, read.User, read.Password == i.databasePassword)
		}
	}

	passwords := []string{
		"postgres://agentiik@db:5432/agentiik?sslmode=require&password=Tr0ub;dor",
		"postgres://agentiik@db:5432/agentiik?sslmode=require& password =Tr0ub4dor",
		"postgres://agentiik@db:5432/agentiik?sslmode=require&pass%77ord=Tr0ub4dor",
	}
	for _, raw := range passwords {
		if read, err := pgconn.ParseConfig(raw); err != nil || read.Password == "" {
			t.Fatalf("%s: pgx finds no password in it, so the case proves nothing: %v", raw, err)
		}
		for _, p := range everyProgram {
			i := anInstallation(t)
			i.env[config.DatabaseURL] = raw
			err := p.read(p.environment(i))
			if names := refused(err); !slices.Equal(names, []string{config.DatabaseURL}) {
				t.Errorf("%s, for %s: the start was refused naming %v: %v", raw, p.name, names, err)
				continue
			}
			saysNothingOf(t, err, "Tr0ub")
		}
	}
}

// verbs are the ways fmt prints a value, each of which prints a string differently.
var verbs = []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X"}

// Printing a configuration, whichever verb prints it and whether whole or a part of it, or
// marshalling it as a log handler does, shows none of the secrets it holds, so that a program that
// logs what it was configured with, or wraps it in an error, logs no secret.
func TestPrintingAConfigurationShowsNoSecret(t *testing.T) {
	i := anInstallation(t)
	api, err := config.ReadAPI(theAPI.environment(i))
	if err != nil {
		t.Fatal(err)
	}
	controller, err := config.ReadController(theController.environment(i))
	if err != nil {
		t.Fatal(err)
	}
	migration, err := config.ReadMigration(migrating.environment(i))
	if err != nil {
		t.Fatal(err)
	}
	secrets := []string{
		i.databasePassword, i.adminPassword, i.busSeed, i.accountSeed, string(i.presignKey),
		base64.StdEncoding.EncodeToString(i.presignKey), string(i.masterKey),
	}
	for _, c := range []any{api, controller, migration, api.Database, api.Bus, migration.Admin, &api, &controller} {
		for _, verb := range verbs {
			printsNoSecret(t, fmt.Sprintf(verb, c), secrets...)
		}
		marshalled, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		printsNoSecret(t, string(marshalled), secrets...)
	}
}

// printsNoSecret fails if printed shows any of secrets in any form fmt, encoding/json or net/url
// writes a string or bytes in.
func printsNoSecret(t *testing.T, printed string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		quoted, marshalled := strconv.Quote(secret), must(json.Marshal(secret))
		shown := []string{
			secret, quoted[1 : len(quoted)-1], string(marshalled[1 : len(marshalled)-1]),
			strings.Trim(fmt.Sprint([]byte(secret)), "[]"),
			hex.EncodeToString([]byte(secret)), strings.ToUpper(hex.EncodeToString([]byte(secret))),
			base64.StdEncoding.EncodeToString([]byte(secret)),
			url.QueryEscape(secret), url.PathEscape(secret),
			strings.TrimPrefix(url.UserPassword("role", secret).String(), "role:"),
		}
		for _, form := range shown {
			if strings.Contains(printed, form) {
				t.Errorf("%q is shown as %q in %s", secret, form, printed)
				break
			}
		}
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// A program that checks a setting further than this package can still refuses the start naming
// the variable, as the API does with the master key it alone may parse.
func TestAProgramRefusesASettingByItsVariable(t *testing.T) {
	cause := errors.New("secret: the master key has no id")
	err := config.Refuse(config.MasterKeyFile, cause)
	if !errors.Is(err, cause) || !slices.Equal(refused(err), []string{config.MasterKeyFile}) || !strings.HasPrefix(err.Error(), "config: AGK_MASTER_KEY_FILE ") {
		t.Errorf("the refusal reads %v", err)
	}
}

// A token for the metrics with no address to answer them on guards nothing, and is the sign of an
// installation that meant to open the port and did not say where: it is refused, naming the token.
func TestAMetricsTokenWithNoAddressIsRefused(t *testing.T) {
	i := anInstallation(t)
	delete(i.env, config.MetricsListen)
	_, err := config.ReadController(theController.environment(i))
	if names := refused(err); !slices.Equal(names, []string{config.MetricsTokenFile}) {
		t.Fatalf("the start was refused naming %v: %v", names, err)
	}
	saysNothingOf(t, err, i.secrets...)
}
