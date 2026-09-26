package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/runner"
)

// unjoined takes the host's identity away, as a runner that never joined has none, and gives serve
// the API's address and a join token in a file, as Compose gives them.
func (h *host) unjoined(t *testing.T) {
	t.Helper()
	for _, path := range []string{h.e.EnvFile, h.e.KeyFile} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	h.set(runner.API, h.api)
	h.withTokenFile(t)
}

// withTokenFile gives serve a join token in a file, as init writes the local runner's.
func (h *host) withTokenFile(t *testing.T) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "join-token")
	if err := os.WriteFile(file, []byte(aToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.set(runner.JoinTokenFile, file)
}

// patience makes serve wait for an API that does not answer its join for d rather than minutes.
func patience(t *testing.T, d time.Duration) {
	was := joinPatience
	joinPatience = d
	t.Cleanup(func() { joinPatience = was })
}

// Given a join token and no identity, serve joins, writing what join writes, then serves as the
// runner it joined as.
func TestServeGivenAJoinTokenAndNoIdentityJoinsThenServes(t *testing.T) {
	h := newHost(t, daemon(t, true), secretsTmpfs)
	h.unjoined(t)
	h.set(runner.Labels, "zone=dmz")
	h.serving(t)
	if n := h.joins.Load(); n != 1 {
		t.Errorf("serve joined %d times, want once", n)
	}
	for _, path := range []string{h.e.EnvFile, h.e.KeyFile} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s is not what join writes: %v", path, err)
		}
	}
	for _, want := range []string{"joined pool dmz as runner runner-dmz-03, claiming the labels zone=dmz", runner.JoinTokenFile, "serving as runner-dmz-03"} {
		if !strings.Contains(h.err.String(), want) {
			t.Errorf("the agent's log does not say %q:\n%s", want, h.err)
		}
	}
	if strings.Contains(h.err.String(), aToken[len("agkjoin_"):]) {
		t.Errorf("the agent's log carries the join token:\n%s", h.err)
	}
}

// A join token given as a value joins as one in a file does, which is how a runner on another
// machine is given one with nothing written on it first.
func TestServeJoinsWithAJoinTokenGivenAsAValue(t *testing.T) {
	h := newHost(t, daemon(t, true), secretsTmpfs)
	h.unjoined(t)
	h.set(runner.JoinTokenFile, "")
	h.set(runner.JoinToken, aToken)
	h.serving(t)
	if n := h.joins.Load(); n != 1 {
		t.Errorf("serve joined %d times, want once", n)
	}
}

// A host that has joined, whose environment claims what it joined with, keeps its identity and
// spends nothing, whatever join token it is given.
func TestServeWithAnIdentityItsEnvironmentAgreesWithDoesNotJoin(t *testing.T) {
	h := newHost(t, daemon(t, true), secretsTmpfs)
	h.set(runner.API, h.api)
	h.set(runner.Labels, "zone=dmz,arch=amd64")
	h.withTokenFile(t)
	h.serving(t)
	if n := h.joins.Load(); n != 0 {
		t.Errorf("a host that has joined joined %d more times", n)
	}
	for _, want := range []string{"is not used", "serving as runner-dmz-02"} {
		if !strings.Contains(h.err.String(), want) {
			t.Errorf("the agent's log does not say %q:\n%s", want, h.err)
		}
	}
}

// No setting is read only at the first start: a runner whose environment claims other labels than
// it joined with joins again with its join token, and serves as the runner that claims them.
func TestServeJoinsAgainWhenItsEnvironmentClaimsOtherwiseThanItJoinedWith(t *testing.T) {
	h := newHost(t, daemon(t, true), secretsTmpfs)
	key, err := os.ReadFile(h.e.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	h.set(runner.API, h.api)
	h.set(runner.Labels, "zone=lab")
	h.withTokenFile(t)
	h.serving(t)
	if n := h.joins.Load(); n != 1 {
		t.Fatalf("serve joined %d times, want once:\n%s", n, h.err)
	}
	if claimed, _ := h.claimed.Load().(string); !strings.Contains(claimed, `"labels":["zone=lab"]`) {
		t.Errorf("the join claimed %s, want the labels the environment claims", claimed)
	}
	text, err := os.ReadFile(h.e.EnvFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), "AGK_RUNNER_LABELS=zone=lab\n") || !strings.Contains(string(text), "AGK_RUNNER_ID=runner-dmz-03\n") {
		t.Errorf("runner.env is not the new runner's:\n%s", text)
	}
	if now, _ := os.ReadFile(h.e.KeyFile); string(now) == string(key) {
		t.Error("the runner joined again with the key it had, and a new runner has a new key")
	}
	for _, want := range []string{runner.Labels + " in the environment says otherwise", "serving as runner-dmz-03"} {
		if !strings.Contains(h.err.String(), want) {
			t.Errorf("the agent's log does not say %q:\n%s", want, h.err)
		}
	}

	// Joined again, it agrees with its environment, and the next start joins no more.
	h.err = &output{}
	h.e.Err = h.err
	h.serving(t)
	if n := h.joins.Load(); n != 1 {
		t.Errorf("a runner that joined again joined %d times in all, want once", n)
	}
}

