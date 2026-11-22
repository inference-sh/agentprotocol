package harness

import "strings"

// Everything below is belt as this package installed it before callers
// passed their own PluginSource: the names its files and agents are written
// under, and the command its hooks run. It is the one place belt is named.
//
// Deprecated: remove once go/cli passes its own PluginSource.
const legacySourceName = "belt"

var legacySource = PluginSource{Name: legacySourceName, HookCommand: beltCmd, owns: legacyOwns}

func beltCmd(event HookEvent) string {
	return "belt plugin hook " + string(event)
}

// legacyOwns matches every belt hook command, including the ones older belt
// versions wrote, which no exact command list would name.
func legacyOwns(cmd string) bool { return strings.Contains(cmd, "belt plugin hook") }

// GoosePluginDir is belt's plugin directory under goose's shared plugin root.
//
// It was ".agents/plugins/belt-test/hooks" until 2026-09: this suite's name
// had been copied into the shipped registry, so every belt user's goose hooks
// went into a directory named after the test harness. Renaming it moves the
// install, so Install removes the old one.
//
// Deprecated: a source's goose directory is .agents/plugins/<source name>.
const GoosePluginDir = ".agents/plugins/" + legacySourceName

// gooseLegacyPluginDir is where belt used to write goose's hooks. Removable
// once no install predating 2026-09 is in use.
const gooseLegacyPluginDir = ".agents/plugins/belt-test"

// KiroBeltAgentName is the agent belt installs its kiro hooks into. Install
// selects it with chat.defaultAgent, which both engines honour.
//
// Deprecated: a source's kiro agent is named after the source.
const KiroBeltAgentName = legacySourceName

// KiroSelectBeltAgent makes belt's agent the default in root's kiro settings
// when no other agent is: chat.defaultAgent unset, kiro_default, or already
// belt. It returns the other agent's name, untouched, when the user chose one.
//
// Deprecated: InstallSource selects the source's agent.
func KiroSelectBeltAgent(root string) (other string, err error) {
	return kiroSelectAgent(root, legacySourceName)
}
