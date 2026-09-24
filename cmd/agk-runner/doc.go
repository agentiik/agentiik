// Command agk-runner is the agent a runner host runs: one program, shipped as a static binary run
// by a systemd unit and as a container image, with three verbs.
//
//	agk-runner join     trades a join token for the runner's identity
//	agk-runner serve    the agent: what the unit and the container run
//	agk-runner version  prints the version join sends
//
// # Why it is not a verb of agk
//
// join writes what serve reads, so a runner host needs one binary. agk links package api, and
// through it the database and the controller, which this program must keep out: a runner "holds no
// database credential, no secret-store credential and no standing object-store credential", and
// the program on its host links none of the code that would use one either. boundary_test.go holds
// the closure, as driver and graph hold theirs, and reads the linked binary for anything that could
// open an inbound port, since a runner host opens none.
//
// # The floor, and what lifts it
//
// serve refuses a daemon that does not remap user namespaces, before it makes any call to the API.
// require_userns_remap = false in /etc/agentiik/runner.toml lifts that, and nothing else does: no
// flag, no variable. The documentation gives the reason, which is that the line "is a line in
// /etc/agentiik/runner.toml and not a command line flag, so the decision survives in something
// reviewable rather than in somebody's shell history". agk run --local lifts the floor by default
// for a laptop, and this program does not share that path: a missing runner.toml keeps the floor,
// and one that cannot be read refuses the start rather than falling back to anything.
//
// serve also refuses to run as root. The agent needs the group that owns the daemon socket and
// three capabilities to own a task's directory, and a root agent is a runner whose every mistake is
// made as the host's root.
package main
