package main

import (
	"fmt"
	"io"
	"os"

	"github.com/agentiik/agentiik/audit"
)

// auditVerify checks the chain of an audit log export, and answers the exit code: 0 where it holds,
// 1 where it breaks or cannot be read.
//
// It reads a file and nothing else, no configuration and no database, because it is run where the
// export was received, outside the installation, and most of all when the installation's own host
// is the one in question.
func auditVerify(path string, stdout, stderr io.Writer) int {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(stderr, "%s audit-verify: the export could not be opened: %s\n", program, err)
		return exitFailed
	}
	defer f.Close()
	v, err := audit.VerifyExport(f)
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "%s audit-verify: %s\n", program, err)
		return exitFailed
	case v.First == 1:
		fmt.Fprintf(stdout, "entries 1 to %d hold, from the start of the chain\n", v.Last)
	default:
		fmt.Fprintf(stdout, "entries %d to %d hold, taking entry %d's hash of the one before it as given: the export holds nothing earlier\n", v.First, v.Last, v.First)
	}
	return exitStopped
}
