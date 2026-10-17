package thinking

import (
	"errors"
	"strings"

	"airouter/internal/domain"
	"airouter/internal/proxy/opencode"
)

// writeOpencode applies the catalog row for the provider tier. No intent writes
// nothing. Google rows write nothing: native thinkingConfig is deferred.
// An effort list wins over toggle and budget. Only the selected MiniMax M3
// Messages rows write a thinking toggle; other toggle-only rows write nothing.
func writeOpencode(m map[string]any, model string, cfg *Config, tier string, protocol domain.Protocol) {
	if cfg == nil {
		return
	}
	if tier == "" {
		tier = opencode.Tier("")
	}
	spec, ok := opencode.Lookup(tier, model)
	if !ok || spec.SDK == "google" || !opencodeWritesReasoning(spec) {
		return
	}
	if spec.SDK == "anthropic" && !lastMessageIsUser(m) {
		return
	}
	if OpencodeMiniMaxToggle(spec) {
		writeOpencodeMiniMaxToggle(m, cfg)
		return
	}
	if len(spec.Efforts) > 0 {
		// Exact qwen3.8-flash Anthropic rows accept an explicit disable that must
		// not be rewritten as their lowest listed effort.
		if opencodeQwen38FlashDisable(spec) && qwen38FlashDisableIntent(cfg) {
			writeOpencodeQwen38FlashDisabled(m)
			return
		}
		writeOpencodeEffort(m, spec, cfg)
		return
	}
	writeOpencodeBudget(m, spec, cfg, protocol)
}

// opencodeQwen38FlashDisable selects only the exact Anthropic Messages rows.
// Prefix, suffix, and neighboring model ids stay false.
func opencodeQwen38FlashDisable(spec opencode.ModelSpec) bool {
	return spec.SDK == "anthropic" && spec.ID == "qwen3.8-flash"
}

// qwen38FlashDisableIntent separates explicit enablement from intensity.
// A positive effort remains effort unless enablement or a none/off suffix
// explicitly disables thinking.
func qwen38FlashDisableIntent(cfg *Config) bool {
	if cfg == nil {
		return false
	}
	if cfg.Enable == EnableDisabled {
		return true
	}
	switch cfg.Mode {
	case ModeNone:
		return true
	case ModeLevel:
		level := strings.ToLower(strings.TrimSpace(cfg.Level))
		return level == "none" || level == "off"
	default:
		return false
	}
}

// writeOpencodeQwen38FlashDisabled writes the pinned SDK disabled shape and
// removes effort. output_config siblings survive through the existing finalizer.
func writeOpencodeQwen38FlashDisabled(m map[string]any) {
	m["thinking"] = mergeObject(m["thinking"], map[string]any{"type": "disabled"})
	deleteThinkingBudget(m)
	if oc, ok := m["output_config"].(map[string]any); ok {
		delete(oc, "effort")
		if len(oc) == 0 {
			delete(m, "output_config")
		}
	}
}

func writeOpencodeEffort(m map[string]any, spec opencode.ModelSpec, cfg *Config) {
	level := LevelFor(cfg)
	if cfg.Mode == ModeBudget {
		level = BudgetToLevel(cfg.Budget)
	}
	effort, ok := opencode.MapEffort(level, spec.Efforts)
	if !ok {
		return
	}
	switch spec.SDK {
	case "openai":
		r, _ := m["reasoning"].(map[string]any)
		if r == nil {
			r = map[string]any{}
		}
		r["effort"] = effort
		if _, has := r["summary"]; !has {
			r["summary"] = "auto"
		}
		m["reasoning"] = r
	case "anthropic":
		writeAnthropicEffort(m, spec, effort)
	default:
		m["reasoning_effort"] = effort
	}
}

func writeAnthropicEffort(m map[string]any, spec opencode.ModelSpec, effort string) {
	shape := opencode.AnthropicEffort(spec.ID)
	switch shape {
	case opencode.AnthropicEffortNone:
		return
	case opencode.AnthropicEffortOnly:
		// v1.19 effort fallback writes no thinking block.
	case opencode.AnthropicEffortEnabled:
		thinking := map[string]any{"type": "enabled"}
		if budget := opencode.Opus45BudgetTokens(spec.Output); budget > 0 {
			thinking["budget_tokens"] = budget
		}
		m["thinking"] = mergeObject(m["thinking"], thinking)
	case opencode.AnthropicEffortAdaptive, opencode.AnthropicEffortSummarized:
		thinking := map[string]any{"type": "adaptive"}
		if shape == opencode.AnthropicEffortSummarized {
			thinking["display"] = "summarized"
		}
		m["thinking"] = mergeObject(m["thinking"], thinking)
	default:
		return
	}
	oc, _ := m["output_config"].(map[string]any)
	if oc == nil {
		oc = map[string]any{}
	}
	oc["effort"] = effort
	m["output_config"] = oc
}

func opencodeWritesReasoning(spec opencode.ModelSpec) bool {
	if OpencodeMiniMaxToggle(spec) {
		return true
	}
	if len(spec.Efforts) > 0 {
		if spec.SDK == "anthropic" && opencode.AnthropicEffort(spec.ID) == opencode.AnthropicEffortNone {
			return false
		}
		return true
	}
	return spec.Budget && spec.SDK == "anthropic"
}

