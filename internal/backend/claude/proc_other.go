//go:build !unix

package claude

import "os/exec"

// setProcAttrs is a no-op on non-unix platforms; exec.CommandContext's
// default Kill of the direct child is the best available behavior there.
func setProcAttrs(cmd *exec.Cmd) {}
