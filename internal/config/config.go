// Package config reads an installation's configuration, for the two programs that serve it.
//
// The API and the controller are configured the way a runner is: by AGK_* environment variables,
// which a systemd unit, a Compose file and a container platform all set the same way, and which
// need no file format of their own. A secret is the exception, and the exception is the point. A
// secret is never a value in the environment, which every process the program starts inherits and
// anybody allowed to inspect the process or its container reads. It is a file readable by its
// owner alone, named by a variable ending _FILE, and a secret written as the value of a variable is
// refused rather than used.
//
// # Refusing to start
//
// A setting that is missing, unreadable or malformed refuses the start and names the variable. A
// server that started anyway would fail later, on the first request needing what it could not
// read, and one that fell back to a default would be running an installation nobody configured.
// Every setting that refuses a start is named on that start, rather than one per restart.
//
// A refusal never repeats the value of a setting that can hold a secret. Not a URL, which may
// carry a password, nor what its parser said of it, which quotes part of it, and not the path a
// _FILE variable holds either: a secret pasted into the variable that should name its file is a
// value like any other, and repeating it would put it in whatever log the refusal reaches.
//
// # Each program reads what it needs
//
// ReadAPI, ReadController and ReadMigration each read the settings of one program, or of one verb
// of it, and nothing else, so a program never opens a file it has no use for. The controller goes
// one step further with the master key. "The master key, held by the API alone, is what keeps them
// from it": the sealed values sit in the database the controller shares, so a controller whose
// environment names the master key's file is an installation deployed with that key in the
// controller's reach, and it refuses to start.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agentiik/agentiik/graph"
	"github.com/nats-io/jwt/v2"
)

// The variables, spelled once so that a program wrapping a refusal of its own names the same one.
const (
	DatabaseURL                 = "AGK_DATABASE_URL"
	DatabasePasswordFile        = "AGK_DATABASE_PASSWORD_FILE"
	MigrateDatabaseURL          = "AGK_MIGRATE_DATABASE_URL"
	MigrateDatabasePasswordFile = "AGK_MIGRATE_DATABASE_PASSWORD_FILE"
	BusURL                      = "AGK_BUS_URL"
	BusCredentialsFile          = "AGK_BUS_CREDENTIALS_FILE"
	BusAccountSeedFile          = "AGK_BUS_ACCOUNT_SEED_FILE"
	ObjectsDir                  = "AGK_OBJECTS_DIR"
	PublicURL                   = "AGK_PUBLIC_URL"
	PresignKeyFile              = "AGK_PRESIGN_KEY_FILE"
	MasterKeyFile               = "AGK_MASTER_KEY_FILE"
	EnvPrefixes                 = "AGK_ENV_PREFIXES"
	Listen                      = "AGK_LISTEN"
	MaxRequeues                 = "AGK_MAX_REQUEUES"
	TaskCeiling                 = "AGK_TASK_CEILING"
	JoinRotation                = "AGK_JOIN_ROTATION"
	RevocationGrace             = "AGK_REVOCATION_GRACE"
	OperatorTokenFile           = "AGK_OPERATOR_TOKEN_FILE"
)

// secretFiles are the variables that name a secret's file. The same name without _FILE is the
// variable somebody sets when they mean to pass the secret itself, and every program refuses it,
// whether or not it reads the file.
var secretFiles = []string{
	DatabasePasswordFile, MigrateDatabasePasswordFile, BusCredentialsFile, BusAccountSeedFile,
	PresignKeyFile, MasterKeyFile, OperatorTokenFile,
}

// DefaultListen is where the API listens when AGK_LISTEN is unset.
//
// Every interface, on 8080. Every profile terminates TLS in front of the API, so what listens here
// speaks plain HTTP to that terminator, and it runs as a user that cannot bind a port below 1024:
// 8080 is the port such a service conventionally takes.
const DefaultListen = ":8080"

