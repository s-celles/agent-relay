// Package config loads and validates the relay configuration. Env-first
// (12-factor, container-friendly); loaded once, validated, then immutable.
package config

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/s-celles/agent-relay/internal/core"
)

type Config struct {
	BindAddr       string   // default "127.0.0.1:18082" (REQ-NET-01)
	Tokens         [][]byte // caller bearer tokens (REQ-AUTH-03)
	Backend        string   // "claude"
	Backends       map[string]core.BackendConfig
	MaxConcurrent  int // default 10 (REQ-PROC-02)
	RequestTimeout time.Duration
	Agentic        core.AgenticConfig // disabled by default (REQ-EXEC-01)
	// AgenticTokens authorize individual requests for agentic execution when
	// Agentic.PerRequestAuthz is set (REQ-EXEC-06). Keep them distinct from
	// the caller Tokens.
	AgenticTokens [][]byte
	LogLevel      string
}

// FromEnv builds a Config from environment variables. getenv is injectable
// for tests; pass os.Getenv in production. Parse failures are fatal
// (REQ-CFG-05: fail fast, never limp along on a half-read config).
func FromEnv(getenv func(string) string) (Config, error) {
	get := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}

	cfg := Config{
		BindAddr: get("RELAY_BIND", "127.0.0.1:18082"),
		Backend:  get("RELAY_BACKEND", "claude"),
		LogLevel: get("RELAY_LOG_LEVEL", "info"),
	}

	for _, t := range splitCSV(getenv("RELAY_TOKENS")) {
		cfg.Tokens = append(cfg.Tokens, []byte(t))
	}

	var err error
	if cfg.MaxConcurrent, err = strconv.Atoi(get("RELAY_MAX_CONCURRENT", "10")); err != nil {
		return Config{}, fmt.Errorf("RELAY_MAX_CONCURRENT: %w", err)
	}
	if cfg.RequestTimeout, err = time.ParseDuration(get("RELAY_REQUEST_TIMEOUT", "10m")); err != nil {
		return Config{}, fmt.Errorf("RELAY_REQUEST_TIMEOUT: %w", err)
	}

	cfg.Agentic = core.AgenticConfig{
		Enabled:         getenv("RELAY_AGENTIC_ENABLED") == "true",
		PerRequestAuthz: getenv("RELAY_AGENTIC_PER_REQUEST_AUTHZ") == "true",
		ExtraArgs:       splitCSV(getenv("RELAY_AGENTIC_ARGS")),
	}
	for _, t := range splitCSV(getenv("RELAY_AGENTIC_TOKENS")) {
		cfg.AgenticTokens = append(cfg.AgenticTokens, []byte(t))
	}

	modelMap, err := parseModelMap(getenv("RELAY_CLAUDE_MODEL_MAP"))
	if err != nil {
		return Config{}, err
	}
	cfg.Backends = map[string]core.BackendConfig{
		"claude": {
			CLIPath:  get("RELAY_CLAUDE_CLI", "claude"),
			Workdir:  getenv("RELAY_CLAUDE_WORKDIR"),
			ModelMap: modelMap,
			EnvDeny:  splitCSV(getenv("RELAY_ENV_DENY")),
			Agentic:  cfg.Agentic,
		},
	}
	return cfg, nil
}

// Validate encodes the anti-goals as invariants. There is, by construction,
// no config in which an unauthenticated caller on a non-loopback interface
// reaches a backend (NFR-SEC-01).
func (c Config) Validate() error {
	if c.BindAddr == "" {
		return errors.New("no bind address configured")
	}
	if !isLoopback(c.BindAddr) && len(c.Tokens) == 0 {
		return errors.New("refusing non-loopback bind without auth (REQ-NET-02)")
	}
	if c.Agentic.Enabled && !isLoopback(c.BindAddr) && !c.Agentic.PerRequestAuthz {
		return errors.New("refusing agentic mode on non-loopback bind without per-request authz (REQ-EXEC-06)")
	}
	if c.Agentic.Enabled && c.Agentic.PerRequestAuthz && len(c.AgenticTokens) == 0 {
		return errors.New("per-request agentic authz requires RELAY_AGENTIC_TOKENS (REQ-EXEC-06)")
	}
	if c.Backend == "" {
		return errors.New("no backend configured")
	}
	if c.MaxConcurrent < 1 {
		return errors.New("max concurrent requests must be >= 1 (REQ-PROC-02)")
	}
	return nil
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseModelMap(s string) (map[string]string, error) {
	pairs := splitCSV(s)
	if len(pairs) == 0 {
		return nil, nil
	}
	m := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		logical, backend, ok := strings.Cut(pair, "=")
		if !ok || logical == "" || backend == "" {
			return nil, fmt.Errorf("RELAY_CLAUDE_MODEL_MAP: bad entry %q (want logical=backend)", pair)
		}
		m[logical] = backend
	}
	return m, nil
}
