package harness

import (
	"fmt"
	"time"
)

// Tool is a tool a hook can be matched on, named once for every agent. The
// agent's own name for it is a fact in the registry (Harness.ToolNames): the
// shell tool is "Bash" to claude and to codex, whose exec_command reaches
// hooks under that name (codex-rs/core/src/tools/hook_names.rs).
type Tool string

const (
	ToolShell Tool = "shell"
	ToolSkill Tool = "skill"
)

// HookSpec is what a plugin wants from one hook event: which tools it fires
// for (none: every tool), how long the agent may wait for it (zero: the
// format's default), and whether the agent may run it without waiting.
//
// A hook whose Tools cannot be matched fires for every tool instead, unless
// Strict: then it is not installed. Strict is for a handler that does
// nothing for other tools, where firing on every call is only cost.
type HookSpec struct {
	Tools   []Tool
	Timeout time.Duration
	Async   bool
	Strict  bool
}

// PluginSource is the caller's side of an install: the command each event
// runs and what it wants from each event. The registry holds what each agent
// can do; nothing about a particular plugin lives in this package.
type PluginSource struct {
	Name        string
	HookCommand HookCommand
	// Hooks names the events to install and how. Nil installs every event
	// the agent has, as DefaultHooks does.
	Hooks map[HookEvent]HookSpec
}

// DefaultHooks is every event with no tool filter and each format's default
// timeout: the shape Install wrote before plugins could ask for more.
func DefaultHooks() map[HookEvent]HookSpec {
	out := map[HookEvent]HookSpec{}
	for _, e := range HookEvents() {
		out[e] = HookSpec{}
	}
	return out
}

// HookNote is something a source asked for that an agent's config cannot
// carry, and what was written instead. The config still works; it does more
// or waits longer than asked.
type HookNote struct {
	Event  HookEvent
	Detail string
}

func (n HookNote) String() string { return fmt.Sprintf("%s: %s", n.Event, n.Detail) }

// plannedHook is one event as a format writes it.
type plannedHook struct {
	Event    HookEvent
	Name     string   // the agent's name for the event
	Matchers []string // agent tool names; empty matches every tool
	Timeout  time.Duration
	Async    bool
}

// matcherFormats are the formats whose entries take a tool matcher.
func supportsMatcher(f HookFormat) bool { return f == JSONNested || f == TOML }

// planHooks resolves a source's specs against what the agent supports. Every
// shortfall is a note: an event the agent lacks, a tool it has no name for, a
// matcher or async its format cannot express.
func planHooks(h Harness, src PluginSource) ([]plannedHook, []HookNote) {
	specs := src.Hooks
	if specs == nil {
		specs = DefaultHooks()
	}
	var plan []plannedHook
	var notes []HookNote
	for _, e := range HookEvents() {
		spec, wanted := specs[e]
		if !wanted {
			continue
		}
		name := e.AgentName(h)
		if name == "" {
			notes = append(notes, HookNote{e, h.Name + " has no such hook; not installed"})
			continue
		}
		p := plannedHook{Event: e, Name: name, Timeout: spec.Timeout}
		if len(spec.Tools) > 0 && !e.IsToolEvent() {
			notes = append(notes, HookNote{e, "not a tool event; tool filter ignored"})
		} else if len(spec.Tools) > 0 {
			unmatched := ""
			if !supportsMatcher(h.HookFormat) {
				unmatched = fmt.Sprintf("%s hooks take no tool matcher", h.HookFormat)
			}
			for _, t := range spec.Tools {
				if unmatched != "" {
					break
				}
				n, ok := h.ToolNames[t]
				if !ok {
					unmatched = fmt.Sprintf("no %s tool name known for %s", t, h.Name)
					break
				}
				p.Matchers = append(p.Matchers, n)
			}
			if unmatched != "" {
				p.Matchers = nil
				if spec.Strict {
					notes = append(notes, HookNote{e, unmatched + "; not installed"})
					continue
				}
				notes = append(notes, HookNote{e, unmatched + "; fires for every tool"})
			}
		}
		if spec.Async {
			if h.HookAsync {
				p.Async = true
			} else {
				notes = append(notes, HookNote{e, h.Name + " cannot run a hook without waiting; runs in line"})
			}
		}
		plan = append(plan, p)
	}
	return plan, notes
}

// timeoutIn renders a timeout in the unit a format counts in, rounding up so
// a hook is never given less than asked. Zero is the format's default.
func timeoutIn(d time.Duration, unit time.Duration, def int) int {
	if d <= 0 {
		return def
	}
	return int((d + unit - 1) / unit)
}

// RenderHooks is the hook configuration for an agent from a plugin source,
// with a note for everything the source asked for that the agent's config
// could not carry.
func RenderHooks(name string, src PluginSource) (string, []HookNote, error) {
	h, ok := All[name]
	if !ok {
		return "", nil, fmt.Errorf("unknown harness: %s", name)
	}
	if src.HookCommand == nil {
		return "", nil, fmt.Errorf("plugin source %q has no hook command", src.Name)
	}
	plan, notes := planHooks(h, src)
	content, err := generateHookConfig(name, h, plan, src.HookCommand)
	return content, notes, err
}
