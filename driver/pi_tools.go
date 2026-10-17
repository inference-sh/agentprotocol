package driver

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/inference-sh/agentprotocol/internal/mcptools"
)

// piToolsExtension is the pi extension that registers the caller's tools. It
// lists them from the session's loopback MCP endpoint at load and sends each
// call there; see the file's own comment.
//
//go:embed piext/tools.js
var piToolsExtension []byte

// The variables that give the extension its endpoint. The token is never on
// pi's command line, where any local user could read it.
const (
	piToolsURLEnv   = "AGENTPROTOCOL_PI_TOOLS_URL"
	piToolsTokenEnv = "AGENTPROTOCOL_PI_TOOLS_TOKEN"
)

// piCallerTools is what one session needs for SessionConfig.Tools: the
// loopback serving them and the temp dir holding the extension.
type piCallerTools struct {
	loopback *mcptools.Loopback
	dir      string
	ext      string
	once     sync.Once
}

// startPiCallerTools serves cfg.Tools on loopback and writes the extension.
func startPiCallerTools(cfg SessionConfig) (*piCallerTools, error) {
	if cfg.OnlyCallerTools {
		// --tools takes a comma-separated list.
		for _, t := range cfg.Tools {
			if strings.ContainsAny(t.Function.Name, ", \t\n") {
				return nil, fmt.Errorf("driver: pi cannot allowlist tool %q: its name has a comma or a space", t.Function.Name)
			}
		}
	}
	server, err := callerToolServer(cfg.Tools, cfg.CallTool)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "agentprotocol-pi-tools-")
	if err != nil {
		return nil, fmt.Errorf("driver: pi caller tools: %w", err)
	}
	t := &piCallerTools{dir: dir, ext: filepath.Join(dir, "agentprotocol-tools.js")}
	if err := os.WriteFile(t.ext, piToolsExtension, 0o600); err != nil {
		t.close()
		return nil, fmt.Errorf("driver: pi caller tools: %w", err)
	}
	if t.loopback, err = mcptools.Listen(server); err != nil {
		t.close()
		return nil, fmt.Errorf("driver: pi caller tools: %w", err)
	}
	return t, nil
}

// args are pi's flags for the extension, and with only it switches every
// other tool off. --tools is an allowlist over built-in, extension and custom
// tools alike (pi 0.68.0 on), and pi drops what it does not allow from the
// tool registry, so neither a user's extension nor setActiveTools can bring
// one back. --no-builtin-tools says the same for the built-ins.
func (t *piCallerTools) args(cfg SessionConfig) []string {
	args := []string{"--extension", t.ext}
	if cfg.OnlyCallerTools {
		names := make([]string, len(cfg.Tools))
		for i, tool := range cfg.Tools {
			names[i] = tool.Function.Name
		}
		args = append(args, "--no-builtin-tools", "--tools", strings.Join(names, ","))
	}
	return args
}

// env is base, or the parent's environment when base is nil, with the
// endpoint added.
func (t *piCallerTools) env(base []string) []string {
	if base == nil {
		base = os.Environ()
	}
	env := make([]string, 0, len(base)+2)
	for _, kv := range base {
		if !strings.HasPrefix(kv, piToolsURLEnv+"=") && !strings.HasPrefix(kv, piToolsTokenEnv+"=") {
			env = append(env, kv)
		}
	}
	return append(env, piToolsURLEnv+"="+t.loopback.URL, piToolsTokenEnv+"="+t.loopback.Token)
}

// close stops the endpoint, which ends calls still running, and removes the
// extension. Safe on nil and more than once.
func (t *piCallerTools) close() error {
	if t == nil {
		return nil
	}
	var err error
	t.once.Do(func() {
		err = errors.Join(t.loopback.Close(), os.RemoveAll(t.dir))
	})
	return err
}
