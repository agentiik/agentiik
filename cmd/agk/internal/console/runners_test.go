package console

import (
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
)

var (
	four = tea.KeyPressMsg{Code: '4', Text: "4"}
	one  = tea.KeyPressMsg{Code: '1', Text: "1"}
)

var dana = &principal{Principal: "dana", Admin: true, Permissions: map[string][]string{"finance": {"run:read"}}}

// aFleet is three pools and six runners: one of each condition a runner can be in, and one more
// that is ready, so that a pool counts two.
func aFleet() *installation {
	host := func(id, pool string, labels ...string) db.Runner {
		return db.Runner{ID: id, Pool: pool, Labels: labels, CPU: 8, MemoryBytes: 32 << 30, DiskBytes: 200 << 30, Architecture: "amd64",
			AgentVersion: "0.6.0", State: "ready", ReportedState: "ready", Concurrency: 4, JoinedAt: now.Add(-5 * 24 * time.Hour), LastSeenAt: now.Add(-2 * time.Second), RotateBy: now.Add(25 * 24 * time.Hour)}
	}
	ready := host("01jmz8x2d5f8qx7t2n5r8wd3hk", "default")
	ready.Concurrency = 8
	silent := host("01jmz8x3a0b1c2d3e4f5g6h7j8", "default")
	silent.LastSeenAt = now.Add(-45 * time.Second)
	draining := host("01jmz8x9t4m2ke6v0c3b7n1a5q", "dmz", "zone=dmz", "arch=amd64")
	draining.State, draining.DrainedBy, draining.DrainedAt, draining.DrainReason = "draining", "dana", now.Add(-2*time.Hour), "kernel update"
	alsoReady := host("01jmz8xb6c7d8e9f0g1h2j3k4m", "dmz", "zone=dmz", "arch=amd64")
	unhealthy := host("01jmz8xd0e1f2g3h4j5k6m7n8p", "gpu", "gpu=true")
	unhealthy.ReportedState, unhealthy.Concurrency = "unhealthy", 1
	revoked := host("01jmz8xf2g3h4j5k6m7n8p9q0r", "dmz", "zone=dmz", "arch=amd64")
	revoked.State, revoked.RevokedBy, revoked.RevokedAt, revoked.DrainReason = "revoked", "dana", now.Add(-2*24*time.Hour), "disk replaced"
	revoked.ResultsAcceptedUntil, revoked.LastSeenAt = now.Add(-2*24*time.Hour+15*time.Minute), now.Add(-2*24*time.Hour)
	return &installation{
		runs: someRuns(),
		me:   dana,
		pools: []api.Pool{
			{Name: "default", Labels: []string{}, Namespaces: []string{}, Ceilings: &api.Ceilings{}, Containment: "hardened"},
			{Name: "dmz", Labels: []string{"zone=dmz", "arch=amd64"}, Namespaces: []string{"finance", "team-ops"}, Ceilings: &api.Ceilings{CPU: "4", Memory: "8Gi"}, Containment: "hardened"},
			{Name: "gpu", Labels: []string{"gpu=true"}, Namespaces: []string{"research"}, Ceilings: &api.Ceilings{CPU: "16", Memory: "64Gi", PIDs: 4096}, Containment: "sandboxed"},
		},
		runners: []db.Runner{ready, silent, draining, alsoReady, unhealthy, revoked},
	}
}

