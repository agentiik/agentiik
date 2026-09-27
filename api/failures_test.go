package api

import (
	"fmt"
	"testing"
	"time"
)

// No failed sign-in goes uncounted: every one is recorded, or counted in an entry recorded after
// it, however many addresses they come from and whatever the counts forget of the addresses whose
// window has ended. What is left is what no entry has followed yet.
func TestNoFailedSignInGoesUncounted(t *testing.T) {
	f := newFailedSignIns()
	now := time.Now()
	failed, recorded, counted := 0, 0, 0
	fail := func(address string) {
		failed++
		if unrecorded, ok := f.admit(address, now); ok {
			recorded++
			counted += unrecorded
		}
	}
	for range failuresRecorded + 5 {
		fail("198.51.100.4")
	}
	for i := range failuresTracked + 200 {
		fail(fmt.Sprintf("10.%d.%d.1", i/256, i%256))
	}
	now = now.Add(failuresWindow)
	for i := range failuresTracked + 10 {
		fail(fmt.Sprintf("10.%d.%d.2", i/256, i%256))
	}
	if left := f.recordedAnyway(); recorded+counted+left != failed {
		t.Errorf("%d failures are %d recorded, %d counted in entries and %d left to count", failed, recorded, counted, left)
	}
	if len(f.by) > failuresTracked+1 {
		t.Errorf("the counts hold %d addresses", len(f.by))
	}
	if recorded != 2*failuresRecordedAll {
		t.Errorf("two windows recorded %d entries", recorded)
	}
}

// An address is counted as itself where it is IPv4, IPv4 mapped into IPv6 included, and as its
// /64 where it is IPv6, since one machine is handed a /64.
func TestAFailedSignInIsCountedUnderItsAddressOrItsSlash64(t *testing.T) {
	for address, key := range map[string]string{
		"192.0.2.7":          "192.0.2.7",
		"::ffff:192.0.2.7":   "192.0.2.7",
		"2001:db8:7:1::1":    "2001:db8:7:1::/64",
		"2001:db8:7:1:ab::9": "2001:db8:7:1::/64",
		"fe80::1%en0":        "fe80::/64",
		"not an address":     "not an address",
	} {
		if got := failureKey(address); got != key {
			t.Errorf("%s is counted under %s", address, got)
		}
	}
}
