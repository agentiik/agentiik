package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// credential is a runner credential written the way token.New writes one.
const credential = "agkrunner_Xy9QkZ3v0bq8LrT2mN5pW7sD1fG4hJ6kA9cE0uI3oY2"

// joined is runner.env as join writes it.
var joined = strings.Join([]string{
	"# written by agk-runner join",
	"AGK_API=https://agentiik.example.com",
	"AGK_RUNNER_ID=runner-dmz-02",
	"AGK_RUNNER_POOL=dmz",
	"AGK_RUNNER_LABELS=zone=dmz,arch=amd64",
	"AGK_RUNNER_CREDENTIAL=" + credential,
	"",
}, "\n")

// envFile writes a runner.env of the given text and mode, and answers its path.
func envFile(t *testing.T, text string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner.env")
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile's mode passes through the umask, and the mode is what is being tested.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// environment is a Lookup over a map, so that no test reads or writes the process's own.
func environment(vars map[string]string) Lookup {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

// refusedFor answers the variables a refusal names, one per setting refused.
func refusedFor(err error) []string {
	var names []string
	for _, e := range unjoin(err) {
		var r *Error
		if errors.As(e, &r) {
			names = append(names, r.Variable)
		} else if errors.Is(e, ErrNotJoined) {
			names = append(names, "not joined")
		}
	}
	return names
}

func unjoin(err error) []error {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		return j.Unwrap()
	}
	if err == nil {
		return nil
	}
	return []error{err}
}

func TestAJoinedHostReadsItsSettingsAndIdentityFromRunnerEnv(t *testing.T) {
	c, err := ReadConfig(environment(nil), envFile(t, joined, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		API:         "https://agentiik.example.com",
		Labels:      []string{"zone=dmz", "arch=amd64"},
		Concurrency: runtime.NumCPU(),
		WorkDir:     DefaultWorkDir,
		Runner:      "runner-dmz-02",
		Pool:        "dmz",
		Credential:  credential,
	}
	if fmt.Sprintf("%#v", c) != fmt.Sprintf("%#v", want) || c.Credential != want.Credential {
		t.Errorf("read %#v, want %#v", c, want)
	}
	if c.Namespaces != nil {
		t.Errorf("namespaces are %q where AGK_RUNNER_NAMESPACES is unset, and nil is every namespace the pool accepts", c.Namespaces)
	}
}

func TestTheEnvironmentCarriesTheSettingsTheUnitWrites(t *testing.T) {
	c, err := ReadConfig(environment(map[string]string{
		"AGK_RUNNER_CONCURRENCY": "8",
		"AGK_RUNNER_WORKDIR":     "/srv/agentiik/work/",
		"AGK_RUNNER_NAMESPACES":  "finance,team-ops",
		// The same value in both places is one value.
		"AGK_API": "https://agentiik.example.com",
	}), envFile(t, joined, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if c.Concurrency != 8 || c.WorkDir != "/srv/agentiik/work" || !slices.Equal(c.Namespaces, []string{"finance", "team-ops"}) {
		t.Errorf("read concurrency %d, work root %s and namespaces %q", c.Concurrency, c.WorkDir, c.Namespaces)
	}
}

func TestASettingSetToNothingIsUnset(t *testing.T) {
	c, err := ReadConfig(environment(map[string]string{"AGK_RUNNER_CONCURRENCY": ""}),
		envFile(t, joined+"AGK_RUNNER_NAMESPACES=\n", 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if c.Concurrency != runtime.NumCPU() || c.Namespaces != nil {
		t.Errorf("read concurrency %d and namespaces %q, and both are unset", c.Concurrency, c.Namespaces)
	}
}

// A runner of the pool default joined claiming no label, and runner.env carries none: serve reads
// it as claiming none rather than refusing the start for a setting nobody had to write.
func TestARunnerEnvWithNoLabelsClaimsNone(t *testing.T) {
	text := strings.Replace(joined, "AGK_RUNNER_LABELS=zone=dmz,arch=amd64\n", "", 1)
	text = strings.Replace(text, "AGK_RUNNER_POOL=dmz", "AGK_RUNNER_POOL=default", 1)
	c, err := ReadConfig(environment(nil), envFile(t, text, 0o600))
	if err != nil {
		t.Fatalf("serve refuses a runner that claims no label: %s", err)
	}
	if c.Labels != nil || c.Pool != "default" {
		t.Errorf("read the labels %q in pool %s, and it claims none in default", c.Labels, c.Pool)
	}
}

// Labels are claimed at join, within what the token permits: a runner that joined claiming none is
// not given labels afterwards by its environment, which would take work of its pool the API never
// let it claim.
func TestLabelsInTheEnvironmentOfARunnerThatJoinedClaimingNoneAreRefused(t *testing.T) {
	text := strings.Replace(joined, "AGK_RUNNER_LABELS=zone=dmz,arch=amd64\n", "", 1)
	_, err := ReadConfig(environment(map[string]string{Labels: "zone=dmz"}), envFile(t, text, 0o600))
	if got := refusedFor(err); !slices.Equal(got, []string{Labels}) {
		t.Errorf("refused %q (%v), want %s alone", got, err, Labels)
	}
	// Set to nothing, it is unset, and claims nothing either.
	if _, err := ReadConfig(environment(map[string]string{Labels: ""}), envFile(t, text, 0o600)); err != nil {
		t.Errorf("an empty %s refused the start: %s", Labels, err)
	}
}

func TestAHostThatHasNotJoinedIsToldToJoin(t *testing.T) {
	_, err := ReadConfig(environment(map[string]string{"AGK_API": "https://agentiik.example.com", "AGK_RUNNER_LABELS": "zone=dmz"}),
		filepath.Join(t.TempDir(), "runner.env"))
	if !errors.Is(err, ErrNotJoined) {
		t.Fatalf("a missing runner.env gave %v, want ErrNotJoined", err)
	}
	if !strings.Contains(err.Error(), "agk-runner join") {
		t.Errorf("the refusal does not say how to join: %s", err)
	}
}

func TestACredentialFileAnybodyElseCanReadIsRefused(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660} {
		path := envFile(t, joined, mode)
		_, err := ReadConfig(environment(nil), path)
		// The file alone: AGK_API and the labels are written in it, and refusing them
		// as missing too would send an operator after two settings that are there.
		if !slices.Equal(refusedFor(err), []string{path}) {
			t.Errorf("runner.env of mode %#o gave %v, and a credential its group or anybody else can read is refused", mode, err)
		}
		if err != nil && strings.Contains(err.Error(), credential) {
			t.Errorf("the refusal repeats the credential: %s", err)
		}
	}
}

func TestSomethingOtherThanAFileIsRefusedAsRunnerEnv(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadConfig(environment(nil), dir)
	if !slices.Contains(refusedFor(err), dir) {
		t.Errorf("a directory as runner.env gave %v", err)
	}
}

func TestRunnerEnvIsReadStrictlyAndNoLineOfItIsRepeated(t *testing.T) {
	for _, c := range []struct{ name, line, says string }{
		{"a line with no =", credential, "is not KEY=VALUE"},
		{"a key in lowercase", "agk_api=https://agentiik.example.com", "is not KEY=VALUE"},
		{"space before the =", "AGK_RUNNER_CONCURRENCY =4", "is not KEY=VALUE"},
		{"export in front", "export AGK_RUNNER_CONCURRENCY=4", "is not KEY=VALUE"},
		{"a key it does not read", "AGK_RUNNER_CONCURENCY=4", "does not read"},
		{"a key set twice", "AGK_RUNNER_POOL=edge", "a second time"},
		{"a quoted value", `AGK_RUNNER_NAMESPACES="finance"`, "is quoted"},
		{"space around a value", "AGK_RUNNER_NAMESPACES= finance", "white space"},
		{"a carriage return", "AGK_RUNNER_NAMESPACES=finance\r", "carriage return"},
		{"a NUL", "AGK_RUNNER_NAMESPACES=fin\x00ance", "NUL"},
		{"a backslash joining the next line", "AGK_RUNNER_WORKDIR=/srv/work\\", "backslash"},
		{"a substitution", "AGK_RUNNER_WORKDIR=/srv/$HOME", "substitution"},
		{"a backquote", "AGK_RUNNER_WORKDIR=/srv/`id`", "backquote"},
		{"a comment after the value", "AGK_RUNNER_WORKDIR=/srv/work #fast disk", "comment"},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := envFile(t, joined+c.line+"\n", 0o600)
			_, err := ReadConfig(environment(nil), path)
			if err == nil {
				t.Fatalf("%q was read", c.line)
			}
			if !strings.Contains(err.Error(), "line 7") || !strings.Contains(err.Error(), c.says) {
				t.Errorf("the refusal does not name line 7 and say %q: %s", c.says, err)
			}
			if strings.Contains(err.Error(), credential) {
				t.Errorf("the refusal repeats the credential: %s", err)
			}
			// A file with a line refused is not read for an identity, so the refusal
			// is about the line and not also about three keys it then found missing.
			if got := refusedFor(err); !slices.Equal(got, []string{path}) {
				t.Errorf("refused %q, want the file alone", got)
			}
		})
	}
}

func TestTheIdentityIsNeverReadFromTheEnvironment(t *testing.T) {
	for _, name := range []string{Credential, RunnerID, RunnerPool} {
		value := map[string]string{Credential: credential, RunnerID: "runner-dmz-02", RunnerPool: "dmz"}[name]
		_, err := ReadConfig(environment(map[string]string{name: value}), envFile(t, joined, 0o600))
		if !slices.Contains(refusedFor(err), name) {
			t.Errorf("%s in the environment gave %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), credential) {
			t.Errorf("the refusal repeats the credential: %s", err)
		}
	}
}

func TestASettingWrittenTwoWaysIsRefusedRatherThanGuessed(t *testing.T) {
	_, err := ReadConfig(environment(map[string]string{
		"AGK_API":           "https://agentiik.example.org",
		"AGK_RUNNER_LABELS": "zone=lan",
	}), envFile(t, joined, 0o600))
	got := refusedFor(err)
	if !slices.Equal(got, []string{API, Labels}) {
		t.Fatalf("refused %q, want AGK_API and AGK_RUNNER_LABELS once each: %v", got, err)
	}
	if strings.Contains(err.Error(), "example.org") || strings.Contains(err.Error(), "example.com") {
		t.Errorf("the refusal repeats a URL: %s", err)
	}
}

func TestTheAPIIsReachedOverTLSOrOnThisMachine(t *testing.T) {
	for _, c := range []struct {
		api, want string
		refused   bool
	}{
		{api: "https://agentiik.example.com/", want: "https://agentiik.example.com"},
		{api: "https://agentiik.example.com/agk//", want: "https://agentiik.example.com/agk"},
		{api: "http://127.0.0.1:8080", want: "http://127.0.0.1:8080"},
		{api: "http://[::1]:8080", want: "http://[::1]:8080"},
		{api: "http://localhost:8080", want: "http://localhost:8080"},
		{api: "http://agentiik.example.com", refused: true},
		{api: "http://10.0.0.1:8080", refused: true},
		{api: "ftp://agentiik.example.com", refused: true},
		{api: "agentiik.example.com", refused: true},
		{api: "https://", refused: true},
		{api: "https://agentiik.example.com?", refused: true},
		{api: "https://agentiik.example.com/#", refused: true},
		{api: "https://runner:Xy9/Qk@agentiik.example.com", refused: true},
		// Parsed, this is the host runner:1234 and no user at all, so only a reading of
		// the text finds the password.
		{api: "https://runner:1234/Xy9@agentiik.example.com", refused: true},
	} {
		text := strings.Replace(joined, "AGK_API=https://agentiik.example.com\n", "", 1)
		cfg, err := ReadConfig(environment(map[string]string{"AGK_API": c.api}), envFile(t, text, 0o600))
		switch {
		case c.refused && !slices.Equal(refusedFor(err), []string{API}):
			t.Errorf("%s gave %v, and it is refused", c.api, err)
		case c.refused && strings.Contains(err.Error(), "Xy9"):
			t.Errorf("the refusal repeats the password: %s", err)
		case !c.refused && err != nil:
			t.Errorf("%s was refused: %v", c.api, err)
		case !c.refused && cfg.API != c.want:
			t.Errorf("%s read as %s, want %s", c.api, cfg.API, c.want)
		}
	}
}

func TestEverySettingIsHeldToItsGrammar(t *testing.T) {
	for _, c := range []struct{ name, value string }{
		{Labels, "zone=dmz, arch=amd64"},
		{Labels, "zone"},
		{Labels, "zone=dmz,zone=dmz"},
		{Labels, "Zone=dmz"},
		{Namespaces, "finance,"},
		{Namespaces, "Finance"},
		{Namespaces, "finance,finance"},
		{Concurrency, "0"},
		{Concurrency, "-2"},
		{Concurrency, "eight"},
		{Concurrency, "4097"},
		{WorkDir, "work"},
		{WorkDir, "./var/lib/agentiik/work"},
	} {
		// Labels are written in runner.env, where join writes them, since a runner that joined
		// claiming none is refused labels from the environment whatever they are.
		text, vars := joined, map[string]string{c.name: c.value}
		if c.name == Labels {
			text, vars = strings.Replace(joined, "zone=dmz,arch=amd64", c.value, 1), nil
		}
		_, err := ReadConfig(environment(vars), envFile(t, text, 0o600))
		if !slices.Equal(refusedFor(err), []string{c.name}) {
			t.Errorf("%s=%q gave %v", c.name, c.value, err)
		}
	}
}

func TestConcurrencyStopsWhereAHeartbeatStops(t *testing.T) {
	c, err := ReadConfig(environment(map[string]string{Concurrency: "4096"}), envFile(t, joined, 0o600))
	if err != nil || c.Concurrency != MaxConcurrency {
		t.Errorf("4096 read as %d, %v", c.Concurrency, err)
	}
}

func TestTheRequiredSettingsAreNamedWhenMissing(t *testing.T) {
	_, err := ReadConfig(environment(nil), envFile(t, "# nothing yet\n", 0o600))
	got := refusedFor(err)
	want := []string{API, RunnerID, RunnerPool, Credential}
	if !slices.Equal(got, want) {
		t.Errorf("refused %q, want %q named on the one start", got, want)
	}
}

func TestAnIdentityJoinCouldNotHaveWrittenIsRefused(t *testing.T) {
	for _, c := range []struct{ name, from, to string }{
		{Credential, credential, "agkjoin_Xy9QkZ3v0bq8LrT2mN5pW7sD1fG4hJ6kA9cE0uI3oY2"},
		{Credential, credential, "agkrunner_short"},
		{Credential, credential, "agkrunner_Xy9QkZ3v0bq8LrT2mN5pW7sD1fG4hJ6kA9cE0uI3o+/="},
		{RunnerID, "AGK_RUNNER_ID=runner-dmz-02", "AGK_RUNNER_ID=Runner_02"},
		{RunnerPool, "AGK_RUNNER_POOL=dmz", "AGK_RUNNER_POOL=dmz_pool"},
	} {
		_, err := ReadConfig(environment(nil), envFile(t, strings.Replace(joined, c.from, c.to, 1), 0o600))
		if !slices.Equal(refusedFor(err), []string{c.name}) {
			t.Errorf("%s written %q gave %v", c.name, c.to, err)
			continue
		}
		written := c.to
		// A line of runner.env is KEY=VALUE, and the value is what must not be repeated.
		// A credential is not a line, and its base64 may end in a = of its own.
		if key, value, found := strings.Cut(c.to, "="); found && strings.HasPrefix(key, "AGK_") {
			written = value
		}
		if strings.Contains(err.Error(), written) {
			t.Errorf("the refusal repeats what was written: %s", err)
		}
	}
}

func TestACredentialIsNeverPrinted(t *testing.T) {
	c, err := ReadConfig(environment(nil), envFile(t, joined, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	printed := []string{
		fmt.Sprint(c), fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c),
		fmt.Sprintf("%s", c.Credential), c.Credential.String(), string(encoded),
	}
	// A verb that is wrong for a string, which a line with its arguments out of order
	// gives, prints the credential too unless it is told not to.
	for _, verb := range []string{"%d", "%x", "%X", "%q", "%t", "%c", "%10.3f"} {
		for _, v := range []any{c, &c, c.Credential, []Secret{c.Credential}, map[string]Secret{"k": c.Credential}} {
			printed = append(printed, fmt.Sprintf(verb, v))
		}
	}
	for _, printed := range printed {
		if strings.Contains(printed, credential[len("agkrunner_"):]) {
			t.Errorf("the credential is printed: %s", printed)
		}
		if !strings.Contains(printed, "agkrunner_[redacted]") {
			t.Errorf("what the credential is is not printed either: %s", printed)
		}
	}
}

func TestNoRefusalRepeatsWhatWasPastedWhereASettingBelongs(t *testing.T) {
	for _, name := range []string{Labels, Namespaces, Concurrency, WorkDir} {
		text := strings.Replace(joined, "AGK_RUNNER_LABELS=zone=dmz,arch=amd64\n", "", 1)
		if name != Labels {
			text += "AGK_RUNNER_LABELS=zone=dmz\n"
		}
		for _, pasted := range []string{credential, "zone=dmz," + credential} {
			_, err := ReadConfig(environment(nil), envFile(t, text+name+"="+pasted+"\n", 0o600))
			if err == nil {
				t.Errorf("%s=%s was read", name, pasted)
				continue
			}
			if strings.Contains(err.Error(), credential[len("agkrunner_"):]) {
				t.Errorf("the refusal of %s repeats the credential pasted there: %s", name, err)
			}
		}
	}
}

func TestASymbolicLinkIsRefusedAsRunnerEnv(t *testing.T) {
	target := envFile(t, joined, 0o600)
	link := filepath.Join(t.TempDir(), "runner.env")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, err := ReadConfig(environment(nil), link)
	if !slices.Contains(refusedFor(err), link) || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("a link to runner.env gave %v", err)
	}
}

func TestRunnerEnvOwnedBySomebodyElseIsRefused(t *testing.T) {
	path := envFile(t, joined, 0o600)
	if _, err := ReadConfig(environment(nil), path); err != nil {
		t.Fatalf("runner.env owned by the account reading it was refused: %v", err)
	}
	// A test cannot give a file away without being root, so it is the agent that is
	// somebody else.
	defer func(was func() int) { geteuid = was }(geteuid)
	geteuid = func() int { return os.Geteuid() + 1 }
	_, err := ReadConfig(environment(nil), path)
	if !slices.Contains(refusedFor(err), path) || !strings.Contains(err.Error(), "is owned by account") {
		t.Errorf("runner.env owned by another account gave %v", err)
	}
}

func TestRunnerEnvLargerThanJoinWritesIsRefused(t *testing.T) {
	path := envFile(t, joined+strings.Repeat("# padding\n", 7000), 0o600)
	_, err := ReadConfig(environment(nil), path)
	if !slices.Equal(refusedFor(err), []string{path}) || !strings.Contains(err.Error(), "more than") {
		t.Errorf("a runner.env of more than 64 KiB gave %v", err)
	}
}