// 4 turns an administrator's console to the runners: each pool with what it accepts and caps, its
// runners counted by condition and what they take at once, then each runner with its condition as
// a word beside its colour, its heartbeat, and who drained or revoked it.
func TestTheRunnersViewListsThePoolsAndTheirRunners(t *testing.T) {
	in := aFleet()
	m := press(t, opened(t, in, Options{}, 160, 40), four)
	if m.view != runnersView {
		t.Fatalf("4 leaves an administrator's console in view %d", m.view)
	}
	s := screen(m)
	lines := strings.Split(s, "\n")
	if !strings.Contains(lines[0], "1 Runs   2 Workflows   3 Sharing   4 Runners") {
		t.Errorf("the top line does not name the views a digit turns to: %s", lines[0])
	}
	for _, want := range []string{
		"Pools", "3 pools",
		"default      no label             every namespace      none",
		"dmz          zone=dmz,arch=amd64  finance, team-ops    cpu 4 · memory 8Gi",
		"gpu          gpu=true             research             cpu 16 · memory 64Gi · pids 4096 sandboxed   1 unhealthy",
		"1 ready · 1 silent", "1 ready · 1 draining · 1 revoked", "1 unhealthy",
		"Runners", "6 runners",
		"01jmz8x2d5f8qx7t2n5r8wd3hk default      no label             ● ready               8 2s ago",
		"● silent", "45s ago",
		"● draining", "drained by dana, 10:00:00: kernel update",
		"● unhealthy",
		"● revoked", "revoked by dana, 2026-09-23 12:00: disk replaced",
		"never the host it runs on",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the runners view does not say %q:\n%s", want, s)
		}
	}
	// What the ready runners of a pool take at once, a silent or draining one offering nothing.
	for pool, takes := range map[string]string{"default": "8", "dmz": "4", "gpu": "0"} {
		if !lineEndsWith(s, pool+" ", takes) {
			t.Errorf("pool %s does not take %s at once:\n%s", pool, takes, s)
		}
	}
	if last := strings.TrimRight(lines[39], " "); last != "↑↓ Move   esc Runs   q Quit   ? Every key" {
		t.Errorf("the runners view's key line is %q", last)
	}
	for _, a := range []string{"/api/v1/runner-pools", "/api/v1/runners"} {
		if !asked(in, a) {
			t.Errorf("the runners view did not read %s: %v", a, in.asked)
		}
	}
}

var trailing = regexp.MustCompile(` +\n`)

func lineEndsWith(s, start, end string) bool {
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, start) && strings.HasSuffix(strings.TrimRight(l, " "), " "+end) {
			return true
		}
	}
	return false
}

func asked(in *installation, path string) bool {
	for _, a := range in.asked {
		if a == path {
			return true
		}
	}
	return false
}

// The runner chosen is described below the list: what its condition does to work, who stopped it
// and why, what its host has and the dates its credential lives by. ↓ and k move the choice.
func TestTheRunnerChosenIsDescribedBelowTheList(t *testing.T) {
	m := press(t, opened(t, aFleet(), Options{}, 160, 40), four)
	s := screen(m)
	for _, want := range []string{
		"01jmz8x2d5f8qx7t2n5r8wd3hk  ● ready: takes work",
		"pool default · no label · concurrency 8 · 8 vCPU · 32 GiB · 200 GiB disk · amd64 · agent 0.6.0",
		"joined 2026-09-20 12:00 · last heartbeat 2s ago · its credential rotated by 2026-10-20 12:00",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the runner chosen is not described as %q:\n%s", want, s)
		}
	}
	m = press(t, m, j, down)
	if m.runner != "01jmz8x9t4m2ke6v0c3b7n1a5q" {
		t.Fatalf("j then ↓ chooses %q", m.runner)
	}
	if s := screen(m); !strings.Contains(s, "01jmz8x9t4m2ke6v0c3b7n1a5q  ● draining: takes nothing new, and finishes what it holds") {
		t.Errorf("the draining runner is not described:\n%s", s)
	}
	m = press(t, m, j, j, j, j)
	s = screen(m)
	if m.runner != "01jmz8xf2g3h4j5k6m7n8p9q0r" || !strings.Contains(s, "● revoked: out of service for good") ||
		!strings.Contains(s, "revoked by dana, 2026-09-23 12:00: disk replaced, its results taken until 2026-09-23 12:15") {
		t.Errorf("j past the last runner leaves %q:\n%s", m.runner, s)
	}
	m = press(t, m, tea.KeyPressMsg{Code: 'k', Text: "k"}, tea.KeyPressMsg{Code: 'k', Text: "k"}, up)
	if !strings.Contains(screen(m), "01jmz8x9t4m2ke6v0c3b7n1a5q  ● draining") || m.runner != "01jmz8x9t4m2ke6v0c3b7n1a5q" {
		t.Errorf("k twice then ↑ chooses %q", m.runner)
	}
}

// A silent runner is said to be one, since it is no longer doing what it last reported: three
// heartbeats missed have had its tasks declared lost.
func TestASilentRunnerIsSaidSilent(t *testing.T) {
	m := press(t, opened(t, aFleet(), Options{}, 160, 40), four, j)
	if s := screen(m); !strings.Contains(s, "● silent: silent for 45s: three heartbeats missed, and the tasks it held declared lost") {
		t.Errorf("the silent runner is not said to be one:\n%s", s)
	}
}