// DefaultTaskCeiling is how long a task no timeout bounds may run, where AGK_TASK_CEILING is unset.
//
// The installation ceiling row of the step keywords: "A step when none of the three is written. An
// hour by default."
const DefaultTaskCeiling = time.Hour

// DefaultJoinRotation is how long a runner credential is accepted for, where AGK_JOIN_ROTATION is
// unset. Thirty days, because "a machine dark for a month should be reconsidered, not readmitted".
const DefaultJoinRotation = 30 * 24 * time.Hour

// fileMaxBytes is the most a file an _FILE variable names is read to. The largest of them, a NATS
// credential, is under two kibibytes, and a file of more is not the one the variable should name.
const fileMaxBytes = 64 << 10

// presignKeyMinBytes is the shortest presign key, which artifact.NewSigned refuses anything under.
const presignKeyMinBytes = 32

// Lookup reads one variable of the environment, as os.LookupEnv does.
type Lookup func(name string) (string, bool)

// Error is one setting that refuses the start.
//
// Reason follows the variable's name in the message, so that every refusal names the variable it
// is about by construction rather than by each message remembering to.
type Error struct {
	Variable string
	Reason   string
	Err      error
}

func (e *Error) Error() string { return "config: " + e.Variable + " " + e.Reason }

func (e *Error) Unwrap() error { return e.Err }

// Refuse is a program refusing a setting this package read but could not check to the end.
//
// Two settings are checked to the end in the API alone. Only the API may link the package that
// parses a master key, so this package reads the file and the API's main package parses it. What
// an env prefix may be is api.Environment.Check's, and writing the rule again here would be two
// rules that drift. A key or a prefix the API cannot use still refuses the start, naming the
// variable it was read from.
func Refuse(variable string, err error) error {
	return &Error{Variable: variable, Reason: "cannot be used: " + err.Error(), Err: err}
}

// Database is one connection to PostgreSQL: where, as whom, and what that role signs in with.
type Database struct {
	// URL is the address as written, which never carries a password.
	URL string

	// Role is the user the URL names.
	Role string

	// Password is read from the file the matching _FILE variable names, and is empty where
	// there is none: the role then signs in some other way, with a client certificate the URL
	// names or as the operating system's user over a local socket.
	Password string
}

// ConnString is the URL with the password in it, which is what db.Open takes. It is for opening a
// connection and nothing else, and nothing may print it.
func (d Database) ConnString() string {
	if d.Password == "" {
		return d.URL
	}
	u, err := url.Parse(d.URL)
	if err != nil {
		// Unreachable for a Database this package read, whose URL it parsed already.
		return d.URL
	}
	u.User = url.UserPassword(d.Role, d.Password)
	return u.String()
}

// String is the URL as written, so that printing a Database prints no password.
func (d Database) String() string { return d.URL }

// Bus is where the control plane reaches the bus, and the credential it reaches it with.
type Bus struct {
	// URL is the bus's address, one or several separated by commas. It is also the address a
	// runner is handed with its bus credential, so it has to be one every runner reaches.
	URL string

	// JWT and Seed are the control plane's NATS user credential. Expires is when it stops
	// working, and is zero for one that never does.
	JWT     string
	Seed    string
	Expires time.Time
}

// API is what agentiik-api serve reads.
type API struct {
	Database Database
	Bus      Bus

	// AccountSeed is the NATS account seed runner bus credentials are minted with.
	AccountSeed string

	// Objects is the directory the built-in object store keeps every object in.
	Objects string

	// PublicURL is the address runners and clients reach the API at, with no trailing slash.
	// Every presigned URL is minted on it rather than on a request's Host header.
	PublicURL string

	// PresignKey signs every presigned URL and upload policy.
	PresignKey []byte

	// MasterKey is the master key file as it was read. The API's main package parses it,
	// because only the API may link the secret store that knows how.
	MasterKey []byte

	// EnvPrefixes gives each namespace opted in to env the prefix its variables begin with. Nil
	// opts in none, which is what an installation that is not for development is.
	EnvPrefixes map[string]string

	Listen          string
	JoinRotation    time.Duration
	RevocationGrace time.Duration

	// OperatorToken is the SHA-256 of the interim operator token, in lowercase hexadecimal.
	OperatorToken string
}

