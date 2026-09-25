package api

import (
	"context"
	"testing"

	"github.com/agentiik/agentiik/schema"
)

// A start waiting on another's compile goes when its caller goes, and a compile that failed keeps
// nothing, so the next start compiles again rather than being handed the failure.
func TestAStartWaitingOnACompileGoesWithItsCallerAndAFailureIsNotKept(t *testing.T) {
	var d declarations
	_, held, done := d.claim(t.Context(), "finance/monthly-invoicing@a")
	if held || done == nil {
		t.Fatalf("the first claim is held %v, with done %v", held, done != nil)
	}
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	if _, held, other := d.claim(gone, "finance/monthly-invoicing@a"); held || other != nil {
		t.Errorf("a claim whose caller went away while another compiled was answered held %v, with done %v", held, other != nil)
	}

	done(nil, false)
	_, held, again := d.claim(t.Context(), "finance/monthly-invoicing@a")
	if held || again == nil {
		t.Fatalf("after a failed compile the claim is held %v, with done %v", held, again != nil)
	}
	again(map[string]schema.Input{"orders": {Required: true}}, true)
	if declared, held, _ := d.claim(t.Context(), "finance/monthly-invoicing@a"); !held || !declared["orders"].Required {
		t.Errorf("a compiled declaration is held %v: %v", held, declared)
	}
}
