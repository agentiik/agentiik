package main

import (
	"crypto/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/repo"
)

// A second commit moves the branch, and what the push sends leaves out what the installation holds:
// the repository ends at the new commit, with the first still in its history.
func TestASecondCommitMovesTheBranchTheInstallationHolds(t *testing.T) {
	dir := repository(t)
	in := aPushStandIn(t, http.StatusOK)
	first := gitIn(t, dir, "rev-parse", "HEAD")
	if code, out, errs := pushAgainst(dir, in.server.URL); code != exitSucceeded {
		t.Fatalf("the first push answered %d: %s%s", code, out, errs)
	}
	write(t, dir, "notes.txt", "a second commit\n")
	commitAll(t, dir, "notes")
	second := gitIn(t, dir, "rev-parse", "HEAD")
	code, out, errs := pushAgainst(dir, in.server.URL)
	if code != exitSucceeded || !strings.Contains(out, "pushed to") {
		t.Fatalf("the second push answered %d: %s%s", code, out, errs)
	}
	bare := in.bare("monthly-invoicing")
	branch := gitIn(t, dir, "symbolic-ref", "HEAD")
	if at := in.git(bare, "rev-parse", branch); at != second {
		t.Errorf("%s is at %s, where the push left it at %s", branch, at, second)
	}
	if parent := in.git(bare, "rev-parse", second+"^"); parent != first {
		t.Errorf("the pushed commit's parent is %s, where it is %s", parent, first)
	}
}

// A branch the installation holds at a commit the pushed one is not ahead of is not moved: that
// would be a forced push, rewriting what others fetched, which agk push never makes.
func TestABranchThePushIsNotAheadOfIsNotMoved(t *testing.T) {
	dir := repository(t)
	in := aPushStandIn(t, http.StatusOK)
	write(t, dir, "notes.txt", "one\n")
	commitAll(t, dir, "one")
	pushed := gitIn(t, dir, "rev-parse", "HEAD")
	if code, out, errs := pushAgainst(dir, in.server.URL); code != exitSucceeded {
		t.Fatalf("the first push answered %d: %s%s", code, out, errs)
	}
	gitIn(t, dir, "reset", "-q", "--hard", "HEAD~1")
	write(t, dir, "notes.txt", "another\n")
	commitAll(t, dir, "another")

	code, out, errs := pushAgainst(dir, in.server.URL)
	if code != exitRefused || !strings.Contains(errs, "is not ahead of it") || strings.Contains(out, "pushed to") {
		t.Fatalf("a push that is not ahead answered %d: %s%s", code, out, errs)
	}
	branch := gitIn(t, dir, "symbolic-ref", "HEAD")
	if at := in.git(in.bare("monthly-invoicing"), "rev-parse", branch); at != pushed {
		t.Errorf("%s moved to %s", branch, at)
	}
}

// A branch the installation holds at a commit this clone has never fetched is not moved, and the
// refusal says to fetch it: whether the push is ahead of it cannot be known here.
func TestABranchAtACommitTheCloneLacksIsNotMoved(t *testing.T) {
	dir := repository(t)
	in := aPushStandIn(t, http.StatusOK)
	if code, out, errs := pushAgainst(dir, in.server.URL); code != exitSucceeded {
		t.Fatalf("the first push answered %d: %s%s", code, out, errs)
	}
	branch := gitIn(t, dir, "symbolic-ref", "HEAD")
	bare := in.bare("monthly-invoicing")
	tree := in.git(bare, "rev-parse", branch+"^{tree}")
	elsewhere := in.git(bare, "-c", "user.name=Bob", "-c", "user.email=bob@example.com", "commit-tree", tree, "-p", branch, "-m", "pushed from another clone")
	in.git(bare, "update-ref", branch, elsewhere)
	write(t, dir, "notes.txt", "mine\n")
	commitAll(t, dir, "mine")

	code, out, errs := pushAgainst(dir, in.server.URL)
	if code != exitRefused || !strings.Contains(errs, "which this repository does not hold: fetch it") {
		t.Fatalf("a push behind a commit it lacks answered %d: %s%s", code, out, errs)
	}
	if at := in.git(bare, "rev-parse", branch); at != elsewhere {
		t.Errorf("%s moved to %s", branch, at)
	}
}

// A tag the installation holds is not moved to another commit: a tag others fetched names one
// commit for good.
func TestATagTheInstallationHoldsIsNotMoved(t *testing.T) {
	dir := repository(t)
	in := aPushStandIn(t, http.StatusOK)
	gitIn(t, dir, "tag", "v1")
	tagged := gitIn(t, dir, "rev-parse", "v1")
	if code, out, errs := pushAgainst(dir, in.server.URL, "--commit", "v1"); code != exitSucceeded {
		t.Fatalf("the tag's first push answered %d: %s%s", code, out, errs)
	}
	write(t, dir, "notes.txt", "later\n")
	commitAll(t, dir, "later")
	gitIn(t, dir, "tag", "-f", "v1")

	code, out, errs := pushAgainst(dir, in.server.URL, "--commit", "v1")
	if code != exitRefused || !strings.Contains(errs, "a tag names one commit for good") {
		t.Fatalf("a moved tag answered %d: %s%s", code, out, errs)
	}
	if at := in.git(in.bare("monthly-invoicing"), "rev-parse", "refs/tags/v1"); at != tagged {
		t.Errorf("v1 moved to %s", at)
	}
}

