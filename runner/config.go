package runner

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/agentiik/agentiik/internal/token"
)

// The settings, spelled once, so that a refusal and join, which writes some of them, name the same
// variable.
const (
	API         = "AGK_API"
	Labels      = "AGK_RUNNER_LABELS"
	Concurrency = "AGK_RUNNER_CONCURRENCY"
	WorkDir     = "AGK_RUNNER_WORKDIR"
	Namespaces  = "AGK_RUNNER_NAMESPACES"

	// The runner's identity, which join writes into runner.env and nothing else writes: the
	// identifier and the pool the API answered, and the credential it minted.
	RunnerID   = "AGK_RUNNER_ID"
	RunnerPool = "AGK_RUNNER_POOL"
	Credential = "AGK_RUNNER_CREDENTIAL"
)

// EnvPath is the file join writes the runner's identity to, and serve reads.
//
// serve reads it itself rather than having the unit load it with EnvironmentFile=, "so the
// credential stays out of the environment": every process the agent started would inherit it, and
// anybody allowed to inspect the process would read it there.
const EnvPath = "/etc/agentiik/runner.env"

// DefaultWorkDir is where task directories and the record of keys go when AGK_RUNNER_WORKDIR is
// unset. It is under /var/lib/agentiik because that is the one tree the unit's ProtectSystem=strict
// leaves writable, through ReadWritePaths.
const DefaultWorkDir = "/var/lib/agentiik/work"

// MaxConcurrency is the most tasks one host may be told to hold at once.
//
// A heartbeat names every key the host holds, and the API refuses one naming more than 4096, which
// it sizes as more containers than one host runs at once. A concurrency above it would be a host
// whose heartbeat is refused the moment it is full, which is the moment it matters most.
const MaxConcurrency = 4096

// envFileMaxBytes is the most runner.env is read to. What join writes is a few hundred bytes, and
// a file of more is not the one join wrote.
const envFileMaxBytes = 64 << 10

// Lookup reads one variable of the environment, as os.LookupEnv does.
type Lookup func(name string) (string, bool)

// Config is what agk-runner serve is given: the settings of #configuration, from the environment
// and from runner.env, and the identity join wrote.
type Config struct {
	// API is the one address a runner is given, without its trailing slashes; "the API hands
	// out the rest".
	API string

	// Labels are the labels this runner claims, each key=value, in the order they were
	// written.
	Labels []string

	// Concurrency is how many tasks this host holds at once.
	Concurrency int

	// WorkDir is the work root: task directories and the record of keys.
	WorkDir string

	// Namespaces narrows this host to fewer namespaces than its pool accepts. Nil is every
	// namespace the pool accepts, and never an empty list, which would be a host that can be
	// handed nothing.
	Namespaces []string

	// Runner and Pool are the identifier and the pool the API answered at join.
	Runner string
	Pool   string

	// Credential is what every call to the API carries.
	Credential Secret
}

// Secret is a value that is never printed.
//
// A Config is logged, formatted into errors and dumped by whoever is debugging, and the credential
// in it opens the API as this runner. So every way of printing one prints what kind of credential
// it is and nothing of the credential.
type Secret string

func (s Secret) shown() string {
	if s == "" {
		return ""
	}
	if kind, ok := token.KindOf(string(s)); ok {
		return string(kind) + "_[redacted]"
	}
	return "[redacted]"
}

// String is what %s and %v print.
func (s Secret) String() string { return s.shown() }

// GoString is what %#v prints, which would otherwise quote the value.
func (s Secret) GoString() string { return strconv.Quote(s.shown()) }

// MarshalText is what an encoder writes, so that a Config marshalled into a log line carries no
// credential either.
func (s Secret) MarshalText() ([]byte, error) { return []byte(s.shown()), nil }

// Error is one setting that refuses the start.
//
// Reason follows the variable's name, so that every refusal names what it is about by construction
// rather than by each message remembering to.
type Error struct {
	Variable string
	Reason   string
}

func (e *Error) Error() string { return "runner: " + e.Variable + " " + e.Reason }

// ErrNotJoined is runner.env missing, which is a host that has not joined.
var ErrNotJoined = errors.New("runner: this host has not joined an installation: run agk-runner join, which writes the runner's identity to " + EnvPath)

