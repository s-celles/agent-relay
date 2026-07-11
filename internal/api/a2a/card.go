package a2a

import (
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// Skill ids on the Agent Card. A peer does not name a skill on the wire — it
// just asks — so these exist so a registry, or a human, can tell what this
// agent is for, and so that a relay with agentic execution off does not
// advertise work it will refuse.
const (
	SkillChat        = "chat"
	SkillAgenticTask = "agentic-task"
)

// CardConfig is what the relay knows about itself when it publishes its card.
type CardConfig struct {
	BaseURL string   // public origin, e.g. https://relay.example.com
	Version string   // relay version
	Models  []string // logical model names this relay serves
	Agentic bool     // agentic execution is enabled on this relay
}

// NewAgentCard describes this relay to A2A peers. It advertises only what the
// relay actually does: the JSON-RPC binding (gRPC and HTTP+JSON are optional in
// the spec and not served here), streaming, and — only when the operator turned
// it on — agentic execution.
//
// The card is public by design: discovery is the point. Keep that in mind when
// enabling A2A, since the card names the models served and says whether the
// relay can run an agent on its host.
func NewAgentCard(cfg CardConfig) *a2a.AgentCard {
	desc := "Exposes an agent CLI (and any routed local model) as an A2A agent."
	if len(cfg.Models) > 0 {
		desc += " Models: " + strings.Join(cfg.Models, ", ") +
			" — select one with message.metadata.model."
	}
	version := cfg.Version
	if version == "" {
		version = "0.0.0"
	}

	skills := []a2a.AgentSkill{{
		ID:          SkillChat,
		Name:        "Chat",
		Description: "Answer a question or transform text. Images and PDFs may be attached as raw parts.",
		Tags:        []string{"chat", "text", "vision"},
		Examples:    []string{"Summarize this changelog in three bullets."},
		InputModes:  []string{"text/plain", "image/png", "application/pdf"},
		OutputModes: []string{"text/plain"},
	}}
	if cfg.Agentic {
		skills = append(skills, a2a.AgentSkill{
			ID:   SkillAgenticTask,
			Name: "Agentic task",
			Description: "Run a task in a private workspace on the relay host — the agent reads, writes and runs " +
				"commands there — and return the files it produced as artifacts. Requires an agentic credential " +
				"(X-Agentic-Authorization); the workspace persists for the life of the A2A context.",
			Tags:        []string{"agentic", "files", "code"},
			Examples:    []string{"Write a Python script that plots this CSV, run it, and return the chart."},
			InputModes:  []string{"text/plain"},
			OutputModes: []string{"text/plain", "application/octet-stream"},
		})
	}

	return &a2a.AgentCard{
		Name:        "agent-relay",
		Description: desc,
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(strings.TrimSuffix(cfg.BaseURL, "/")+"/a2a", a2a.TransportProtocolJSONRPC),
		},
		Version:            version,
		Capabilities:       a2a.AgentCapabilities{Streaming: true},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		SecuritySchemes: a2a.NamedSecuritySchemes{
			"bearer": a2a.HTTPAuthSecurityScheme{
				Scheme:      "Bearer",
				Description: "A relay caller token (RELAY_TOKENS).",
			},
		},
		SecurityRequirements: a2a.SecurityRequirementsOptions{
			{a2a.SecuritySchemeName("bearer"): {}},
		},
		Skills: skills,
	}
}
