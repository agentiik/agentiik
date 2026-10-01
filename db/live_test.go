package db_test

import (
	"testing"

	"github.com/agentiik/agentiik/db"
)

// A payload of the live channel is read as the triggers of migration 0067 write it, and one they do
// not write is refused, which WatchLive then says as everything.
func TestALiveChangeIsReadAsItsTriggerWritesIt(t *testing.T) {
	for payload, want := range map[string]db.LiveChange{
		"run finance monthly-invoicing 01JMZ8W4K2R7AAAAAAAAAAAAAA": {Kind: db.ChangedRun, Namespace: "finance", Workflow: "monthly-invoicing", Run: "01JMZ8W4K2R7AAAAAAAAAAAAAA"},
		"notifications finance/nightly-sync":                       {Kind: db.ChangedNotifications, Recipient: "finance/nightly-sync"},
		"runners":                                                  {Kind: db.ChangedRunners},
	} {
		if got, err := db.ParseLiveChange(payload); err != nil || got != want {
			t.Errorf("%q is read as %+v, %v", payload, got, err)
		}
	}
	for _, payload := range []string{"", "run finance monthly-invoicing", "all", "runners now", "notifications"} {
		if got, err := db.ParseLiveChange(payload); err == nil {
			t.Errorf("%q is read as %+v", payload, got)
		}
	}
}
