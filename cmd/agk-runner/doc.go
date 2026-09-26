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
// serve never serves as root. The agent needs the group that owns the daemon socket and three
// capabilities to own a task's directory, and a root agent is a runner whose every mistake is made
// as the host's root. Started as root, as the image starts it, serve gives the agent's account its
// directories, takes the socket's group, drops to that account and starts itself again, and it
// refuses the start where it cannot; root.go says why it goes about it so.
//
// # Joining on its own
//
// serve given a join token, in AGK_RUNNER_JOIN_TOKEN or the file AGK_RUNNER_JOIN_TOKEN_FILE names,
// joins where the host has no identity yet, and joins again where its key or runner.env is gone or
// its environment claims another address, other labels or other namespaces than the runner.env it
// joined with, so that a runner configured by its environment at every start is never one serving
// as a runner that environment no longer describes.
package main
