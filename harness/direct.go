package harness

// Direct is how a harness is launched to serve as a model in direct mode
// (package llm), as opposed to running as itself: the caller's system prompt
// in place of the harness's own, none of its built-in tools, no settings,
// rules, skills, memory or context files of its own, and the caller's tools
// from an MCP server the caller serves for the session. The harness never
// runs a tool: the server holds each call until the caller has its result.
//
// A nil Direct means the harness has no measured way to be launched so;
// Claude Code is driven natively by llm.Claude and has none here. Each field
// is a measurement of the version in Measured, made by harness-test's
// direct probe; a flag that only appears in --help is not enough.
type Direct struct {
	// Args are appended to the session launch command. They may use
	// {{.SystemPrompt}}, the prompt's text; {{.SystemPromptFile}}, a file
	// holding it, for an agent whose flag takes a path; {{.Dir}}, a
	// directory of the conversation's own outside its working directory,
	// where Files are written; and {{.MCPURL}} and {{.MCPToken}}, the tool
	// server's address and the bearer token it wants.
	Args []string

	// Env is set on the child, with the same placeholders.
	Env map[string]string

	// Files are written into {{.Dir}} before launch, by name, their
	// contents with the same placeholders: a settings file an agent is
	// pointed at with a flag or variable. A name may use the placeholders
	// too, and {{.WorkDir}}, the session's working directory, for a
	// settings file an agent reads only from its project.
	Files map[string]string

	// MCPInArgs reports that Args or Files declare the tool server, so it
	// is not also given to the session (ACP session/new). For an agent
	// whose session MCP servers are dropped in the mode Args select.
	MCPInArgs bool

	// ToolName is the name the model is given for one of the caller's tools,
	// with {{.Server}} and {{.Tool}}: how the harness namespaces MCP tools.
	// Empty is the tool's own name. History a restart writes names the
	// caller's tool calls the same way, so they match the tools offered.
	ToolName string

	// ContinuePrompt is sent after a restart whose written session ends in
	// tool results, for an agent that will not start a turn without a
	// prompt. Empty is "Continue.".
	ContinuePrompt string

	// Measured is the version and date the direct probe passed on.
	Measured string
}