var (
	// givenName is the grammar the API mints a runner identifier and names a pool in, which is
	// also a namespace's.
	givenName = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

	// labelForm is $defs/label of the wire.
	labelForm = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*=[A-Za-z0-9]+(?:[._-][A-Za-z0-9]+)*$`)

	// variableName is what a line of runner.env may start with, so that a refusal repeats a
	// key only where it is one and never the start of a credential pasted on a line of its own.
	variableName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
)

// fileOnly are the keys read from runner.env alone: the identity join wrote.
var fileOnly = []string{RunnerID, RunnerPool, Credential}

// settings are the keys that may be written in the environment, in runner.env, or in both where
// the two agree.
var settings = []string{API, Labels, Concurrency, WorkDir, Namespaces}

// ReadConfig reads the runner's configuration from the environment and from the file join wrote.
//
// Each of the five settings may be written in either place, "in either runner form", which is how
// a unit's Environment= lines and the AGK_API join wrote live side by side. Where both carry one
// and the two disagree, the start is refused rather than one of them winning: an operator who
// moved the API in the unit and not in the file, or the reverse, meant one of the two, and a
// runner guessing which would reach the other with its credential. The identity is read from the
// file alone.
//
// Every setting that refuses the start is named on that start, and no refusal repeats a value that
// may hold a secret: a line of runner.env can be a credential, and so can whatever was pasted where
// a URL or an identifier belongs.
func ReadConfig(lookup Lookup, path string) (Config, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	r := &reader{lookup: lookup, path: path, file: map[string]string{}, written: map[string]bool{}}
	for _, name := range fileOnly {
		if _, set := r.env(name); !set {
			continue
		}
		if name == Credential {
			r.refuse(name, "is set in the environment, and a credential is never read from there: every process started from this one inherits it and anybody who can inspect it reads it. join writes it to "+path+", which serve reads itself")
			continue
		}
		r.refuse(name, "is set in the environment, and a runner's identity is read from "+path+" alone, where join wrote it beside the credential it belongs to, so that the two never come from two places")
	}
	joined := r.readFile()

	c := Config{
		API:         r.api(),
		Labels:      r.labels(),
		Concurrency: r.concurrency(),
		WorkDir:     r.workDir(),
		Namespaces:  r.namespaces(),
	}
	if joined {
		c.Runner = r.name(RunnerID, "the identifier the API minted for this runner")
		c.Pool = r.name(RunnerPool, "the pool this runner joined")
		c.Credential = r.credential()
	}
	return c, r.err()
}

// reader reads one start's settings and keeps every refusal.
type reader struct {
	lookup Lookup
	path   string
	file   map[string]string
	// written is every key a line of the file set, to nothing included, so that a second line
	// setting it is refused whatever the first one said.
	written map[string]bool
	refused []error
}

func (r *reader) refuse(name, reason string) {
	r.refused = append(r.refused, &Error{Variable: name, Reason: reason})
}

func (r *reader) err() error { return errors.Join(r.refused...) }

// env is a variable of the environment, and whether it is set. Set to nothing is unset: a unit or
// a Compose file writes an empty variable more often by accident than to mean anything.
func (r *reader) env(name string) (string, bool) {
	v, ok := r.lookup(name)
	return v, ok && v != ""
}

// value is a setting from wherever it is written, and whether it is. Written in both places, the
// two agree or the start is refused.
func (r *reader) value(name string) (string, bool) {
	fromEnv, inEnv := r.env(name)
	fromFile, inFile := r.file[name]
	switch {
	case inEnv && inFile && fromEnv != fromFile:
		r.refuse(name, "is set in the environment and in "+r.path+" to two different values, and a runner that chose one would be guessing which of them was meant: set it in one place, or to the same value in both")
		return "", false
	case inEnv:
		return fromEnv, true
	case inFile:
		return fromFile, true
	}
	return "", false
}

// readFile reads runner.env into r.file, and says whether there was one to read.
//
// The reading is strict, KEY=VALUE and nothing else, because this file is also read by whatever
// an operator points at it: a systemd EnvironmentFile= or a Compose env_file strips quotes and
// whitespace that this reader would keep, and a value two readers read differently is a
// credential that works in one and not the other. So a line either reads the same everywhere or
// is refused. Blank lines and lines beginning with # are skipped, which every reader agrees on.
func (r *reader) readFile() bool {
	path := r.path
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.refused = append(r.refused, ErrNotJoined)
		return false
	case err != nil:
		r.refuse(path, "cannot be read: "+reasonOf(err))
		return false
	case !info.Mode().IsRegular():
		r.refuse(path, "is not a file, and it is where join wrote this runner's credential")
		return false
	case info.Mode().Perm()&0o077 != 0:
		r.refuse(path, fmt.Sprintf("has mode %#o, and it holds this runner's credential, which is readable by the agent's account alone: chmod 600 it, because a credential anybody on the host can read is one anybody on the host has", info.Mode().Perm()))
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		r.refuse(path, "cannot be read: "+reasonOf(err))
		return false
	}
	defer f.Close()
	text, err := io.ReadAll(io.LimitReader(f, envFileMaxBytes+1))
	switch {
	case err != nil:
		r.refuse(path, "cannot be read: "+reasonOf(err))
		return false
	case len(text) > envFileMaxBytes:
		r.refuse(path, fmt.Sprintf("is more than %d bytes, and what join writes there is a few hundred", envFileMaxBytes))
		return false
	}

	known := map[string]bool{}
	for _, name := range settings {
		known[name] = true
	}
	for _, name := range fileOnly {
		known[name] = true
	}

	ok := true
	for i, line := range bytes.Split(text, []byte("\n")) {
		n := i + 1
		s := string(line)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		key, value, found := strings.Cut(s, "=")
		switch {
		case !found || !variableName.MatchString(key):
			// The line is not repeated, nor any part of it: it may be a credential
			// written on a line of its own.
			r.refuse(path, fmt.Sprintf("line %d is not KEY=VALUE, with the key in capitals and nothing around the =", n))
		case !known[key]:
			r.refuse(path, fmt.Sprintf("line %d sets %s, which this runner does not read: a misspelled setting is refused rather than left to its default without a word", n, key))
		case r.written[key]:
			r.refuse(path, fmt.Sprintf("line %d sets %s a second time, and a file whose readers could each take a different one of the two is refused", n, key))
		case strings.ContainsAny(value, "\r\x00"):
			r.refuse(path, fmt.Sprintf("line %d, setting %s, holds a carriage return or a NUL, which one reader keeps and another drops: write the file with plain line endings", n, key))
		case value != strings.TrimSpace(value):
			r.refuse(path, fmt.Sprintf("line %d, setting %s, has white space around its value, which one reader keeps and another strips", n, key))
		case strings.HasPrefix(value, `"`) || strings.HasPrefix(value, `'`):
			r.refuse(path, fmt.Sprintf("line %d, setting %s, is quoted, and this file is read as written: a quote one reader strips is a quote another keeps as part of the value", n, key))
		default:
			r.written[key] = true
			// Set to nothing is unset, in the file as in the environment.
			if value != "" {
				r.file[key] = value
			}
			continue
		}
		ok = false
	}
	return ok
}

