package thinking

import (
	"strings"

	"airouter/internal/domain"
	"airouter/internal/proxy/opencode"
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
	FormatMiniMax
	FormatGrok
	FormatCline
	FormatCursor
)

// Caps describes how a model under a provider dialect handles thinking.
type Caps struct {
	Reasoning  bool
	CanDisable bool
	// OmitOnDisable sends no thinking control for an explicit disable. It is
	// distinct from CanDisable (write a disabled field) and !CanDisable
	// (upgrade none to the minimum accepted level).
	OmitOnDisable bool
	Format        Format
	// BudgetMin/Max clamp budget formats; zero means no clamp on that side.
	BudgetMin int
	BudgetMax int
	// MaxOutput is a model output ceiling used when reconciling budget vs max_tokens.
	MaxOutput int
	// Levels lists accepted discrete effort values for this model/dialect.
	// Empty means use the format's default set.
	Levels []string
	// AllowMax permits "max" when normalizing Codex effort values.
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
	case domain.ReasoningCline:
		return clineCaps(model)
	case domain.ReasoningOpencode:
		return opencodeCaps(m, protocol)
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
	m = StripContextSelectors(m)
	c := Caps{
		Reasoning:  true,
		CanDisable: true,
		Format:     FormatClaudeBudget,
		BudgetMin:  1024,
		BudgetMax:  128000,
		MaxOutput:  64000,
		Levels:     []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"},
	}
	var row claudeCatalogCaps
	var known bool
	if protocol == domain.ProtocolClaudeCode {
		m = ClaudeCodeCapabilityID(m)
		row, known = claudeCodeCatalogRow(m)
		row = claudeCodeEffectiveRow(m, row, known)
	} else {
		row, known = claudeCatalogRow(m)
	}
	if known {
		if row.maxOutput > 0 {
			c.MaxOutput = row.maxOutput
		}
		// A catalog hit is not evidence of thinking. Claude 3.5 and 3.7 are in
		// the table for output limits and have empty capability lists. They must
		// not bypass the non-reasoner guard.
		if row.nonReasoner {
			c.Reasoning = false
			c.Format = FormatNone
			c.Levels = nil
			return c
		}
		if row.adaptive {
			c.Format = FormatClaudeAdaptive
			c.Levels = claudeAdaptiveLevels(row)
		} else {
			c.Levels = []string{"none", "low", "medium", "high"}
			if row.xhigh {
				c.Levels = append(c.Levels, "xhigh")
			}
			if row.maxEffort {
				c.Levels = append(c.Levels, "max")
			}
		}
		if row.omitOnDisable {
			c.OmitOnDisable = true
		}
		if row.rejectsDisabled {
			c.CanDisable = false
			c.Levels = withoutLevel(c.Levels, "none")
		}
		if !row.effort && !row.adaptive {
			// Budget models still accept a thinking budget. They do not accept
			// an effort level the catalog does not list.
		}
	} else if strings.Contains(m, "claude-3") {
		c.Reasoning = false
		c.Format = FormatNone
		c.Levels = nil
	} else {
		// An unknown name is not catalog evidence. Keep budget and do not
		// invent adaptive, xhigh, or a disable rejection.
		c.Levels = []string{"none", "low", "medium", "high"}
	}
	_ = protocol
	return c
}

// claudeCodeCatalogRow uses only the source-listed first-party identity. It
// does not tail-strip an arbitrary date, so an unknown dated name stays unknown.
func claudeCodeCatalogRow(model string) (claudeCatalogCaps, bool) {
	row, ok := claudeCatalog[ClaudeCodeCapabilityID(model)]
	return row, ok
}

// claudeCodeEffectiveRow applies pinned runtime capabilities that the raw
// catalog row does not encode. Mythos 5 has an empty raw capability list, but
// the first-party runtime enables adaptive thinking, effort (including
// max/xhigh), and disabled-thinking rejection. It also omits the thinking field
// for an explicit disable instead of sending disabled or upgrading to low.
func claudeCodeEffectiveRow(id string, row claudeCatalogCaps, known bool) claudeCatalogCaps {
	if id != "claude-mythos-5" || !known {
		return row
	}
	row.adaptive = true
	row.effort = true
	row.maxEffort = true
	row.xhigh = true
	row.rejectsDisabled = true
	row.omitOnDisable = true
	return row
}

