package claude

import (
	"os"
	"strings"
)

// baseEnvDeny lists variables that must never reach the subprocess:
// base-url overrides would loop the CLI back through the relay itself, and
// CLAUDECODE leaks host session state (REQ-PROC-05 / REQ-PROC-07).
var baseEnvDeny = []string{
	"ANTHROPIC_BASE_URL",
	"OPENAI_BASE_URL",
	"CLAUDECODE",
}

// sanitizedEnv returns os.Environ() minus the baseline deny list and any
// operator-configured deny keys.
func (b *Backend) sanitizedEnv() []string {
	denied := make(map[string]bool, len(baseEnvDeny)+len(b.envDeny))
	for _, k := range baseEnvDeny {
		denied[k] = true
	}
	for _, k := range b.envDeny {
		denied[k] = true
	}

	env := os.Environ()
	out := env[:0]
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if !denied[key] {
			out = append(out, kv)
		}
	}
	return out
}
