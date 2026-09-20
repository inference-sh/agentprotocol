package harness

// productNames gives every registry entry its product name and vendor. The
// registry's keys are ids — short, stable, typed by users and exported to
// hook scripts — and "claude" is the id of the product called Claude Code.
// Humans should read the product name, so anything that shows an agent to a
// person uses DisplayName, and the id stays what it is.
//
// Kept in one table beside the registry so the test can require that every
// entry has one and no name refers to an entry that does not exist.
var productNames = map[string]struct{ Display, Vendor string }{
	"claude":   {"Claude Code", "Anthropic"},
	"codex":    {"Codex CLI", "OpenAI"},
	"copilot":  {"GitHub Copilot CLI", "GitHub"},
	"cursor":   {"Cursor Agent", "Cursor"},
	"droid":    {"Droid", "Factory"},
	"gemini":   {"Gemini CLI", "Google"},
	"goose":    {"Goose", "Block"},
	"grok":     {"Grok CLI", "xAI"},
	"hermes":   {"Hermes Agent", "Nous Research"},
	"kilo":     {"Kilo Code", "Kilo Code"},
	"kimi":     {"Kimi Code", "Moonshot AI"},
	"kiro":     {"Kiro CLI", "AWS"},
	"omp":      {"Oh My Pi", "Oh My Pi"},
	"opencode": {"OpenCode", "SST"},
	"pi":       {"Pi Coding Agent", "Earendil Works"},
	"qwen":     {"Qwen Code", "Alibaba"},
	"windsurf": {"Windsurf", "Cognition"},
}

// init stamps the names onto the entries the same way the other side tables
// do; decorate panics on a name the registry does not know. Package-level
// variables are initialised before any init runs, so All is complete here
// whichever file's init goes first.
func init() {
	for id, n := range productNames {
		decorate("productNames", id, func(h *Harness) {
			h.DisplayName = n.Display
			h.Vendor = n.Vendor
		})
	}
}

// Display returns the product name for an id, or the id itself when it is
// not in the registry — a caller showing an unknown harness still shows
// something readable.
func Display(id string) string {
	if h, ok := All[id]; ok && h.DisplayName != "" {
		return h.DisplayName
	}
	return id
}
