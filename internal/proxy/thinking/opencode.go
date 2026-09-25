package thinking

import (
	"airouter/internal/proxy/opencode"
)

// writeOpencode applies the catalog row for the provider tier. No intent writes
// nothing. Google rows write nothing: native thinkingConfig is deferred.
// An effort list wins over toggle and budget. Toggle-only writes nothing.
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
	case opencode.AnthropicEffortEnabled:
		thinking := map[string]any{"type": "enabled"}
		if budget := opencode.Opus45BudgetTokens(spec.Output); budget > 0 {
			thinking["budget_tokens"] = budget
		}
		m["thinking"] = mergeObject(m["thinking"], thinking)
	default:
		thinking := map[string]any{"type": "adaptive"}
		if shape == opencode.AnthropicEffortSummarized {
			thinking["display"] = "summarized"
		}
		m["thinking"] = mergeObject(m["thinking"], thinking)
	}
	oc, _ := m["output_config"].(map[string]any)
	if oc == nil {
		oc = map[string]any{}
	}
	oc["effort"] = effort
	m["output_config"] = oc
}

func opencodeWritesReasoning(spec opencode.ModelSpec) bool {
	if len(spec.Efforts) > 0 {
		if spec.SDK == "anthropic" && opencode.AnthropicEffort(spec.ID) == opencode.AnthropicEffortNone {
			return false
		}
		return true
	}
	return spec.Budget && spec.SDK == "anthropic"
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
