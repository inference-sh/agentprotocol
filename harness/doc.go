// Package harness is the registry of coding-agent CLIs: which agents exist,
// how to detect one on a machine, how it takes hooks and instructions, and
// which mode it answers in. Every consumer that needs to name an agent —
// a CLI installing hooks, a daemon reporting what a machine hosts, a server
// launching a session — reads this one table, so an agent's id means the
// same thing everywhere.
//
// Versions: Harness.Tested is the range of each agent's versions inference
// has verified, Harness.Requires the floor below which a capability the
// driver needs is missing, and Support(name, version) places an installed
// version against both, runs nothing, and carries a user-facing reason and
// the upgrade command. Only a version below Requires is refused. Behaviour that exists only on some versions is recorded once as a
// VersionRange in Harness.Features and read with HasFeature, rather than by
// forking the entry per version.
//
// The package depends only on the standard library. The conformance suite
// that exercises the registry against real agents lives in
// github.com/belt-sh/harness-test.
package harness
