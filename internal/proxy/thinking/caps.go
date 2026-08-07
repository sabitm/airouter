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
	FormatOpenAIResponses // reasoning.effort object (Responses / Codex)
	FormatClaudeBudget
	FormatClaudeAdaptive
	FormatKimi
	FormatQwen
	FormatDeepSeek
	FormatZAI
	FormatGrok
	FormatCursor
)

// Caps describes how a model under a provider dialect handles thinking.
type Caps struct {
	Reasoning  bool
	CanDisable bool
	Format     Format
	// BudgetMin/Max clamp budget formats; zero means no clamp on that side.
	BudgetMin int
	BudgetMax int
	// MaxOutput is a model output ceiling used when reconciling budget vs max_tokens.
	MaxOutput int
	// Levels lists accepted discrete effort values for this model/dialect.
	// Empty means use the format's default set.
	Levels []string
	// AllowMax permits the "max" effort on OpenAI-family writers (else clamp to xhigh).
	AllowMax bool
	// RequiredDefault, when non-empty, is injected when the caller expressed no
	// intent (Codex default-low). Empty means no injection without intent.
	RequiredDefault string
}

// CapsFor resolves thinking capabilities from the provider's effective dialect,
// transport protocol, and model-name refinements. Broad semantics come from the
// dialect; the model only narrows support, levels, and adaptive-vs-budget.
func CapsFor(model string, protocol domain.Protocol, dialect domain.ReasoningDialect) Caps {
	m := strings.ToLower(strings.TrimSpace(model))
	// Strip vendor prefix: "anthropic/claude-opus-4.7" -> "claude-opus-4.7".
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}

	// Protocol-managed backends keep native behavior outside the generic writer.
	switch protocol {
	case domain.ProtocolCursor:
		return Caps{Reasoning: true, CanDisable: true, Format: FormatCursor}
	case domain.ProtocolKiro, domain.ProtocolQoder, domain.ProtocolAntigravity:
		return Caps{Reasoning: false, CanDisable: true, Format: FormatNone}
	}

	// Explicit none disables the generic writer.
	if dialect == domain.ReasoningNone {
		return Caps{Reasoning: false, CanDisable: true, Format: FormatNone}
	}

	switch dialect {
	case domain.ReasoningOpenAI:
		return openaiCaps(m, protocol)
	case domain.ReasoningCodex:
		return codexCaps(m)
	case domain.ReasoningClaude:
		return claudeCaps(m, protocol)
	case domain.ReasoningKimi:
		return kimiCaps(m)
	case domain.ReasoningQwen:
		return qwenCaps(m)
	case domain.ReasoningDeepSeek:
		return deepseekCaps(m)
	case domain.ReasoningZAI:
		return zaiCaps(m)
	case domain.ReasoningGrok:
		return grokCaps(m, protocol)
	default:
		return Caps{Reasoning: false, CanDisable: true, Format: FormatNone}
	}
}

func openaiCaps(m string, protocol domain.Protocol) Caps {
	fmt := FormatOpenAI
	if protocol == domain.ProtocolOpenAIResponses || protocol == domain.ProtocolOpenAICodex {
		fmt = FormatOpenAIResponses
	}
	c := Caps{
		Reasoning:  true,
		CanDisable: true,
		Format:     fmt,
		Levels:     []string{"none", "minimal", "low", "medium", "high", "xhigh"},
		MaxOutput:  128000,
	}
	// Classic codex-named models on OpenAI transport cannot disable.
	if strings.Contains(m, "codex") {
		c.CanDisable = false
		c.Levels = []string{"low", "medium", "high", "xhigh"}
	}
	// Known non-reasoners: plain gpt-4o family without reasoning.
	if isKnownNonReasoner(m) {
		c.Reasoning = false
		c.Format = FormatNone
	}
	// Models that accept "max" natively (rare; most clamp to xhigh).
	if allowsOpenAIMax(m) {
		c.AllowMax = true
		c.Levels = append(c.Levels, "max")
	}
	return c
}

func codexCaps(m string) Caps {
	c := Caps{
		Reasoning:       true,
		CanDisable:      false,
		Format:          FormatOpenAIResponses,
		Levels:          []string{"low", "medium", "high", "xhigh"},
		RequiredDefault: "low",
		MaxOutput:       128000,
	}
	// Newer Codex 5.6 variants accept none/minimal/max (and ultra on sol/terra).
	if strings.Contains(m, "gpt-5.6-sol") || strings.Contains(m, "gpt-5.6-terra") {
		c.CanDisable = true
		c.AllowMax = true
		c.Levels = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}
	} else if strings.Contains(m, "gpt-5.6-luna") || strings.Contains(m, "gpt-5.6") {
		c.CanDisable = true
		c.AllowMax = true
		c.Levels = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}
	}
	return c
}

