package api

import (
	"fmt"
	"net/http/httptest"
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

// A sign-in's address is the connection's, whatever X-Forwarded-For says, unless the API is served
// behind the proxy AGK_PROXY_URL names: then it is the header's last entry, the one that proxy wrote
// after whatever its client sent, of the last header where there are several, and the connection's
// where the header holds nothing that reads as an address.
func TestASignInsAddressIsTheProxysLastEntryBehindTheProxyAlone(t *testing.T) {
	for _, c := range []struct {
		forwarded []string
		proxied   bool
		want      string
	}{
		{nil, false, "127.0.0.1"},
		{[]string{"203.0.113.9"}, false, "127.0.0.1"},
		{nil, true, "127.0.0.1"},
		{[]string{"203.0.113.9"}, true, "203.0.113.9"},
		{[]string{"192.0.2.66, 10.1.1.1,203.0.113.9"}, true, "203.0.113.9"},
		{[]string{"192.0.2.66", "203.0.113.9"}, true, "203.0.113.9"},
		{[]string{"2001:db8::7"}, true, "2001:db8::7"},
		{[]string{"203.0.113.9, unknown"}, true, "127.0.0.1"},
		{[]string{"203.0.113.9, "}, true, "127.0.0.1"},
		{[]string{"[2001:db8::7]:443"}, true, "127.0.0.1"},
	} {
		r := httptest.NewRequest("POST", "/api/v1/auth/login", nil)
		r.RemoteAddr = "127.0.0.1:51234"
		for _, v := range c.forwarded {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := addressOf(r, c.proxied); got != c.want {
			t.Errorf("X-Forwarded-For %q, proxied %v, is read as %s", c.forwarded, c.proxied, got)
		}
	}
}