// OpencodeMiniMaxToggle selects only the pinned Anthropic Messages catalog rows
// that expose a thinking toggle and no effort or budget control. Prefix and
// Chat lookalikes stay false.
func OpencodeMiniMaxToggle(spec opencode.ModelSpec) bool {
	if spec.SDK != "anthropic" || !spec.Toggle || len(spec.Efforts) > 0 || spec.Budget {
		return false
	}
	switch spec.ID {
	case "minimax-m3", "minimax-m3-free":
		return true
	default:
		return false
	}
}

// writeOpencodeMiniMaxToggle writes the official toggle body. Explicit disable
// wins over effort and budget. Positive effort or budget is enable intent only;
// neither value is forwarded.
func writeOpencodeMiniMaxToggle(m map[string]any, cfg *Config) {
	enabled, write := miniMaxToggleEnabled(cfg)
	if !write {
		return
	}
	typ := "disabled"
	if enabled {
		typ = "adaptive"
	}
	m["thinking"] = mergeObject(m["thinking"], map[string]any{"type": typ})
	deleteThinkingBudget(m)
}

// miniMaxToggleEnabled maps existing unified intent. Explicit enablement wins.
// ModeAuto without enablement is not a toggle selection, so it writes nothing.
func miniMaxToggleEnabled(cfg *Config) (enabled bool, write bool) {
	if cfg == nil {
		return false, false
	}
	switch cfg.Enable {
	case EnableDisabled:
		return false, true
	case EnableEnabled:
		return true, true
	}
	switch cfg.Mode {
	case ModeNone:
		return false, true
	case ModeLevel:
		level := strings.ToLower(strings.TrimSpace(cfg.Level))
		if level == "" || level == "auto" {
			return false, false
		}
		if level == "none" || level == "off" {
			return false, true
		}
		return true, true
	case ModeBudget:
		return cfg.Budget > 0, cfg.Budget > 0
	default:
		return false, false
	}
}

func deleteThinkingBudget(m map[string]any) {
	thinking, ok := m["thinking"].(map[string]any)
	if !ok {
		return
	}
	delete(thinking, "budget_tokens")
}

func writeOpencodeBudget(m map[string]any, spec opencode.ModelSpec, cfg *Config, protocol domain.Protocol) {
	if budget, ok := opencodeExplicitBudget(spec, cfg, protocol); ok {
		// The caller number is the wire budget. LevelFor would replace it with a
		// high/max preset before the finalizer can keep or reject it.
		m["thinking"] = mergeObject(m["thinking"], map[string]any{"type": "enabled", "budget_tokens": budget})
		return
	}
	level := LevelFor(cfg)
	if cfg.Mode == ModeNone {
		level = "none"
	}
	if level == "" || level == "auto" || level == "none" {
		return
	}
	budget, ok := opencode.BudgetTokens(spec, level)
	if !ok || budget <= 0 {
		return
	}
	m["thinking"] = mergeObject(m["thinking"], map[string]any{"type": "enabled", "budget_tokens": budget})
}

// ErrOpencodeExplicitBudget is a static range rejection for an effective
// positive numeric budget on an exact budget-only Messages row. It carries no
// request payload. The proxy maps it to the same HTTP 400 as a budget that
// cannot fit the caller cap or catalog ceiling.
var ErrOpencodeExplicitBudget = errors.New("thinking budget exceeds max_tokens or model output limit")

// opencodeExplicitBudget returns a positive ModeBudget value only for the four
// Zen Anthropic Messages rows that are budget-only. Effort rows, Go rows, and
// nonpositive budgets stay on the existing preset or omission path.
func opencodeExplicitBudget(spec opencode.ModelSpec, cfg *Config, protocol domain.Protocol) (int, bool) {
	// Exact numeric preservation is an OpenCode protocol behavior. The same
	// catalog row on another protocol keeps the preexisting high/max preset.
	if protocol != domain.ProtocolOpencode || cfg == nil || cfg.Mode != ModeBudget || cfg.Budget <= 0 || !opencodeBudgetOnlyRow(spec) {
		return 0, false
	}
	return cfg.Budget, true
}

func opencodeBudgetOnlyRow(spec opencode.ModelSpec) bool {
	if spec.SDK != "anthropic" || !spec.Budget || len(spec.Efforts) > 0 || spec.Toggle {
		return false
	}
	switch spec.ID {
	case "claude-haiku-4-5", "claude-opus-4-1", "claude-sonnet-4", "claude-sonnet-4-5":
		return true
	default:
		return false
	}
}

// OpencodeExplicitBudgetInvalid reports whether the effective numeric budget
// is outside the selected row. BudgetSet alone is not a budget. A nonpositive
// ModeBudget value is existing omission, not a new range error. The caller
// must apply the same final-user gate as the writer before calling this.
func OpencodeExplicitBudgetInvalid(spec opencode.ModelSpec, cfg *Config, maxTokens int) bool {
	budget, ok := opencodeExplicitBudget(spec, cfg, domain.ProtocolOpencode)
	if !ok {
		return false
	}
	minimum := spec.BudgetMin
	if minimum <= 0 {
		minimum = 1024
	}
	if budget < minimum {
		return true
	}
	ceiling := spec.Output
	if ceiling <= 0 {
		return true
	}
	if budget >= ceiling {
		return true
	}
	if maxTokens > 0 && (budget >= maxTokens || maxTokens > ceiling) {
		return true
	}
	return false
}