func claudeCaps(m string, protocol domain.Protocol) Caps {
	c := Caps{
		Reasoning:  true,
		CanDisable: true,
		Format:     FormatClaudeBudget,
		BudgetMin:  1024,
		BudgetMax:  128000,
		MaxOutput:  64000,
		Levels:     []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"},
	}
	// Adaptive for Opus/Sonnet 4.6+ (and 5.x). Haiku/Fable/Mythos stay budget.
	// Claude Code uses the same model refinement (no longer all-adaptive).
	if claudeAdaptiveModel(m) {
		c.Format = FormatClaudeAdaptive
		c.MaxOutput = 128000
		// Adaptive effort set: none/low/medium/high/max (minimal->low, xhigh->high).
		c.Levels = []string{"none", "low", "medium", "high", "max"}
	} else if strings.Contains(m, "fable") || strings.Contains(m, "mythos") {
		c.MaxOutput = 128000
	}
	if strings.Contains(m, "claude-3") {
		c.Reasoning = false
		c.Format = FormatNone
	}
	_ = protocol
	return c
}

func kimiCaps(m string) Caps {
	c := Caps{
		Reasoning:  true,
		CanDisable: true,
		Format:     FormatKimi,
		Levels:     []string{"none", "low", "medium", "high", "max"},
		MaxOutput:  65536,
	}
	// K2.7-code / K3 / for-coding cannot disable.
	if strings.Contains(m, "k2.7") && strings.Contains(m, "code") ||
		strings.Contains(m, "kimi-k3") || m == "k3" ||
		strings.Contains(m, "for-coding") {
		c.CanDisable = false
	}
	return c
}

func qwenCaps(m string) Caps {
	c := Caps{
		Reasoning:  true,
		CanDisable: true,
		Format:     FormatQwen,
		BudgetMin:  0,
		BudgetMax:  128000,
		Levels:     []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"},
		MaxOutput:  65536,
	}
	if strings.Contains(m, "qwq") {
		c.CanDisable = false
	}
	return c
}

func deepseekCaps(m string) Caps {
	c := Caps{
		Reasoning:  true,
		CanDisable: true,
		Format:     FormatDeepSeek,
		Levels:     []string{"none", "high", "max"},
		MaxOutput:  128000,
	}
	// reasoner / r1 family cannot disable.
	if strings.Contains(m, "reasoner") || strings.Contains(m, "deepseek-r") {
		c.CanDisable = false
	}
	if strings.Contains(m, "deepseek-chat") && !strings.Contains(m, "reasoner") {
		c.Reasoning = false
		c.Format = FormatNone
	}
	return c
}

func zaiCaps(m string) Caps {
	_ = m
	return Caps{
		Reasoning:  true,
		CanDisable: true,
		Format:     FormatZAI,
		Levels:     []string{"none", "thinking"},
		MaxOutput:  128000,
	}
}

func grokCaps(m string, protocol domain.Protocol) Caps {
	fmt := FormatGrok
	if protocol == domain.ProtocolOpenAIResponses {
		// Grok API on Responses still uses reasoning.effort when supported.
		fmt = FormatOpenAIResponses
	}
	c := Caps{
		Reasoning:  true,
		CanDisable: true,
		Format:     fmt,
		Levels:     []string{"none", "minimal", "low", "medium", "high", "xhigh"},
		MaxOutput:  128000,
	}
	// Known non-reasoners / image-only.
	if strings.Contains(m, "image") || strings.Contains(m, "imagine") {
		c.Reasoning = false
		c.Format = FormatNone
	}
	return c
}

func claudeAdaptiveModel(m string) bool {
	// Opus 4.6+ / Sonnet 4.6+ / 5.x require thinking:{type:adaptive}+output_config.
	// Haiku, Fable, Mythos, and older Opus/Sonnet stay on budget.
	patterns := []string{
		"claude-opus-4.6", "claude-opus-4-6",
		"claude-opus-4.7", "claude-opus-4-7",
		"claude-opus-4.8", "claude-opus-4-8",
		"claude-opus-5",
		"claude-sonnet-4.6", "claude-sonnet-4-6",
		"claude-sonnet-4.7", "claude-sonnet-4-7",
		"claude-sonnet-5",
		"opus-4.6", "opus-4-6",
		"opus-4.7", "opus-4-7",
		"opus-4.8", "opus-4-8",
		"opus-5",
		"sonnet-4.6", "sonnet-4-6",
		"sonnet-4.7", "sonnet-4-7",
		"sonnet-5",
	}
	// Exclude haiku even if a broader pattern somehow matched.
	if strings.Contains(m, "haiku") || strings.Contains(m, "fable") || strings.Contains(m, "mythos") {
		return false
	}
	for _, p := range patterns {
		if strings.Contains(m, p) {
			return true
		}
	}
	return false
}

func isKnownNonReasoner(m string) bool {
	patterns := []string{
		"gpt-image", "gpt-4o", "gpt-4.1", "gpt-4-turbo", "gpt-4", "gpt-3.5",
		"embedding", "whisper", "tts", "audio", "image",
	}
	for _, pattern := range patterns {
		if strings.Contains(m, pattern) {
			return true
		}
	}
	return false
}

func allowsOpenAIMax(m string) bool {
	// Most OpenAI models reject "max"; only explicit allow-list (Codex 5.6 handled
	// in codexCaps). Keep false by default so max->xhigh.
	_ = m
	return false
}
