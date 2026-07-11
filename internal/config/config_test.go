package config

import (
	"testing"
	"time"

	"github.com/s-celles/agent-relay/internal/core"
)

func validLoopback() Config {
	return Config{
		BindAddr:       "127.0.0.1:18082",
		Backend:        "claude",
		MaxConcurrent:  10,
		RequestTimeout: 5 * time.Minute,
	}
}

// Truth table for the startup guards (REQ-NET-02, REQ-EXEC-06, REQ-CFG-05).
// There must be no configuration in which an unauthenticated caller on a
// non-loopback interface reaches a backend (NFR-SEC-01).
func TestValidateTruthTable(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"loopback without tokens is allowed", func(c *Config) {}, false},
		{"loopback with tokens is allowed", func(c *Config) {
			c.Tokens = [][]byte{[]byte("secret")}
		}, false},
		{"non-loopback without tokens is refused", func(c *Config) {
			c.BindAddr = "100.64.0.5:18082"
		}, true},
		{"0.0.0.0 without tokens is refused", func(c *Config) {
			c.BindAddr = "0.0.0.0:18082"
		}, true},
		{"non-loopback with tokens is allowed", func(c *Config) {
			c.BindAddr = "100.64.0.5:18082"
			c.Tokens = [][]byte{[]byte("secret")}
		}, false},
		{"agentic on loopback without per-request authz is allowed", func(c *Config) {
			c.Agentic.Enabled = true
		}, false},
		{"agentic on non-loopback without per-request authz is refused", func(c *Config) {
			c.BindAddr = "100.64.0.5:18082"
			c.Tokens = [][]byte{[]byte("secret")}
			c.Agentic.Enabled = true
		}, true},
		{"agentic on non-loopback with per-request authz is allowed", func(c *Config) {
			c.BindAddr = "100.64.0.5:18082"
			c.Tokens = [][]byte{[]byte("secret")}
			c.Agentic.Enabled = true
			c.Agentic.PerRequestAuthz = true
		}, false},
		{"empty backend is refused", func(c *Config) { c.Backend = "" }, true},
		{"zero max concurrent is refused", func(c *Config) { c.MaxConcurrent = 0 }, true},
		{"empty bind addr is refused", func(c *Config) { c.BindAddr = "" }, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validLoopback()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("Validate: expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate: unexpected error: %v", err)
			}
		})
	}
}

func TestIsLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:18082", true},
		{"[::1]:18082", true},
		{"localhost:8080", true},
		{"0.0.0.0:18082", false},
		{"100.64.0.5:18082", false},
		{"192.168.1.10:80", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isLoopback(tc.addr); got != tc.want {
			t.Errorf("isLoopback(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestFromEnvDefaults(t *testing.T) {
	cfg, err := FromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.BindAddr != "127.0.0.1:18082" {
		t.Errorf("BindAddr = %q, want default loopback (REQ-NET-01)", cfg.BindAddr)
	}
	if cfg.Backend != "claude" {
		t.Errorf("Backend = %q, want claude", cfg.Backend)
	}
	if cfg.MaxConcurrent != 10 {
		t.Errorf("MaxConcurrent = %d, want 10 (REQ-PROC-02)", cfg.MaxConcurrent)
	}
	if cfg.Agentic.Enabled {
		t.Error("Agentic must be disabled by default (REQ-EXEC-01)")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("default config must validate: %v", err)
	}
}

func TestFromEnvParsing(t *testing.T) {
	env := map[string]string{
		"RELAY_BIND":             "100.64.0.5:9000",
		"RELAY_TOKENS":           "tok-a,tok-b",
		"RELAY_MAX_CONCURRENT":   "3",
		"RELAY_REQUEST_TIMEOUT":  "90s",
		"RELAY_CLAUDE_CLI":       "/opt/bin/claude",
		"RELAY_CLAUDE_MODEL_MAP": "sonnet=claude-sonnet-5,haiku=claude-haiku-4-5",
		"RELAY_ENV_DENY":         "MY_SECRET,OTHER_SECRET",
	}
	cfg, err := FromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.BindAddr != "100.64.0.5:9000" {
		t.Errorf("BindAddr = %q", cfg.BindAddr)
	}
	if len(cfg.Tokens) != 2 || string(cfg.Tokens[0]) != "tok-a" || string(cfg.Tokens[1]) != "tok-b" {
		t.Errorf("Tokens = %q", cfg.Tokens)
	}
	if cfg.MaxConcurrent != 3 {
		t.Errorf("MaxConcurrent = %d", cfg.MaxConcurrent)
	}
	if cfg.RequestTimeout != 90*time.Second {
		t.Errorf("RequestTimeout = %v", cfg.RequestTimeout)
	}
	bc, ok := cfg.Backends["claude"]
	if !ok {
		t.Fatal("missing claude backend config")
	}
	if bc.CLIPath != "/opt/bin/claude" {
		t.Errorf("CLIPath = %q", bc.CLIPath)
	}
	if bc.ModelMap["sonnet"] != "claude-sonnet-5" || bc.ModelMap["haiku"] != "claude-haiku-4-5" {
		t.Errorf("ModelMap = %v", bc.ModelMap)
	}
	if len(bc.EnvDeny) != 2 || bc.EnvDeny[0] != "MY_SECRET" {
		t.Errorf("EnvDeny = %v", bc.EnvDeny)
	}
}

func TestFromEnvInvalidValues(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"bad max concurrent": {"RELAY_MAX_CONCURRENT": "not-a-number"},
		"bad timeout":        {"RELAY_REQUEST_TIMEOUT": "soon"},
		"bad model map":      {"RELAY_CLAUDE_MODEL_MAP": "missing-equals"},
	} {
		t.Run(name, func(t *testing.T) {
			e := env
			if _, err := FromEnv(func(k string) string { return e[k] }); err == nil {
				t.Fatal("FromEnv: expected error, got nil (REQ-CFG-05 fail fast)")
			}
		})
	}
}

var _ = core.BackendConfig{} // config builds on the core types