// With no join token, a runner whose environment names another API than it joined with refuses to
// serve with the address it joined with, and says a join token would join it again.
func TestServeWhoseEnvironmentMovedOnWithoutAJoinTokenRefusesAndSaysWhatWouldJoinIt(t *testing.T) {
	h := newHost(t, daemon(t, true), secretsTmpfs)
	h.set(runner.API, "https://elsewhere.example.com")
	said := h.refused(t)
	for _, want := range []string{runner.API, runner.JoinToken, runner.JoinTokenFile} {
		if !strings.Contains(said, want) {
			t.Errorf("the refusal does not name %s:\n%s", want, said)
		}
	}
}

// A host that never joined and has no join token is told both ways to join.
func TestServeWithNoIdentityAndNoJoinTokenSaysHowToJoin(t *testing.T) {
	h := newHost(t, daemon(t, true), secretsTmpfs)
	h.unjoined(t)
	h.set(runner.JoinTokenFile, "")
	said := h.refused(t)
	for _, want := range []string{"agk-runner join", runner.JoinToken} {
		if !strings.Contains(said, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, said)
		}
	}
}

// An API that is not up yet is waited for, and the join asked again.
func TestServeWaitsForAnAPIThatDoesNotAnswerItsJoinYet(t *testing.T) {
	h := newHost(t, daemon(t, true), secretsTmpfs)
	h.unjoined(t)
	h.unanswered.Store(1)
	h.serving(t)
	if n := h.joins.Load(); n != 2 {
		t.Errorf("serve joined %d times, want twice", n)
	}
	if !strings.Contains(h.err.String(), "asked again in 1s") {
		t.Errorf("the agent's log does not say it asks again:\n%s", h.err)
	}
}

// An API that never answers is waited for a bounded time, and the start then refused, saying where
// to look.
func TestServeGivesUpOnAnAPIThatNeverAnswersItsJoin(t *testing.T) {
	patience(t, 1500*time.Millisecond)
	h := newHost(t, daemon(t, true), secretsTmpfs)
	h.unjoined(t)
	h.unanswered.Store(1000)
	if code := run(context.Background(), h.e, []string{"serve"}); code != exitRefused {
		t.Fatalf("serve exited %d, want %d:\n%s", code, exitRefused, h.err)
	}
	if n := h.joins.Load(); n < 2 {
		t.Errorf("serve asked %d times, and it asks again while it waits", n)
	}
	for _, want := range []string{"has not answered a join for 1.5s", runner.API} {
		if !strings.Contains(h.err.String(), want) {
			t.Errorf("the refusal does not say %q:\n%s", want, h.err)
		}
	}
	if _, err := os.Stat(h.e.EnvFile); err == nil {
		t.Error("a join nobody answered left a runner.env")
	}
}

// A join token the API refuses is refused on the first try: asking again gets the same answer.
func TestAJoinTokenTheAPIRefusesRefusesTheStartAtOnce(t *testing.T) {
	h := newHost(t, daemon(t, true), secretsTmpfs)
	h.unjoined(t)
	h.joinStatus.Store(401)
	if code := run(context.Background(), h.e, []string{"serve"}); code != exitRefused {
		t.Fatalf("serve exited %d, want %d:\n%s", code, exitRefused, h.err)
	}
	if n := h.joins.Load(); n != 1 {
		t.Errorf("serve joined %d times, want once", n)
	}
	if !strings.Contains(h.err.String(), "refused the join token") {
		t.Errorf("the refusal does not say the token was refused:\n%s", h.err)
	}
}

