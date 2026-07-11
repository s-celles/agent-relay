package core

import (
	"fmt"
	"sync"
)

// AgenticConfig gates host-side agentic execution. Disabled by default
// (REQ-EXEC-01); enabling it is an explicit, logged operator decision.
type AgenticConfig struct {
	Enabled         bool
	PerRequestAuthz bool
	// ExtraArgs are the operator-chosen permission flags appended to the
	// backend CLI invocation when agentic mode is enabled (REQ-EXEC-02).
	ExtraArgs []string
}

// PermissionArgs returns the CLI flags for agentic mode; empty when disabled.
func (a AgenticConfig) PermissionArgs() []string {
	if !a.Enabled {
		return nil
	}
	return a.ExtraArgs
}

// BackendConfig carries per-backend settings from configuration to a Factory.
// Not every backend is a subprocess: CLIPath/Workdir/EnvDeny/Agentic are for
// CLI adapters, BaseURL for HTTP ones.
type BackendConfig struct {
	CLIPath  string
	BaseURL  string
	Workdir  string
	ModelMap map[string]string // logical name -> backend model id
	EnvDeny  []string          // extra env keys stripped from the subprocess
	Agentic  AgenticConfig
}

type Factory func(cfg BackendConfig) (Backend, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register makes a backend factory available under name. Adapters call it
// from an init(); adding a new agent CLI is one new package, nothing else
// (REQ-BK-03).
func Register(name string, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = f
}

// New instantiates the named backend.
func New(name string, cfg BackendConfig) (Backend, error) {
	registryMu.RLock()
	f, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown backend %q", name)
	}
	return f(cfg)
}
