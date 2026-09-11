package ulid

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewIsTwentySixCrockfordCharacters(t *testing.T) {
	for i := 0; i < 1000; i++ {
		id := New()
		if len(id) != 26 {
			t.Fatalf("New() = %q, want twenty-six characters, got %d", id, len(id))
		}
		for _, c := range id {
			if !strings.ContainsRune(crockford, c) {
				t.Fatalf("New() = %q, which carries %q, outside Crockford base32", id, c)
			}
		}
		if id[0] > '7' {
			t.Fatalf("New() = %q, whose first character is above 7", id)
		}
	}
}

func TestNewSortsInTheOrderItWasMinted(t *testing.T) {
	const n = 20000
	ids := make([]string, n)
	for i := range ids {
		ids[i] = New()
	}
	for i := 1; i < n; i++ {
		if ids[i] <= ids[i-1] {
			t.Fatalf("identifier %d is %q and %d is %q, which does not sort after it", i-1, ids[i-1], i, ids[i])
		}
	}
}

func TestNewCarriesTheMintingTime(t *testing.T) {
	before := time.Now().UTC().UnixMilli()
	id := New()
	after := time.Now().UTC().UnixMilli()

	var ms int64
	for _, c := range id[:10] {
		ms = ms<<5 | int64(strings.IndexRune(crockford, c))
	}
	if ms < before || ms > after {
		t.Fatalf("New() = %q, whose timestamp is %d, outside [%d, %d]", id, ms, before, after)
	}
}

func TestNewIsSafeForConcurrentUse(t *testing.T) {
	const goroutines, each = 8, 2000

	var wg sync.WaitGroup
	seen := make(chan string, goroutines*each)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				seen <- New()
			}
		}()
	}
	wg.Wait()
	close(seen)

	unique := make(map[string]bool, goroutines*each)
	for id := range seen {
		if unique[id] {
			t.Fatalf("New() returned %q twice", id)
		}
		unique[id] = true
	}
}

func TestIncrementCarries(t *testing.T) {
	e := [10]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 255}
	if !increment(&e) {
		t.Fatal("increment overflowed on the first carry")
	}
	if e != [10]byte{0, 0, 0, 0, 0, 0, 0, 0, 1, 0} {
		t.Fatalf("increment carried to %v", e)
	}

	full := [10]byte{255, 255, 255, 255, 255, 255, 255, 255, 255, 255}
	if increment(&full) {
		t.Fatal("increment claimed to stay inside eighty bits after the last value")
	}
}
