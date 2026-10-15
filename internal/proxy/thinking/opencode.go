package thinking

import (
	"strings"

	"airouter/internal/proxy/opencode"
)

// writeOpencode applies the catalog row for the provider tier. No intent writes
// nothing. Google rows write nothing: native thinkingConfig is deferred.
// An effort list wins over toggle and budget. Only the selected MiniMax M3
// Messages rows write a thinking toggle; other toggle-only rows write nothing.
func writeOpencode(m map[string]any, model string, cfg *Config, tier string) {
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
		writeOpencodeEffort(m, spec, cfg)
		return
	}
	writeOpencodeBudget(m, spec, cfg)
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

func writeOpencodeBudget(m map[string]any, spec opencode.ModelSpec, cfg *Config) {
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
