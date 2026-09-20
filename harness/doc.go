// Package harness is the registry of coding-agent CLIs: which agents exist,
// how to detect one on a machine, how it takes hooks and instructions, and
// which mode it answers in. Every consumer that needs to name an agent —
// a CLI installing hooks, a daemon reporting what a machine hosts, a server
// launching a session — reads this one table, so an agent's id means the
// same thing everywhere.
//
// The package depends only on the standard library. The conformance suite
// that exercises the registry against real agents lives in
// github.com/belt-sh/harness-test.
package harness