type claudeCatalogCaps struct {
	adaptive        bool
	effort          bool
	maxEffort       bool
	xhigh           bool
	rejectsDisabled bool
	omitOnDisable   bool
	nonReasoner     bool
	maxOutput       int
}

func claudeCatalogRow(model string) (claudeCatalogCaps, bool) {
	// Local table mirrors claudecode.LookupModel. claudecode imports this
	// package, so thinking must not import claudecode.
	id := normalizeClaudeModel(model)
	if mapped, ok := claudeDatedIDs[id]; ok {
		id = mapped
	}
	if _, ok := claudeCatalog[id]; !ok {
		if i := claudeDateSuffix(id); i > 0 {
			if _, ok := claudeCatalog[id[:i]]; ok {
				id = id[:i]
			}
		}
	}
	row, ok := claudeCatalog[id]
	return row, ok
}

func claudeDateSuffix(id string) int {
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '-' && i+9 == len(id) {
			all := true
			for _, c := range id[i+1:] {
				if c < '0' || c > '9' {
					all = false
					break
				}
			}
			if all {
				return i
			}
		}
	}
	return -1
}

func claudeAdaptiveLevels(row claudeCatalogCaps) []string {
	levels := []string{"none", "low", "medium", "high"}
	if row.xhigh {
		levels = append(levels, "xhigh")
	}
	if row.maxEffort {
		levels = append(levels, "max")
	}
	return levels
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

func minimaxCaps(m string) Caps {
	c := Caps{
		Reasoning:  true,
		CanDisable: false,
		Format:     FormatMiniMax,
		Levels:     []string{"none", "thinking"},
		MaxOutput:  131072,
	}
	// M3 can disable; M2.7 / M2.5 / remaining MiniMax models cannot.
	if strings.Contains(m, "minimax-m3") {
		c.CanDisable = true
	}
	return c
}

func grokCaps(m string, protocol domain.Protocol) Caps {
	format := FormatGrok
	if protocol == domain.ProtocolOpenAIResponses {
		format = FormatOpenAIResponses
	}
	if grokNonEffortModel(m) {
		return Caps{Reasoning: false, CanDisable: false, Format: FormatNone, MaxOutput: 128000}
	}
	if grokFamily43(m) {
		return Caps{
			Reasoning:  true,
			CanDisable: true,
			Format:     format,
			Levels:     []string{"none", "low", "medium", "high"},
			MaxOutput:  128000,
		}
	}
	if grokFamily45(m) {
		return Caps{
			Reasoning:  true,
			CanDisable: false,
			Format:     format,
			Levels:     []string{"low", "medium", "high"},
			MaxOutput:  128000,
		}
	}
	return Caps{
		Reasoning:  true,
		CanDisable: false,
		Format:     format,
		Levels:     []string{"low", "medium", "high", "xhigh"},
		MaxOutput:  128000,
	}
}

func grokNonEffortModel(m string) bool {
	if strings.Contains(m, "image") || strings.Contains(m, "imagine") {
		return true
	}
	if strings.Contains(m, "non-reasoning") {
		return true
	}
	if strings.Contains(m, "composer") {
		return true
	}
	if strings.HasPrefix(m, "grok-code-fast") || strings.HasPrefix(m, "grok-code") {
		return true
	}
	if strings.HasPrefix(m, "grok-build") && m != "grok-build-latest" {
		return true
	}
	return false
}

func grokFamily45(m string) bool {
	return strings.HasPrefix(m, "grok-4.5") || m == "grok-build-latest"
}

func grokFamily43(m string) bool {
	return strings.HasPrefix(m, "grok-4.3")
}

// opencodeCaps reads the Zen catalog snapshot. Go rows can differ, but CapsFor
// has no provider URL, so wire writing uses opencode.Lookup with the provider tier.
// An unknown id gets no reasoning control. Google native thinkingConfig is deferred.
func opencodeCaps(m string, protocol domain.Protocol) Caps {
	_ = protocol
	spec, ok := opencode.Lookup(opencode.Tier(""), m)
	return OpencodeCaps(spec, ok)
}

// OpencodeCaps converts a catalog row into thinking caps. ok false is an unknown model.
func OpencodeCaps(spec opencode.ModelSpec, ok bool) Caps {
	if !ok || spec.SDK == "google" || !opencodeWritesReasoning(spec) {
		out := spec.Output
		if out == 0 {
			out = 131072
		}
		return Caps{Reasoning: false, CanDisable: true, Format: FormatNone, MaxOutput: out}
	}
	if OpencodeMiniMaxToggle(spec) {
		return Caps{
			Reasoning:  true,
			CanDisable: true,
			Format:     FormatMiniMax,
			MaxOutput:  spec.Output,
			Levels:     []string{"none", "thinking"},
		}
	}
	format := FormatOpenAI
	switch spec.SDK {
	case "openai":
		format = FormatOpenAIResponses
	case "anthropic":
		format = FormatClaudeAdaptive
	}
	return Caps{
		Reasoning:  true,
		CanDisable: opencodeLevelIn("none", spec.Efforts) || opencodeQwen38FlashDisable(spec) || OpencodeAnthropicDisableSupported(spec),
		Format:     format,
		Levels:     append([]string(nil), spec.Efforts...),
		MaxOutput:  spec.Output,
	}
}

func opencodeLevelIn(level string, levels []string) bool {
	for _, item := range levels {
		if item == level {
			return true
		}
	}
	return false
}

var claudeCatalog = map[string]claudeCatalogCaps{
	"claude-3-5-haiku":  {nonReasoner: true, maxOutput: 8192},
	"claude-3-5-sonnet": {nonReasoner: true, maxOutput: 8192},
	"claude-3-7-sonnet": {nonReasoner: true, maxOutput: 64000},
	"claude-haiku-4-5":  {maxOutput: 64000},
	"claude-haiku-5-5":  {adaptive: true, effort: true, maxEffort: true, xhigh: true, rejectsDisabled: true, maxOutput: 128000},
	"claude-sonnet-4-0": {maxOutput: 64000},
	"claude-sonnet-4-5": {maxOutput: 64000},
	"claude-sonnet-4-6": {adaptive: true, effort: true, maxEffort: true, maxOutput: 128000},
	"claude-sonnet-5":   {adaptive: true, effort: true, maxEffort: true, xhigh: true, maxOutput: 128000},
	"claude-sonnet-5-5": {adaptive: true, effort: true, maxEffort: true, xhigh: true, rejectsDisabled: true, maxOutput: 128000},
	"claude-opus-4-0":   {maxOutput: 32000},
	"claude-opus-4-1":   {maxOutput: 32000},
	"claude-opus-4-5":   {maxOutput: 64000},
	"claude-opus-4-6":   {adaptive: true, effort: true, maxEffort: true, maxOutput: 128000},
	"claude-opus-4-7":   {adaptive: true, effort: true, maxEffort: true, xhigh: true, maxOutput: 128000},
	"claude-opus-4-8":   {adaptive: true, effort: true, maxEffort: true, xhigh: true, maxOutput: 128000},
	"claude-opus-5":     {adaptive: true, effort: true, maxEffort: true, xhigh: true, maxOutput: 128000},
	"claude-opus-5-5":   {adaptive: true, effort: true, maxEffort: true, xhigh: true, rejectsDisabled: true, maxOutput: 128000},
	"claude-fable-5":    {adaptive: true, effort: true, maxEffort: true, xhigh: true, rejectsDisabled: true, maxOutput: 128000},
	"claude-fable-5-1":  {adaptive: true, effort: true, maxEffort: true, xhigh: true, rejectsDisabled: true, maxOutput: 128000},
	"claude-mythos-5":   {maxOutput: 128000},
	"claude-mythos-5-1": {adaptive: true, effort: true, maxEffort: true, xhigh: true, rejectsDisabled: true, maxOutput: 128000},
}

var claudeDatedIDs = map[string]string{
	"claude-haiku-4-5-20251001": "claude-haiku-4-5",
}

func withoutLevel(levels []string, drop string) []string {
	out := make([]string, 0, len(levels))
	for _, level := range levels {
		if level != drop {
			out = append(out, level)
		}
	}
	return out
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