// No join token is spent on a start that is refused anyway.
func TestNoJoinTokenIsSpentOnAStartRefusedAnyway(t *testing.T) {
	h := newHost(t, daemon(t, true), "this is not toml\n")
	h.unjoined(t)
	h.refused(t)
}

// A join token that cannot be read is refused naming the variable, and never repeated.
func TestAJoinTokenThatCannotBeUsedRefusesTheStart(t *testing.T) {
	h := newHost(t, daemon(t, true), secretsTmpfs)
	h.unjoined(t)
	file, _ := h.e.Lookup(runner.JoinTokenFile)
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	said := h.refused(t)
	if !strings.Contains(said, runner.JoinTokenFile+" names a file of mode 0644") {
		t.Errorf("the refusal does not name the variable and the mode:\n%s", said)
	}
	if strings.Contains(said, file) {
		t.Errorf("the refusal repeats the path %s holds:\n%s", runner.JoinTokenFile, said)
	}
}

// What the environment leaves unset, a runner joining again claims none of, rather than keeping it
// from the runner.env it replaces: kept, it would say otherwise than the environment at the next
// start, and join again at every one.
func TestARunnerJoiningAgainClaimsNoneOfWhatItsEnvironmentLeavesUnset(t *testing.T) {
	h := newHost(t, daemon(t, true), secretsTmpfs)
	text, err := os.ReadFile(h.e.EnvFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.e.EnvFile, append(text, []byte("AGK_RUNNER_NAMESPACES=finance\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	h.set(runner.API, h.api)
	h.set(runner.Labels, "zone=dmz,arch=amd64")
	h.withTokenFile(t)
	h.serving(t)
	if claimed, _ := h.claimed.Load().(string); strings.Contains(claimed, "finance") {
		t.Errorf("the join claimed %s, which keeps the namespaces the environment no longer names", claimed)
	}
	h.serving(t)
	if n := h.joins.Load(); n != 1 {
		t.Errorf("the runner joined %d times over two starts, want once", n)
	}
}

// The address written with a trailing slash is the one it joined with, whether or not serve holds a
// join token: neither joins again for it nor refuses it.
func TestAnAddressWithATrailingSlashIsTheOneTheRunnerJoinedWith(t *testing.T) {
	for _, token := range []bool{false, true} {
		h := newHost(t, daemon(t, true), secretsTmpfs)
		h.set(runner.API, h.api+"/")
		h.set(runner.Labels, "zone=dmz,arch=amd64")
		if token {
			h.withTokenFile(t)
		}
		h.serving(t)
		if n := h.joins.Load(); n != 0 {
			t.Errorf("with a token %v, the runner joined %d times for a slash", token, n)
		}
	}
}

// An identity set in the environment, which serve refuses, refuses the join before the token is
// spent, rather than after.
func TestAnIdentityInTheEnvironmentSpendsNoJoinToken(t *testing.T) {
	h := newHost(t, daemon(t, true), secretsTmpfs)
	h.unjoined(t)
	h.set(runner.RunnerID, "runner-dmz-09")
	if said := h.refused(t); !strings.Contains(said, runner.RunnerID+" is set in the environment") {
		t.Errorf("the refusal does not name %s:\n%s", runner.RunnerID, said)
	}
}

// A host that lost half its identity, the key or runner.env, joins again with its join token as a
// new runner rather than being refused at every start.
func TestAHostThatLostHalfItsIdentityJoinsAgainWithItsJoinToken(t *testing.T) {
	for name, c := range map[string]struct {
		lost func(h *host) string
		said string
	}{
		"the key":    {func(h *host) string { return h.e.KeyFile }, "the key it joined with, is gone"},
		"runner.env": {func(h *host) string { return h.e.EnvFile }, "holds a key and no identity"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t, daemon(t, true), secretsTmpfs)
			if err := os.Remove(c.lost(h)); err != nil {
				t.Fatal(err)
			}
			h.set(runner.API, h.api)
			h.set(runner.Labels, "zone=dmz,arch=amd64")
			h.withTokenFile(t)
			h.serving(t)
			if n := h.joins.Load(); n != 1 {
				t.Errorf("serve joined %d times, want once", n)
			}
			for _, want := range []string{c.said, "serving as runner-dmz-03"} {
				if !strings.Contains(h.err.String(), want) {
					t.Errorf("the agent's log does not say %q:\n%s", want, h.err)
				}
			}
		})
	}
}
