// Package all imports every pure-Go session codec and exposes them as a
// table keyed by harness id. Importing it pulls in the codecs a program can
// use without a database driver; the SQLite-backed agents (goose, opencode,
// kilo, hermes) live in the transcript/sqlite module and are added to the
// registry when that module is imported.
//
// A program that wants one agent's codec should import that agent's package
// directly. This package is for the case that needs the whole set: a session
// browser, an importer, the conformance suite.
package all

import (
	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/claude"
	"github.com/inference-sh/agentprotocol/transcript/codex"
	"github.com/inference-sh/agentprotocol/transcript/copilot"
	"github.com/inference-sh/agentprotocol/transcript/cursor"
	"github.com/inference-sh/agentprotocol/transcript/droid"
	"github.com/inference-sh/agentprotocol/transcript/gemini"
	"github.com/inference-sh/agentprotocol/transcript/grok"
	"github.com/inference-sh/agentprotocol/transcript/kimi"
	"github.com/inference-sh/agentprotocol/transcript/kiro"
	"github.com/inference-sh/agentprotocol/transcript/pi"
)

// Codecs maps a harness id to its session codec. It holds every codec that
// needs nothing but the standard library.
var Codecs = map[string]transcript.Codec{
	"claude":  claude.Codec,
	"codex":   codex.Codec,
	"copilot": copilot.Codec,
	"cursor":  cursor.Codec,
	"droid":   droid.Codec,
	"gemini":  gemini.Codec,
	"grok":    grok.Codec,
	"kimi":    kimi.Codec,
	"kiro":    kiro.Codec,
	"omp":     pi.OMP,
	"pi":      pi.Codec,
	"qwen":    gemini.Qwen,
}

// Open binds a harness's codec to a home directory. It returns false when no
// codec is registered for the id; the SQLite module registers the
// driver-backed ones through transcript.Register.
func Open(agent, home string) (transcript.Store, bool, error) {
	c, ok := Codecs[agent]
	if !ok {
		if c, ok = transcript.Registered(agent); !ok {
			return nil, false, nil
		}
	}
	st, err := c.Open(home)
	return st, true, err
}