// Controller is what agentiik-controller reads.
type Controller struct {
	Database    Database
	Bus         Bus
	Objects     string
	MaxRequeues int
	TaskCeiling time.Duration
}

// Migration is what agentiik-api migrate reads.
type Migration struct {
	// Admin is the role that applies the migrations, which may change the schema.
	Admin Database

	// Application is the role the API and the controller connect as, which migrating creates:
	// the role AGK_DATABASE_URL names, signing in with what AGK_DATABASE_PASSWORD_FILE holds.
	Application Database
}

// ReadAPI reads the API's configuration through lookup, which is os.LookupEnv when nil.
func ReadAPI(lookup Lookup) (API, error) {
	r := newReader(lookup)
	var c API
	c.Database = r.database(DatabaseURL, DatabasePasswordFile, true)
	c.Bus = r.bus()
	c.AccountSeed = r.accountSeed()
	c.Objects = r.directory(ObjectsDir, "and it is the directory the built-in object store keeps every object in")
	c.PublicURL = r.publicURL()
	c.PresignKey = r.presignKey()
	c.MasterKey = r.file(MasterKeyFile, "and it names the file holding the master key the built-in secret store seals every value under")
	c.EnvPrefixes = r.envPrefixes()
	c.Listen = r.listen()
	c.JoinRotation = r.duration(JoinRotation, DefaultJoinRotation, "how long a runner credential is accepted for",
		"a credential accepted for no time is a runner that cannot join")
	// The grace defaults to the ceiling, since no task legitimately runs longer, so a task a
	// revoked runner was running when it was revoked has the whole of its time to report.
	c.RevocationGrace = r.duration(RevocationGrace, r.taskCeiling(), "how long a revoked runner's results are still taken",
		"revoking a runner never destroys work already done: a grace of no time refuses the results of what it is finishing")
	c.OperatorToken = r.operatorToken()
	return c, r.err()
}

// ReadController reads the controller's configuration through lookup, which is os.LookupEnv
// when nil.
func ReadController(lookup Lookup) (Controller, error) {
	r := newReader(lookup)
	if _, set := r.value(MasterKeyFile); set {
		r.refuse(MasterKeyFile, "is set for the controller, and the master key is held by the API alone: the sealed values sit in the database the controller shares, and the key is what keeps them from it, so its file is given to the API and nothing else")
	}
	var c Controller
	c.Database = r.database(DatabaseURL, DatabasePasswordFile, true)
	c.Bus = r.bus()
	c.Objects = r.directory(ObjectsDir, "and it is the directory the built-in object store keeps every object in, which the controller reads envelopes from")
	c.MaxRequeues = r.maxRequeues()
	c.TaskCeiling = r.taskCeiling()
	return c, r.err()
}

// ReadMigration reads what migrating needs through lookup, which is os.LookupEnv when nil: the
// role that may change the schema, and the role it creates for the API and the controller.
func ReadMigration(lookup Lookup) (Migration, error) {
	r := newReader(lookup)
	var c Migration
	c.Admin = r.database(MigrateDatabaseURL, MigrateDatabasePasswordFile, false)
	c.Application = r.database(DatabaseURL, DatabasePasswordFile, true)
	return c, r.err()
}

// reader reads one program's settings and keeps every refusal.
type reader struct {
	lookup  Lookup
	refused []error
}

