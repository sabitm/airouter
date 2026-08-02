package thinking

import (
	"strings"

	"airouter/internal/domain"
)

// Format selects the native thinking wire shape for a backend.
type Format int

const (
	FormatNone Format = iota
	FormatOpenAI
	FormatClaudeBudget
	FormatClaudeAdaptive
	FormatCursor
)

// Caps describes how a model/protocol pair handles thinking.
type Caps struct {
	Reasoning  bool
	CanDisable bool
	Format     Format
	// BudgetMin/Max clamp budget formats; zero means no clamp on that side.
	BudgetMin int
	BudgetMax int
}

// CapsFor returns thinking capabilities for an upstream model under protocol.
// The adaptive Claude patterns and codex CanDisable=false are stated assumptions
// ported from 9router; expand the table when live probes contradict it.
func CapsFor(model string, protocol domain.Protocol) Caps {
	m := strings.ToLower(strings.TrimSpace(model))
	switch protocol {
	case domain.ProtocolOpenAI, domain.ProtocolOpenAIResponses:
		c := Caps{Reasoning: true, CanDisable: true, Format: FormatOpenAI}
		if strings.Contains(m, "codex") {
			c.CanDisable = false
		}
		return c
	case domain.ProtocolOpenAICodex:
		return Caps{Reasoning: true, CanDisable: false, Format: FormatOpenAI}
	case domain.ProtocolAnthropic, domain.ProtocolClaudeCode:
		c := Caps{
			Reasoning:  true,
			CanDisable: true,
			Format:     FormatClaudeBudget,
			BudgetMin:  1024,
			BudgetMax:  128000,
		}
		if claudeAdaptiveModel(m) || protocol == domain.ProtocolClaudeCode {
			// Claude Code targets current adaptive models; always adaptive shape.
			c.Format = FormatClaudeAdaptive
		}
		return c
	case domain.ProtocolCursor:
		return Caps{Reasoning: true, CanDisable: true, Format: FormatCursor}
	default:
		// Kiro, Qoder, Antigravity: no request-side writer in v1.
		return Caps{Reasoning: false, CanDisable: true, Format: FormatNone}
	}
}

func claudeAdaptiveModel(m string) bool {
	// Opus 4.6+ and Sonnet 4.6+ require thinking:{type:adaptive}+output_config.
	patterns := []string{
		"claude-opus-4.6", "claude-opus-4-6",
		"claude-opus-4.7", "claude-opus-4-7",
		"claude-opus-4.8", "claude-opus-4-8",
		"claude-opus-5",
		"claude-sonnet-4.6", "claude-sonnet-4-6",
		"claude-sonnet-4.7", "claude-sonnet-4-7",
		"claude-sonnet-5",
	}
	for _, p := range patterns {
		if strings.Contains(m, p) {
			return true
		}
	}
	return false
}
