package api

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A failed task's message is the end of its log at most failureMost bytes long: whole lines where
// the last of them fits, and where it alone does not, its end from where a character begins, so
// that a message never opens on half a line or half a character.
func TestAFailedTasksMessageIsTheEndOfItsLog(t *testing.T) {
	if got := lastOf([]string{"one", "two"}, failureMost); got != "one\ntwo" {
		t.Errorf("a short log is told as %q", got)
	}
	lines := []string{strings.Repeat("a", 1500), strings.Repeat("b", 1000), "refused"}
	if got := lastOf(lines, failureMost); got != strings.Repeat("b", 1000)+"\nrefused" {
		t.Errorf("a long log is told from %.20q, %d bytes", got, len(got))
	}
	long := strings.Repeat("\u00e9", failureMost)
	got := lastOf([]string{"before", long}, failureMost)
	if !utf8.ValidString(got) || len(got) > failureMost || !strings.HasSuffix(long, got) || len(got) < failureMost-1 {
		t.Errorf("a line longer than the message is told as %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
}