func newReader(lookup Lookup) *reader {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	r := &reader{lookup: lookup}
	for _, file := range secretFiles {
		value := strings.TrimSuffix(file, "_FILE")
		if _, set := r.value(value); set {
			r.refuse(value, fmt.Sprintf("is set, and a secret is never read from the environment, which every process started from this one inherits and anybody who can inspect it reads: write it to a file its owner alone can read, and name that file in %s", file))
		}
	}
	return r
}

// value is a variable's value, and whether it is set. Set to nothing is unset: a systemd unit or
// a Compose file writes an empty variable more often by accident than to mean anything.
func (r *reader) value(name string) (string, bool) {
	v, ok := r.lookup(name)
	return v, ok && v != ""
}

func (r *reader) refuse(name, reason string) {
	r.refused = append(r.refused, &Error{Variable: name, Reason: reason})
}

func (r *reader) err() error { return errors.Join(r.refused...) }

// required is a variable that has no default, and why it has none.
func (r *reader) required(name, why string) (string, bool) {
	v, set := r.value(name)
	if !set {
		r.refuse(name, "is not set, "+why)
	}
	return v, set
}

// duration is a positive duration written as Go writes one, 90m or 720h, or fallback where unset.
// The argument what names the setting, and nothing says why no time at all is refused.
func (r *reader) duration(name string, fallback time.Duration, what, nothing string) time.Duration {
	v, set := r.value(name)
	if !set {
		return fallback
	}
	d, err := time.ParseDuration(v)
	switch {
	case err != nil:
		r.refuse(name, fmt.Sprintf("is %q, and it is %s, written as a duration such as 90m or 720h: the units stop at hours", v, what))
		return fallback
	case d <= 0:
		r.refuse(name, fmt.Sprintf("is %s, and %s", v, nothing))
		return fallback
	}
	return d
}

// taskCeiling is the longest a task no timeout bounds may run.
func (r *reader) taskCeiling() time.Duration {
	return r.duration(TaskCeiling, DefaultTaskCeiling, "the longest a task no timeout bounds may run",
		"a task given no time is a task that fails before it starts")
}

// maxRequeues is the installation's max_requeues, graph.DefaultMaxRequeues where unset.
func (r *reader) maxRequeues() int {
	v, set := r.value(MaxRequeues)
	if !set {
		return graph.DefaultMaxRequeues
	}
	n, err := strconv.Atoi(v)
	switch {
	case err != nil:
		r.refuse(MaxRequeues, fmt.Sprintf("is %q, and max_requeues is a whole number of times", v))
		return graph.DefaultMaxRequeues
	case n < 0:
		r.refuse(MaxRequeues, fmt.Sprintf("is %d, and max_requeues is how many times one key is handed out again after a loss: zero is what requeues nothing", n))
		return graph.DefaultMaxRequeues
	}
	return n
}

// listen is the address the API listens on, a host and a port as net.Listen takes them.
func (r *reader) listen() string {
	v, set := r.value(Listen)
	if !set {
		return DefaultListen
	}
	_, port, err := net.SplitHostPort(v)
	if err == nil {
		_, err = strconv.ParseUint(port, 10, 16)
	}
	if err != nil {
		r.refuse(Listen, fmt.Sprintf("is %q, and it is the address the API listens on, a host and a port such as :8080 or 127.0.0.1:8080", v))
		return DefaultListen
	}
	return v
}

// directory is a directory named by an absolute path, which exists.
func (r *reader) directory(name, why string) string {
	v, set := r.required(name, why)
	if !set {
		return ""
	}
	if !filepath.IsAbs(v) {
		r.refuse(name, fmt.Sprintf("is %q, and it is an absolute path, so that where the objects are does not depend on the directory the program was started from", v))
		return ""
	}
	info, err := os.Stat(v)
	switch {
	case err != nil:
		r.refuse(name, "names a directory that cannot be read: "+reasonOf(err))
		return ""
	case !info.IsDir():
		r.refuse(name, fmt.Sprintf("names %s, which is not a directory", v))
		return ""
	}
	return v
}

