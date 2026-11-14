package harness

import (
	"os"
	"sort"
)

// ACPEnviron is env with h.ACPEnv set in it, a nil env standing for the
// parent's environment. ACPEnv's values replace any already in env.
func (h Harness) ACPEnviron(env []string) []string {
	if len(h.ACPEnv) == 0 {
		return env
	}
	if env == nil {
		env = os.Environ()
	}
	keys := make([]string, 0, len(h.ACPEnv))
	for k := range h.ACPEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(env)+len(keys))
	for _, kv := range env {
		k := kv
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				k = kv[:i]
				break
			}
		}
		if _, set := h.ACPEnv[k]; !set {
			out = append(out, kv)
		}
	}
	for _, k := range keys {
		out = append(out, k+"="+h.ACPEnv[k])
	}
	return out
}
