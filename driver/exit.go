package driver

import (
	"fmt"

	"github.com/agentiik/agentiik/agk"
)

// The exit code table is read here and nowhere else in this package.
//
// Every band is read off the code a container exited with, which is the only place it
// can be read: nothing in the manifest declares the codes a brick may exit with, so
// there is nothing for publication to refuse. agk.Band is the table itself, and what
// this file adds is the two things the driver has to do with a band, which are the state
// to report and who the code is charged to.

// ExitContractBroken is the exit code a task is reported with when its container exited 0
// and what it left broke the output contract, which ErrOutputsRefused marks: an envelope
// above inline_max_bytes, envelope_max_bytes or max_items, or one that is not an envelope
// at all. It is 121, the first of the codes the table reserves for the runner, where a
// brick "is treated as having failed the contract, whatever its manifest says". The step
// fails, the failure is the brick's, and no retry policy reaches it, since the band is not
// one retry.on can name.
const ExitContractBroken = 121

// ExitOutputsUnwritten is the exit code a task is reported with when its container exited 0,
// its outputs passed, and the store would not take them: refused, past its upload policy or
// out of reach. A failed task whose container ran carries a code, and 0 would say it
// succeeded. It is 125, the first of the codes the table reads as an infrastructure
// failure, charged to the runner and not to the brick, since the brick did what it was
// asked.
const ExitOutputsUnwritten = 125

// exitState is the state a container that exited reports.
//
// Exit 0 is succeeded and every other code is failed, with the code carried alongside so
// that agk.Band can be read again by whoever decides about a retry. There is no third
// answer: a deadline that fired and a stop that landed are facts the code cannot carry,
// since 137 is what a SIGKILL leaves behind whoever sent it, so the caller that sent the
// signal is the one that reports agk.TaskTimedOut or agk.TaskCancelled, and nothing is
// inferred from the code about it here.
//
// agk.TaskLost is never returned. That is the heartbeat's word for a runner that stopped
// reporting, and a container that exited reported.
func exitState(code int) agk.TaskState {
	if agk.Band(code) == agk.BandSuccess {
		return agk.TaskSucceeded
	}
	return agk.TaskFailed
}

// chargedToPlatform says whether a code is the runtime's rather than the brick's.
//
// It is the band from 125 up, which the table reads as an infrastructure failure charged
// to the runner and not to the brick. Charging is what the driver does with the fact and
// not a value it invents: the code is reported exactly as the container exited, which
// agk.Band already makes unretryable and nameless to retry.on, and a line in the log
// says whose failure it was. The driver never invents a code in that band on a brick's
// behalf, because a driver reporting its own trouble as a brick failure fails somebody
// else's step.
func chargedToPlatform(code int) bool {
	return agk.Band(code) == agk.BandRuntimeFailure
}

// readExit is the whole of what a container's exit code becomes: the line that says how
// it was read, written into the log, and the state to report.
//
// The two are done together because the second half of charging a code to the runtime is
// saying so where a person will read it. A caller that took the state without the line
// would report an infrastructure failure with a log that blames the brick.
func readExit(l *taskLog, code int) agk.TaskState {
	l.note("%s", exitNote(code))
	return exitState(code)
}

// exitNote is what the log says about the code a container exited with.
//
// It is the table's own two columns, the meaning and the handling, in the documentation's
// words, so that a person reading the log reads the rule rather than a number.
func exitNote(code int) string {
	band := agk.Band(code)
	switch band {
	case agk.BandSuccess:
		return "the container exited 0: the output envelopes are published"
	case agk.BandApplicationFailure:
		return fmt.Sprintf("the container exited %d, which the exit code table reads as an %s: the step failed, and there is no retry unless retry.on says so explicitly", code, band)
	case agk.BandTransientFailure:
		return fmt.Sprintf("the container exited %d, which the exit code table reads as a %s: it is retried according to the step policy", code, band)
	case agk.BandInvalidInput:
		return fmt.Sprintf("the container exited %d, which the exit code table reads as %s: a permanent failure, never retried, whatever retry says", code, band)
	case agk.BandReservedForRunner:
		return fmt.Sprintf("the container exited %d, which the exit code table has %s: a brick that exits with one is treated as having failed the contract, whatever its manifest says", code, band)
	default:
		return fmt.Sprintf("the container exited %d, which the exit code table has %s: read as an infrastructure failure, charged to the runner and not to the brick", code, band)
	}
}

// stoppedNote is what the log says about the code of a container this side stopped, at its
// deadline or on a stop.
//
// The table is not read for it. A container killed at its deadline leaves 137, which the
// table has among the codes charged to the runtime, and a log that said so would blame the
// runner for a timeout the step's own timeout decided.
func stoppedNote(code int, state agk.TaskState) string {
	if state == agk.TaskTimedOut {
		return fmt.Sprintf("the container exited %d after it was stopped at its deadline, so the task timed out: the code says how it was stopped and not why", code)
	}
	return fmt.Sprintf("the container exited %d after a stop landed, so the task was cancelled: the code says how it was stopped and not why", code)
}