// A name that is both a branch and a tag is refused, as the installation refuses it, rather than
// read as one here and the other there; HEAD names the branch checked out.
func TestTheRefANamePushesIsTheOneGitReadsTheCommitThrough(t *testing.T) {
	dir := repository(t)
	gitIn(t, dir, "branch", "x")
	gitIn(t, dir, "tag", "x")
	in := aPushStandIn(t, http.StatusOK)
	code, out, errs := pushAgainst(dir, in.server.URL, "--commit", "x")
	if code != exitRefused || !strings.Contains(errs, "both a branch and a tag") {
		t.Errorf("an ambiguous name answered %d: %s%s", code, out, errs)
	}
	code, out, errs = pushAgainst(dir, in.server.URL, "--commit", "HEAD")
	branch := gitIn(t, dir, "symbolic-ref", "HEAD")
	if code != exitSucceeded || !strings.Contains(out, "as "+branch) {
		t.Errorf("--commit HEAD answered %d: %s%s", code, out, errs)
	}
	code, out, errs = pushAgainst(dir, in.server.URL, "--commit", "HEAD~0")
	if code != exitRefused || !strings.Contains(errs, "no branch or tag of this repository") {
		t.Errorf("a revision expression answered %d: %s%s", code, out, errs)
	}
}

// A shallow clone is pushed where the installation holds the history it lacks, and refused naming
// git fetch --unshallow where it does not: the installation holds every object a commit reaches.
func TestAShallowCloneIsPushedOnlyOverHistoryTheInstallationHolds(t *testing.T) {
	origin := repository(t)
	write(t, origin, "notes.txt", "two\n")
	commitAll(t, origin, "two")
	shallow := filepath.Join(t.TempDir(), "shallow")
	gitIn(t, origin, "clone", "-q", "--depth", "1", "file://"+origin, shallow)
	for _, args := range [][]string{{"config", "user.email", "test@example.com"}, {"config", "user.name", "Test"}} {
		gitIn(t, shallow, args...)
	}

	empty := aPushStandIn(t, http.StatusOK)
	code, out, errs := pushAgainst(shallow, empty.server.URL)
	if code != exitRefused || !strings.Contains(errs, "git fetch --unshallow") {
		t.Errorf("a shallow clone pushed to a repository lacking its history answered %d: %s%s", code, out, errs)
	}

	holding := aPushStandIn(t, http.StatusOK)
	if code, out, errs := pushAgainst(origin, holding.server.URL); code != exitSucceeded {
		t.Fatalf("the whole clone's push answered %d: %s%s", code, out, errs)
	}
	write(t, shallow, "notes.txt", "three\n")
	commitAll(t, shallow, "three")
	code, out, errs = pushAgainst(shallow, holding.server.URL)
	if code != exitSucceeded {
		t.Errorf("a shallow clone over history the installation holds answered %d: %s%s", code, out, errs)
	}
}

// A push a gateway answers with a 5xx, or whose answer is cut off, has no outcome, exit 4, rather
// than the refusal of exit 1: the ref may have moved, and pushing again reads where it stands.
func TestAPushAnsweredByAGatewayOrCutOffHasNoOutcome(t *testing.T) {
	for name, receiving := range map[string]http.HandlerFunc{
		"a gateway's 504": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusGatewayTimeout)
			w.Write([]byte("<html>504 Gateway Time-out</html>"))
		},
		"an answer cut off": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
			var b []byte
			b, _ = repo.AppendPkt(b, append([]byte{1}, "000eunpack ok\n"...))
			w.Write(b[:len(b)-3])
		},
		"an answer naming no status": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
			var b []byte
			b, _ = repo.AppendPkt(b, append([]byte{1}, "000eunpack ok\n0000"...))
			w.Write(append(b, "0000"...))
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := repository(t)
			in := aPushStandIn(t, http.StatusOK)
			in.receiving = receiving
			code, out, errs := pushAgainst(dir, in.server.URL)
			if code != exitNoOutcome || !strings.Contains(errs, "whether the push landed is unknown") {
				t.Errorf("answered %d: %s%s", code, out, errs)
			}
		})
	}
}

// An installation that refuses a pack before reading all of it, one past its bound, is heard: its
// refusal is said, not the broken pipe it leaves git writing the rest.
func TestARefusalBeforeThePackIsReadIsSaidRatherThanTheBrokenPipe(t *testing.T) {
	dir := repository(t)
	big := make([]byte, 4<<20)
	rand.Read(big)
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	commitAll(t, dir, "big")
	in := aPushStandIn(t, http.StatusOK)
	in.receiving = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		var report []byte
		report, _ = repo.AppendPkt(report, []byte("unpack the pack is past the installation's bound\n"))
		report = append(report, "0000"...)
		var b []byte
		b, _ = repo.AppendPkt(b, append([]byte{1}, report...))
		w.Write(append(b, "0000"...))
	}
	code, out, errs := pushAgainst(dir, in.server.URL)
	if code != exitRefused || !strings.Contains(errs, "past the installation's bound") || strings.Contains(errs, "could not be written") {
		t.Errorf("answered %d: %s%s", code, out, errs)
	}
}