// api is the address the API is reached at.
//
// https, and never plaintext across a network: every call carries this runner's credential, and a
// credential sent in the clear is one every hop on the way can use. A loopback address is the
// exception, since it crosses no network, and it is what a test installation on one host speaks.
// The URL is never repeated in a refusal, since what was pasted there may carry a password.
func (r *reader) api() string {
	v, set := r.value(API)
	if !set {
		if !r.conflicted(API) {
			r.refuse(API, "is not set, and it is the one address a runner is given: join writes it to "+r.path+", or the unit sets it")
		}
		return ""
	}
	// Looked for in the text rather than through a parser, since a password holding a slash
	// is where parsers part ways, and one that read no user would pass it on as a host.
	if _, rest, found := strings.Cut(v, "//"); found && strings.Contains(rest, "@") {
		r.refuse(API, "carries a user, and the address a runner is given carries no credential: the credential is "+Credential+", in "+r.path)
		return ""
	}
	u, err := url.Parse(v)
	switch {
	case err != nil || u.Host == "" || u.Opaque != "":
		r.refuse(API, "is not a URL with a host, such as https://agentiik.example.com")
		return ""
	case u.User != nil:
		r.refuse(API, "carries a user, and the address a runner is given carries no credential")
		return ""
	case strings.ContainsAny(v, "?#"):
		r.refuse(API, "carries a query or a fragment, even an empty one, and every call is a path below it")
		return ""
	case u.Scheme == "https":
	case u.Scheme == "http" && loopback(u.Hostname()):
	default:
		r.refuse(API, "is not an https URL, and every call to it carries this runner's credential, which is never sent in plaintext across a network: http is taken for a loopback address alone, which crosses none")
		return ""
	}
	return strings.TrimRight(v, "/")
}