// publicURL is the address runners and clients reach the API at.
//
// Never repeated in a refusal, since a URL can carry a password, and refused where it does.
func (r *reader) publicURL() string {
	v, set := r.required(PublicURL, "and it is the address every presigned URL is minted on, which a request's Host header must never choose")
	if !set {
		return ""
	}
	if _, has := userinfo(v); has {
		r.refuse(PublicURL, "carries a user, and it is the address a runner is handed, which carries no credential")
		return ""
	}
	u, err := url.Parse(v)
	switch {
	case err != nil:
		r.refuse(PublicURL, "is not a URL"+unparsed)
		return ""
	case u.Scheme != "https" && u.Scheme != "http" || u.Host == "":
		r.refuse(PublicURL, "is not an http or https URL with a host, such as https://agentiik.example.com")
		return ""
	case u.RawQuery != "" || u.Fragment != "":
		r.refuse(PublicURL, "carries a query or a fragment, and every route is a path below it")
		return ""
	}
	return strings.TrimSuffix(v, "/")
}

// database reads one PostgreSQL URL and the file its password is in.
func (r *reader) database(name, passwordFile string, needsRole bool) Database {
	why := "and it is the database the API and the controller share, and share nothing else"
	if name == MigrateDatabaseURL {
		why = "and migrating is done as a role that may change the schema, which the role the API and the controller connect as may not"
	}
	v, set := r.required(name, why)
	if !set {
		return Database{}
	}
	carriesAPassword := fmt.Sprintf("carries a password, and a secret is never a value in the environment: write it to a file its owner alone can read, and name that file in %s", passwordFile)
	// A role is never written with a colon, a slash, a ? or a # before its @, and a password
	// is: after the colon, and holding the others often enough, since a password from openssl
	// rand -base64 holds a slash one time in three. An @ further on, in the database's name or a
	// parameter, is refused with it, because no reading of the text tells it apart from the end
	// of a password.
	if info, has := userinfo(v); has && strings.ContainsAny(info, ":/?#") {
		r.refuse(name, carriesAPassword)
		return Database{}
	}
	u, err := url.Parse(v)
	switch {
	case err != nil:
		r.refuse(name, "is not a URL"+unparsed)
		return Database{}
	case u.Scheme != "postgres" && u.Scheme != "postgresql" || u.Opaque != "":
		r.refuse(name, "is not a PostgreSQL URL, which begins postgres://")
		return Database{}
	}
	params, ok := parameters(v)
	if !ok {
		r.refuse(name, "has a parameter pgx cannot read, which is a key, one = and a value, with no space inside either")
		return Database{}
	}
	if _, has := params["password"]; has {
		r.refuse(name, carriesAPassword)
		return Database{}
	}
	if _, has := params["sslpassword"]; has {
		r.refuse(name, carriesAPassword)
		return Database{}
	}
	d := Database{URL: v, Role: u.User.Username()}
	// pgx signs in as the user a parameter names over the one before the @, so the role is read
	// the same way: migrating creates the role the programs then sign in as, and no other.
	if user, has := params["user"]; has {
		d.Role = user
	}
	if needsRole && d.Role == "" {
		r.refuse(name, "names no role, and it names the role the API and the controller connect as, which migrating creates")
	}
	if password := r.optionalFile(passwordFile); password != nil {
		p := strings.TrimRight(string(password), "\r\n")
		if strings.ContainsAny(p, "\r\n") {
			r.refuse(passwordFile, "names a file of more than one line, and it holds one password")
			return d
		}
		d.Password = p
	}
	return d
}

