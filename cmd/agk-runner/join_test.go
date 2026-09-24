package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agentiik/agentiik/runner"
)

// aToken is a join token written the way token.New writes one.
const aToken = "agkjoin_lYd41PgjnpmnYTZ1KB2FvzKYJA7GgyKSqukAh9n7h9s"

// joiner is a host about to join an API that answers every join as the real one answers a good
// token, running as the account given and knowing the accounts given.
type joiner struct {
	e        env
	out, err *output
	api      string
	// joins is counted on the server's goroutine and read on the test's, which a socket
	// between them does not order for the race detector.
	joins atomic.Int32
}

func newJoiner(t *testing.T, euid int, accounts map[string]runner.Owner) *joiner {
	t.Helper()
	j := &joiner{out: &output{}, err: &output{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		j.joins.Add(1)
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"runner": "runner-dmz-02", "pool": "dmz", "credential": "`+credential+`", "rotate_by": "2026-12-09T06:12:00Z"}`)
	}))
	t.Cleanup(srv.Close)
	j.api = srv.URL

	dir := t.TempDir()
	vars := map[string]string{
		"DOCKER_HOST":        daemon(t, true).Socket(),
		"AGK_RUNNER_WORKDIR": filepath.Join(dir, "work"),
	}
	j.e = env{
		Out: j.out, Err: j.err,
		Lookup: func(name string) (string, bool) {
			v, ok := vars[name]
			return v, ok
		},
		Geteuid: func() int { return euid },
		EnvFile: filepath.Join(dir, "etc", "runner.env"),
		KeyFile: filepath.Join(dir, "var", "runner.key"),
		MemInfo: filepath.Join("..", "..", "runner", "testdata", "meminfo"),
		Account: func(name string) (runner.Owner, error) {
			o, ok := accounts[name]
			if !ok {
				return runner.Owner{}, errors.New("no account " + name)
			}
			return o, nil
		},
	}
	return j
}

func (j *joiner) join(t *testing.T, args ...string) int {
	t.Helper()
	return run(context.Background(), j.e, append([]string{"join", "--api", j.api, "--token", aToken, "--labels", "zone=dmz"}, args...))
}

// wrote says whether join left either file.
func (j *joiner) wrote() bool {
	for _, path := range []string{j.e.EnvFile, j.e.KeyFile} {
		if _, err := os.Lstat(path); err == nil {
			return true
		}
	}
	return false
}

// Run as root, join gives what it writes to the agent's account, agentiik unless --user names
// another, and says who the host now is.
func TestJoinAsRootGivesItsFilesToTheAgentsAccount(t *testing.T) {
	// The account running the test stands for agentiik, since it is the one a test can give a
	// file to without being root.
	self := runner.Owner{UID: os.Getuid(), GID: os.Getgid()}
	j := newJoiner(t, 0, map[string]runner.Owner{"agentiik": self})
	if code := j.join(t); code != exitSucceeded {
		t.Fatalf("join exited %d:\n%s", code, j.err)
	}
	for _, want := range []string{"runner-dmz-02", "pool dmz", j.e.KeyFile, j.e.EnvFile, "account agentiik", "2026-12-09T06:12:00Z"} {
		if !strings.Contains(j.out.String(), want) {
			t.Errorf("join did not say %q:\n%s", want, j.out)
		}
	}
	if strings.Contains(j.out.String()+j.err.String(), credential) || strings.Contains(j.out.String()+j.err.String(), aToken) {
		t.Error("join printed a credential")
	}
	for _, path := range []string{j.e.EnvFile, j.e.KeyFile} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s is %v (%v)", path, info.Mode(), err)
		}
	}
}

func TestJoinAsRootRefusesAnAccountItCannotGiveItsFilesTo(t *testing.T) {
	for name, c := range map[string]struct {
		args []string
		says string
	}{
		"root, which serve refuses to run as":  {[]string{"--user", "root"}, "serve refuses to run as root"},
		"an account the host does not have":    {[]string{"--user", "nobody-here"}, "nobody-here"},
		"agentiik, where the host has no such": {nil, "agentiik"},
	} {
		t.Run(name, func(t *testing.T) {
			j := newJoiner(t, 0, map[string]runner.Owner{"root": {UID: 0, GID: 0}})
			if code := j.join(t, c.args...); code != exitRefused {
				t.Fatalf("join exited %d:\n%s", code, j.err)
			}
			if !strings.Contains(j.err.String(), c.says) {
				t.Errorf("the refusal does not say %q:\n%s", c.says, j.err)
			}
			if j.joins.Load() != 0 || j.wrote() {
				t.Errorf("a refused join reached the API %d times, or wrote something", j.joins.Load())
			}
		})
	}
}

// Run as anybody but root, join writes as itself, and --user naming another account is refused,
// since only root can give a file away.
func TestJoinAsAnotherAccountWritesAsItselfAlone(t *testing.T) {
	j := newJoiner(t, 1000, map[string]runner.Owner{"agentiik": {UID: 999, GID: 999}, "me": {UID: 1000, GID: 1000}})
	if code := j.join(t, "--user", "agentiik"); code != exitRefused {
		t.Fatalf("join exited %d:\n%s", code, j.err)
	}
	if !strings.Contains(j.err.String(), "run join as root") || j.joins.Load() != 0 || j.wrote() {
		t.Errorf("join refused, and said:\n%s", j.err)
	}

	if code := j.join(t, "--user", "me"); code != exitSucceeded {
		t.Fatalf("join as the account it names exited %d:\n%s", code, j.err)
	}
}

func TestJoinRefusesAHostThatHasJoinedUnlessToldToReplaceIt(t *testing.T) {
	j := newJoiner(t, 1000, nil)
	if code := j.join(t); code != exitSucceeded {
		t.Fatalf("join exited %d:\n%s", code, j.err)
	}
	if code := j.join(t); code != exitRefused || !strings.Contains(j.err.String(), "--replace") {
		t.Errorf("joining again exited %d:\n%s", code, j.err)
	}
	if j.joins.Load() != 1 {
		t.Errorf("joining again reached the API, %d joins in all", j.joins.Load())
	}
	if code := j.join(t, "--replace"); code != exitSucceeded {
		t.Errorf("joining again with --replace exited %d:\n%s", code, j.err)
	}
}

func TestJoinTakesFlagsAlone(t *testing.T) {
	j := newJoiner(t, 1000, nil)
	if code := j.join(t, aToken); code != exitUsage {
		t.Errorf("join given a token as an argument exited %d", code)
	}
	if code := run(context.Background(), j.e, []string{"join", "--label", "zone=dmz"}); code != exitUsage {
		t.Errorf("join given a flag it does not know exited %d", code)
	}
	if code := run(context.Background(), j.e, []string{"join", "-h"}); code != exitSucceeded {
		t.Errorf("join -h exited %d", code)
	}
	if j.joins.Load() != 0 || j.wrote() {
		t.Error("a join refused on its command line reached the API or wrote something")
	}
}
