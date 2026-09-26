package runner

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// lookupOf is an environment holding vars and nothing else.
func lookupOf(vars map[string]string) Lookup {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

// aTokenFile is a join token in a file of mode, as init writes one at 0600.
func aTokenFile(t *testing.T, text string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "join-token")
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAJoinTokenIsReadFromItsValueOrItsFile(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"a value":                {JoinToken: string(aToken)},
		"a file":                 {JoinTokenFile: aTokenFile(t, string(aToken)+"\n", 0o600)},
		"a file and no value":    {JoinToken: "", JoinTokenFile: aTokenFile(t, string(aToken), 0o400)},
		"a value and no file":    {JoinToken: string(aToken), JoinTokenFile: ""},
		"neither, which is none": {},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ReadJoinToken(lookupOf(vars))
			want := aToken
			if len(vars) == 0 {
				want = ""
			}
			if err != nil || got != want {
				t.Errorf("the join token read is %q, %v", got, err)
			}
		})
	}
}

// A join token that cannot be used is refused naming the variable it came from, and neither the
// token nor the path is repeated.
func TestAJoinTokenThatCannotBeUsedIsRefusedNamingItsVariable(t *testing.T) {
	for name, c := range map[string]struct {
		vars map[string]string
		want string
	}{
		"both":               {map[string]string{JoinToken: string(aToken), JoinTokenFile: aTokenFile(t, string(aToken), 0o600)}, JoinToken + " and " + JoinTokenFile + " are both set"},
		"a relative path":    {map[string]string{JoinTokenFile: "join-token"}, JoinTokenFile + " is not an absolute path"},
		"no file":            {map[string]string{JoinTokenFile: filepath.Join(t.TempDir(), "gone")}, JoinTokenFile + " names a file that cannot be read"},
		"a directory":        {map[string]string{JoinTokenFile: t.TempDir()}, JoinTokenFile + " names something that is not a file"},
		"a file others read": {map[string]string{JoinTokenFile: aTokenFile(t, string(aToken), 0o640)}, JoinTokenFile + " names a file of mode 0640"},
		"an empty file":      {map[string]string{JoinTokenFile: aTokenFile(t, "\n", 0o600)}, JoinTokenFile + " names an empty file"},
		"a credential":       {map[string]string{JoinToken: credential}, JoinToken + " is not a join token"},
		"a stray character":  {map[string]string{JoinTokenFile: aTokenFile(t, string(aToken)+"!", 0o600)}, JoinTokenFile + " is not a join token"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ReadJoinToken(lookupOf(c.vars))
			if err == nil || got != "" {
				t.Fatalf("read %q, %v, want a refusal", got, err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal does not say %q: %v", c.want, err)
			}
			for _, secret := range []string{string(aToken)[len("agkjoin_"):], credential[len("agkrunner_"):], c.vars[JoinTokenFile]} {
				if secret != "" && strings.Contains(err.Error(), secret) {
					t.Errorf("the refusal repeats %q: %v", secret, err)
				}
			}
		})
	}
}

// Drifted compares what the environment claims with what runner.env holds, the environment taken
// whole: what it leaves unset, it claims none of.
func TestDriftedNamesWhatTheEnvironmentClaimsOtherwiseThanRunnerEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.env")
	joined := "AGK_API=https://agentiik.example.com\nAGK_RUNNER_ID=runner-dmz-02\nAGK_RUNNER_POOL=dmz\nAGK_RUNNER_LABELS=zone=dmz\nAGK_RUNNER_CREDENTIAL=" + credential + "\n"
	if err := os.WriteFile(path, []byte(joined), 0o600); err != nil {
		t.Fatal(err)
	}
	same := map[string]string{API: "https://agentiik.example.com/", Labels: "zone=dmz"}
	for name, c := range map[string]struct {
		change map[string]string
		want   []string
	}{
		"nothing":            {nil, nil},
		"the address":        {map[string]string{API: "https://elsewhere.example.com"}, []string{API}},
		"the labels":         {map[string]string{Labels: "zone=lab"}, []string{Labels}},
		"labels left unset":  {map[string]string{Labels: ""}, []string{Labels}},
		"namespaces now set": {map[string]string{Namespaces: "finance"}, []string{Namespaces}},
	} {
		t.Run(name, func(t *testing.T) {
			vars := map[string]string{}
			for k, v := range same {
				vars[k] = v
			}
			for k, v := range c.change {
				vars[k] = v
			}
			got, err := Drifted(lookupOf(vars), path)
			if err != nil || !slices.Equal(got, c.want) {
				t.Errorf("drifted %v, %v, want %v", got, err, c.want)
			}
		})
	}
}