// parameters is a PostgreSQL URL's query as pgx reads it, which is libpq's reading, and false
// where pgx would refuse it.
//
// The text after the first ?, in pairs separated by &, each cut on its one =, both halves decoded,
// and the last value of a key standing. net/url reads it otherwise: it drops a pair holding a ;
// without a word, and keeps the spaces pgx trims from a key, so a password=Tr0ub;dor it never saw
// would still sign in. The first ? is the query's, since a URL with one before its @ is refused as
// carrying a password before this reads it.
func parameters(raw string) (map[string]string, bool) {
	params := map[string]string{}
	_, query, _ := strings.Cut(raw, "?")
	for query != "" {
		var pair string
		pair, query, _ = strings.Cut(query, "&")
		key, value, found := strings.Cut(pair, "=")
		if !found || strings.Contains(value, "=") {
			return nil, false
		}
		key, keyRead := unescape(key)
		value, valueRead := unescape(value)
		if !keyRead || !valueRead {
			return nil, false
		}
		params[key] = value
	}
	return params, true
}

// unescape decodes one half of a parameter as libpq does: the spaces around it dropped, none
// inside it, and every %XX its byte, except a NUL.
func unescape(s string) (string, bool) {
	s = strings.Trim(s, " ")
	if strings.Contains(s, " ") {
		return "", false
	}
	v, err := url.PathUnescape(s)
	return v, err == nil && !strings.Contains(v, "\x00")
}

// bus reads where the control plane reaches the bus, and its credential.
//
// The credential is a NATS credential file, the JWT and the seed each between the BEGIN and END
// lines nats and nsc write, which is what jwt.FormatUserConfig writes too.
func (r *reader) bus() Bus {
	var b Bus
	if v, set := r.required(BusURL, "and it is the bus the controller publishes tasks on and every runner takes them from"); set {
		b.URL = v
		for _, server := range strings.Split(v, ",") {
			if reason := notABusServer(strings.TrimSpace(server)); reason != "" {
				r.refuse(BusURL, reason)
				break
			}
		}
	}

	content := r.file(BusCredentialsFile, "and it names the file holding the control plane's bus credential")
	if content == nil {
		return b
	}
	token, err := jwt.ParseDecoratedJWT(content)
	if err != nil {
		r.refuse(BusCredentialsFile, "names a file holding no NATS user JWT: "+err.Error())
		return b
	}
	claims, err := jwt.DecodeUserClaims(token)
	if err != nil {
		r.refuse(BusCredentialsFile, "names a file holding no NATS user JWT: "+err.Error())
		return b
	}
	user, err := jwt.ParseDecoratedUserNKey(content)
	if err != nil {
		r.refuse(BusCredentialsFile, "names a file holding no NATS user seed: "+err.Error())
		return b
	}
	public, err := user.PublicKey()
	if err != nil || public != claims.Subject {
		r.refuse(BusCredentialsFile, "names a file whose seed is not the key its JWT was issued to, so the bus would refuse it")
		return b
	}
	seed, err := user.Seed()
	if err != nil {
		r.refuse(BusCredentialsFile, "names a file whose seed cannot be read: "+err.Error())
		return b
	}
	if claims.Expires != 0 {
		b.Expires = time.Unix(claims.Expires, 0).UTC()
		if !b.Expires.After(time.Now()) {
			r.refuse(BusCredentialsFile, fmt.Sprintf("names a credential that expired at %s, which the bus refuses", b.Expires.Format(time.RFC3339)))
			return b
		}
	}
	b.JWT, b.Seed = token, string(seed)
	return b
}

// notABusServer is why one address of AGK_BUS_URL is not one, or nothing where it is. NATS takes
// several separated by commas, which is how a client finds the rest of a cluster of three.
func notABusServer(server string) string {
	if _, has := userinfo(server); has {
		return fmt.Sprintf("carries a user or a token, and a secret is never a value in the environment: the control plane's credential is the file %s names", BusCredentialsFile)
	}
	u, err := url.Parse(server)
	switch {
	case err != nil:
		return "is not a URL, or a list of them separated by commas" + unparsed
	case u.Host == "" || u.Scheme != "nats" && u.Scheme != "tls" && u.Scheme != "ws" && u.Scheme != "wss":
		return "is not a NATS URL with a host, such as nats://nats:4222"
	}
	return ""
}

