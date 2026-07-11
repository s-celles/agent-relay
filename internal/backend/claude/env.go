package claude

import (
	"os"
	"strings"
)

// baseEnvDeny lists variables that must never reach the subprocess:
//   - base-url overrides would loop the CLI back through the relay itself;
//   - CLAUDECODE leaks host session state (REQ-PROC-05 / REQ-PROC-07);
//   - the Anthropic credentials would silently route the CLI to an API key
//     inherited from the operator's shell instead of the subscription the
//     relay exists to front — and a stale one fails every request with
//     "Invalid API key". Denying them by default is the relay doing the right
//     thing without an operator having to remember RELAY_ENV_DENY. An operator
//     who genuinely wants an API-key path does not need this relay.
var baseEnvDeny = []string{
	"ANTHROPIC_BASE_URL",
	"OPENAI_BASE_URL",
	"CLAUDECODE",
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
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