// conflicted says whether a setting was already refused for being written two ways, so that it is
// not refused a second time as missing.
func (r *reader) conflicted(name string) bool {
	for _, err := range r.refused {
		var e *Error
		if errors.As(err, &e) && e.Variable == name {
			return true
		}
	}
	return false
}

// loopback says whether a host is this machine.
func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// labels are the labels this runner claims, key=value separated by commas.
func (r *reader) labels() []string {
	v, set := r.value(Labels)
	if !set {
		if !r.conflicted(Labels) {
			r.refuse(Labels, "is not set, and it is what a step's runs_on selects this runner on, claimed at join within what the token permits, such as zone=dmz,arch=amd64")
		}
		return nil
	}
	return r.list(Labels, v, labelForm, "a label is key=value, such as zone=dmz, the key lowercase words joined by dots, hyphens or underscores")
}

// namespaces is the narrower list one host accepts, or nil for every namespace its pool accepts.
func (r *reader) namespaces() []string {
	v, set := r.value(Namespaces)
	if !set {
		return nil
	}
	return r.list(Namespaces, v, givenName, "a namespace is lowercase words joined by hyphens, such as finance or team-ops")
}

// list reads a comma separated list, every item held to one grammar and none written twice.
func (r *reader) list(name, v string, form *regexp.Regexp, grammar string) []string {
	items := strings.Split(v, ",")
	seen := map[string]bool{}
	for i, item := range items {
		switch {
		case !form.MatchString(item):
			r.refuse(name, fmt.Sprintf("has %q as its item %d, and %s, separated by commas with no space", item, i+1, grammar))
			return nil
		case seen[item]:
			r.refuse(name, fmt.Sprintf("names %s twice", item))
			return nil
		}
		seen[item] = true
	}
	return items
}

// concurrency is how many tasks this host holds at once: one per vCPU where unset, which is where
// #sizing says to start.
func (r *reader) concurrency() int {
	v, set := r.value(Concurrency)
	if !set {
		return min(runtime.NumCPU(), MaxConcurrency)
	}
	n, err := strconv.Atoi(v)
	switch {
	case err != nil || n < 1:
		r.refuse(Concurrency, fmt.Sprintf("is %q, and it is how many tasks this host holds at once, a whole number of one or more: a runner holding none is one that takes nothing", v))
		return 0
	case n > MaxConcurrency:
		r.refuse(Concurrency, fmt.Sprintf("is %d, and a heartbeat names at most %d tasks, which is more containers than one host runs at once: a host holding more would have its heartbeat refused once it was full", n, MaxConcurrency))
		return 0
	}
	return n
}

// workDir is the work root, an absolute path.
func (r *reader) workDir() string {
	v, set := r.value(WorkDir)
	if !set {
		return DefaultWorkDir
	}
	if !filepath.IsAbs(v) {
		r.refuse(WorkDir, fmt.Sprintf("is %q, and it is an absolute path, so that where task directories and the record of keys are does not depend on the directory the agent was started from", v))
		return ""
	}
	return filepath.Clean(v)
}

// name is one half of the identity join wrote, in the grammar the API mints it in.
func (r *reader) name(key, what string) string {
	v, set := r.file[key]
	switch {
	case !set:
		r.refuse(key, "is not in "+r.path+", and it is "+what+", which join writes there: run agk-runner join again")
		return ""
	case !givenName.MatchString(v):
		r.refuse(key, fmt.Sprintf("in %s is not %s, which is lowercase words joined by hyphens as the API mints it", r.path, what))
		return ""
	}
	return v
}

// credential is the runner credential join wrote. It is never repeated, whatever is wrong with it.
func (r *reader) credential() Secret {
	v, set := r.file[Credential]
	if !set {
		r.refuse(Credential, "is not in "+r.path+", and it is what every call to the API carries, which join writes there: run agk-runner join again")
		return ""
	}
	kind, ok := token.KindOf(v)
	switch {
	case !ok || kind != token.Runner:
		r.refuse(Credential, "in "+r.path+" is not a runner credential, which is written agkrunner_ followed by its secret, as join wrote it")
		return ""
	case strings.ContainsFunc(v, func(c rune) bool {
		return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-')
	}):
		r.refuse(Credential, "in "+r.path+" holds a character a runner credential is never written with, which is base64url after its prefix")
		return ""
	}
	return Secret(v)
}

// reasonOf is what went wrong, without the path os repeats in its error.
func reasonOf(err error) string {
	var path *fs.PathError
	if errors.As(err, &path) {
		return path.Err.Error()
	}
	return err.Error()
}