// accountSeed reads the NATS account seed, plain or between the lines nsc writes it in.
func (r *reader) accountSeed() string {
	content := r.file(BusAccountSeedFile, "and it names the file holding the account seed the API mints runner bus credentials with")
	if content == nil {
		return ""
	}
	account, err := jwt.ParseDecoratedNKey(content)
	if err != nil {
		r.refuse(BusAccountSeedFile, "names a file holding no NATS seed: "+err.Error())
		return ""
	}
	public, err := account.PublicKey()
	if err != nil || !strings.HasPrefix(public, "A") {
		r.refuse(BusAccountSeedFile, "names a seed that is not an account's, and a credential signed by anything else is one no bus trusts")
		return ""
	}
	seed, err := account.Seed()
	if err != nil {
		r.refuse(BusAccountSeedFile, "names a file whose seed cannot be read: "+err.Error())
		return ""
	}
	return string(seed)
}

// presignKey reads the signing key, written in base64 as openssl rand -base64 32 writes one.
//
// Text rather than raw bytes, so that the newline an editor adds changes nothing. Every API of an
// installation holds the same key, for a URL one of them minted to be honoured by another, and a
// key that changed with a newline would be two keys.
func (r *reader) presignKey() []byte {
	content := r.file(PresignKeyFile, "and it names the file holding the key that signs every presigned URL and upload policy")
	if content == nil {
		return nil
	}
	text := strings.TrimSpace(string(content))
	key, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(text)
	}
	switch {
	case err != nil:
		r.refuse(PresignKeyFile, "names a file that is not base64, which is how the key is written")
		return nil
	case len(key) < presignKeyMinBytes:
		r.refuse(PresignKeyFile, fmt.Sprintf("names a key of %d bytes, and a presign key is %d bytes or more, as openssl rand -base64 %d writes one", len(key), presignKeyMinBytes, presignKeyMinBytes))
		return nil
	}
	return key
}

