// Package runner is the agent: what agk-runner serve runs on a runner host, around the container
// driver.
//
// The driver is the half that runs one task on one daemon, and it is shared with agk run --local.
// This is the other half, the one only a server has: the settings a host is given, the credential
// it joined with, the calls it makes to the API, and the service manager it tells when it is
// ready. Serve composes them, and the parts that take, run, heartbeat and report arrive as files
// of this package beside it.
//
// # What it may link
//
// A runner "holds no database credential, no secret-store credential and no standing object-store
// credential", and the program on its host links none of the code that would use one either. So
// this package links no controller, no database, no API and no secret store, and it reaches the
// API over HTTP with types of its own rather than importing the API's. The test that holds this is
// cmd/agk-runner/boundary_test.go, which reads the whole closure of the program rather than of
// this package, since the program is what a host runs.
//
// # What it may not open
//
// "No inbound port is ever opened on a runner host; every connection is outbound, to the API, the
// bus, the object store and the registry." Nothing here listens, and the same test reads the
// linked binary's symbols to hold that nothing that could is in it.
package runner
