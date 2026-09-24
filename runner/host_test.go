package runner

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// memoryForm is the wire's grammar for a capacity's memory and disk, copied from
// $defs/runnerRegistration.
var memoryForm = regexp.MustCompile(`^[1-9][0-9]*(?:Ki|Mi|Gi|Ti)$`)

func TestMemoryIsReadFromMeminfoInKibibytes(t *testing.T) {
	got, err := memTotal(filepath.Join("testdata", "meminfo"))
	if err != nil {
		t.Fatal(err)
	}
	if got != 16318196<<10 {
		t.Errorf("the fixture's MemTotal of 16318196 kB read as %d bytes", got)
	}
}

func TestAMeminfoWithoutItsMemTotalIsRefused(t *testing.T) {
	for name, text := range map[string]string{
		"no MemTotal line":               "MemFree:          812344 kB\n",
		"another unit":                   "MemTotal:       16318196 MB\n",
		"no number":                      "MemTotal:       lots kB\n",
		"no memory at all":               "MemTotal:              0 kB\n",
		"a negative amount":              "MemTotal:       -16318196 kB\n",
		"a number past 63 bits of bytes": "MemTotal:       99999999999999999 kB\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "meminfo")
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			if got, err := memTotal(path); err == nil || !strings.Contains(err.Error(), "memory cannot be measured") {
				t.Errorf("read as %d bytes (%v)", got, err)
			}
		})
	}
}

// A size is written in the largest unit that holds it exactly, so that a host is declared as
// large as it is, and rounded down to a kibibyte where none does, so that it is never declared
// larger.
func TestASizeIsWrittenInTheWiresGrammar(t *testing.T) {
	for bytes, want := range map[int64]string{
		64 << 30:            "64Gi",
		1 << 40:             "1Ti",
		3 << 39:             "1536Gi",
		16318196 << 10:      "16318196Ki",
		400<<30 + 4096:      "419430404Ki",
		2<<20 + 1023:        "2048Ki",
		1 << 10:             "1Ki",
		9223372036854775807: "9007199254740991Ki",
	} {
		got, err := sizeOf(bytes)
		if err != nil || got != want {
			t.Errorf("%d bytes is written %q (%v), and should be %q", bytes, got, err, want)
		}
		if !memoryForm.MatchString(got) {
			t.Errorf("%q is not in the wire's grammar", got)
		}
	}
	if got, err := sizeOf(1023); err == nil {
		t.Errorf("1023 bytes is written %q, and the grammar counts from 1Ki", got)
	}
}

// The disk measured is that of the work root's filesystem, which serve creates, so a work root
// that does not exist yet is measured where it will be.
func TestTheDiskIsMeasuredWhereTheWorkRootWillBe(t *testing.T) {
	dir := t.TempDir()
	there, err := diskAvailable(dir)
	if err != nil {
		t.Fatal(err)
	}
	yet, err := diskAvailable(filepath.Join(dir, "not", "yet"))
	if err != nil {
		t.Fatal(err)
	}
	if there <= 0 || yet <= 0 {
		t.Errorf("the disk under a temporary directory measured %d and %d bytes", there, yet)
	}
}