// operatorToken reads the hash of the interim operator token.
//
// The hash and never the token, as every credential of the installation is kept: "stored hashed,
// shown once at creation". A file holding the token itself is refused, since it is a working
// credential at rest on the server.
func (r *reader) operatorToken() string {
	content := r.file(OperatorTokenFile, "and it names the file holding the hash of the operator token, without which every request is refused")
	if content == nil {
		return ""
	}
	hash := strings.TrimSpace(string(content))
	if len(hash) != 64 || strings.ContainsFunc(hash, func(c rune) bool { return !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') }) {
		r.refuse(OperatorTokenFile, "names a file holding something other than the SHA-256 of the operator token in 64 lowercase hexadecimal characters: the file holds the token's hash and never the token")
		return ""
	}
	return hash
}

// envPrefixes reads which namespaces may keep secrets in env, as namespace=prefix pairs separated
// by commas, the way AGK_RUNNER_LABELS writes labels.
//
// This checks the list's form and no more. What a prefix may be, beginning with AGK_DEV_ and
// beginning no other namespace's, is api.Environment.Check's, which the API holds it to when the
// providers are attached, so the rule is written in one place.
func (r *reader) envPrefixes() map[string]string {
	v, set := r.value(EnvPrefixes)
	if !set {
		return nil
	}
	prefixes := map[string]string{}
	for _, pair := range strings.Split(v, ",") {
		namespace, prefix, ok := strings.Cut(pair, "=")
		switch {
		case !ok || namespace == "" || prefix == "":
			r.refuse(EnvPrefixes, fmt.Sprintf("holds %q, and it is a list of namespace=prefix pairs separated by commas, such as finance=AGK_DEV_FINANCE_", pair))
			return nil
		case prefixes[namespace] != "":
			r.refuse(EnvPrefixes, fmt.Sprintf("gives %s a prefix twice", namespace))
			return nil
		}
		prefixes[namespace] = prefix
	}
	return prefixes
}

// file reads a secret's file that the program cannot start without.
func (r *reader) file(name, why string) []byte {
	if _, set := r.required(name, why); !set {
		return nil
	}
	return r.optionalFile(name)
}

// optionalFile reads the file a _FILE variable names, and is nil where the variable is unset or
// the file refuses the start.
//
// The file is held to what "a file the API user alone can open" means for the master key: an
// absolute path, a regular file, and no permission for its group or anybody else. The rule is one
// rule for every file, so that nobody has to decide which of them a reader could use. Refusing is
// better than warning, because a warning in a log nobody reads is how a key stays world readable
// for a year.
func (r *reader) optionalFile(name string) []byte {
	path, set := r.value(name)
	if !set {
		return nil
	}
	if !filepath.IsAbs(path) {
		r.refuse(name, "is not an absolute path, and it names a file: a secret is never the value of a variable, and a file named relative to wherever the program was started is found by accident")
		return nil
	}
	// Stat before opening, since opening a named pipe waits for somebody to write to it.
	info, err := os.Stat(path)
	switch {
	case err != nil:
		r.refuse(name, "names a file that cannot be read: "+reasonOf(err))
		return nil
	case !info.Mode().IsRegular():
		r.refuse(name, "names something that is not a file")
		return nil
	case info.Mode().Perm()&0o077 != 0:
		r.refuse(name, fmt.Sprintf("names a file of mode %#o, and a file holding a secret is readable by its owner alone: chmod 600 it, because a secret anybody on the host can read is a secret anybody on the host has", info.Mode().Perm()))
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		r.refuse(name, "names a file that cannot be read: "+reasonOf(err))
		return nil
	}
	defer f.Close()
	content, err := io.ReadAll(io.LimitReader(f, fileMaxBytes+1))
	switch {
	case err != nil:
		r.refuse(name, "names a file that cannot be read: "+reasonOf(err))
		return nil
	case len(content) > fileMaxBytes:
		r.refuse(name, fmt.Sprintf("names a file of more than %d bytes, larger than any secret it could name", fileMaxBytes))
		return nil
	case strings.TrimSpace(string(content)) == "":
		r.refuse(name, "names an empty file")
		return nil
	}
	return content
}

// reasonOf is what went wrong without the value that went wrong with it. An error from os
// repeats the path it was given, which can be a secret pasted where its file's path belongs.
func reasonOf(err error) string {
	var path *fs.PathError
	if errors.As(err, &path) {
		return path.Err.Error()
	}
	return err.Error()
}

// unparsed ends the refusal of a URL that does not parse.
//
// It stands where net/url's reason would, which quotes the part of the value the parser stopped
// at, and inside a user that is part of a password: invalid port ":Xy9Qk" after host, for a
// password holding a slash after Xy9Qk, or invalid URL escape "%Qk". No reason net/url gives is
// repeated, rather than those two left out, since the next version may quote the value elsewhere.
const unparsed = ", and what its parser said is left out, since it quotes part of the value and the value may hold a password"

// userinfo is what a URL holds between the // after its scheme and its last @, and whether it
// has an @ there at all.
//
// Read on the text rather than through a parser, because a password holding a slash, a ? or a #
// is where parsers part ways. net/url ends the host at the first of them, so user:Xy9/Qk@db is a
// host and a port that fail to parse, or, where the password begins with digits, a host and a
// port that parse, with no user and the rest of the password in the path. libpq, which pgx
// follows, ends the user at the first @ with no slash before it. Up to the last @ is where
// somebody pasting a credential put it, whichever parser is right about the rest.
func userinfo(raw string) (string, bool) {
	_, rest, found := strings.Cut(raw, "//")
	if !found {
		return "", false
	}
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return "", false
	}
	return rest[:at], true
}
