package harness

import "strings"

// The tests install hooks under belt's name, the way go/cli does, since
// belt's files are what the per-agent behaviour was measured with.
const (
	beltName      = "belt"
	gooseBeltDir  = ".agents/plugins/" + beltName
	kiroBeltAgent = beltName
)

func beltCmd(e HookEvent) string { return "belt plugin hook " + string(e) }

var beltTestSource = PluginSource{
	Name:        beltName,
	HookCommand: beltCmd,
	Owns:        func(cmd string) bool { return strings.Contains(cmd, "belt plugin hook") },
}

func installBelt(name string, scope InstallScope) InstallResult {
	return InstallSource(name, scope, beltTestSource)
}

func uninstallBelt(name string, scope InstallScope) InstallResult {
	return UninstallSource(name, scope, beltTestSource)
}

func beltHooksInstalled(name string, scope InstallScope) bool {
	return SourceHooksInstalled(name, scope, beltTestSource)
}

func beltHookConfig(name string) (string, error) { return hookConfigFor(name, beltCmd) }

func hookConfigFor(name string, cmd HookCommand) (string, error) {
	src := beltTestSource
	src.HookCommand = cmd
	s, _, err := RenderHooks(name, src)
	return s, err
}

func beltHookPath(name string) string { return All[name].HookPath(beltName) }