// At 80 columns a runner keeps its first twelve characters, its pool, condition and heartbeat, and
// its labels take what is left; every line is still as wide as the window.
func TestTheRunnersViewFitsEightyColumns(t *testing.T) {
	m := press(t, opened(t, aFleet(), Options{}, 80, 24), four)
	s := screen(m)
	if !strings.Contains(s, "01jmz8x2d5f… default      ● ready     2s ago") || strings.Contains(s, "01jmz8x2d5f8qx7t2n5r8wd3hk default") {
		t.Errorf("at 80 columns a runner's row is not cut to what fits:\n%s", s)
	}
	// What is said of the runner chosen is broken between two things said, not cut.
	for _, want := range []string{"pool default · no label · concurrency 8 · 8 vCPU · 32 GiB · 200 GiB disk · amd64\n", "agent 0.6.0", "its credential rotated by 2026-10-20 12:00"} {
		if !strings.Contains(trailing.ReplaceAllString(s, "\n"), want) {
			t.Errorf("at 80 columns the runner chosen does not say %q:\n%s", want, s)
		}
	}
	for _, size := range [][2]int{{80, 24}, {120, 30}, {160, 44}} {
		m := press(t, opened(t, aFleet(), Options{}, size[0], size[1]), four)
		lines := strings.Split(screen(m), "\n")
		if len(lines) != size[1] {
			t.Errorf("the runners view is %d lines high in a window of %d", len(lines), size[1])
		}
		for i, l := range lines {
			if w := len([]rune(l)); w != size[0] {
				t.Errorf("line %d is %d columns wide in a window of %d: %q", i, w, size[0], l)
			}
		}
	}
}

// Somebody who is not an administrator is offered no runners: no tab, no key, and nothing read.
func TestTheRunnersAreAnAdministratorsAlone(t *testing.T) {
	in := &installation{runs: someRuns()}
	m := press(t, opened(t, in, Options{}, 160, 40), four)
	if m.view != runsView {
		t.Errorf("4 turns a console that is not an administrator's to view %d", m.view)
	}
	if strings.Contains(screen(m), "Runners") {
		t.Errorf("the runners are offered to who may not read them:\n%s", screen(m))
	}
	if s := screen(press(t, m, help)); strings.Contains(s, "runners") {
		t.Errorf("the list of keys names the runners to who may not read them:\n%s", s)
	}
	for _, a := range in.asked {
		if strings.HasPrefix(a, "/api/v1/runner") {
			t.Errorf("%s was asked for by somebody who is not an administrator", a)
		}
	}
}

// 1 and 4 turn to the runs and to the runners from any view, the run inspector included, and esc
// goes back to the runs from the runners.
func TestTheDigitsTurnBetweenTheViews(t *testing.T) {
	in := aFleet()
	in.run = aFailedRun()
	m := press(t, opened(t, in, Options{}, 160, 40), enter)
	if m.view != runView {
		t.Fatalf("enter leaves view %d", m.view)
	}
	m = press(t, m, four)
	if m.view != runnersView || m.run != nil {
		t.Errorf("4 from a run leaves view %d, the run kept: %v", m.view, m.run != nil)
	}
	m = press(t, m, one)
	if m.view != runsView || !strings.Contains(screen(m), "STATE       RUN") {
		t.Errorf("1 from the runners leaves view %d:\n%s", m.view, screen(m))
	}
	m = press(t, m, four, esc)
	if m.view != runsView {
		t.Errorf("esc from the runners leaves view %d", m.view)
	}
}

// A read again asked for by a view since left is dropped, even once that view is come back to,
// which asks for its own: otherwise each return would read it once more every five seconds.
func TestAReadAskedForByAViewLeftIsDropped(t *testing.T) {
	m := opened(t, aFleet(), Options{}, 160, 40)
	before := m.shown
	m = press(t, m, four, one)
	if _, cmd := m.Update(again{view: runsView, shown: before}); cmd != nil {
		t.Error("a read asked for before the runs were left and come back to is still made")
	}
	if _, cmd := m.Update(again{view: runsView, shown: m.shown}); cmd == nil {
		t.Error("the read the runs ask for now is not made")
	}
}
