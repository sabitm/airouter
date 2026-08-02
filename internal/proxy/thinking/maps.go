package thinking

// LevelToBudget maps discrete effort levels to token budgets (9router thinking.js).
var LevelToBudget = map[string]int{
	"none":    0,
	"minimal": 512,
	"low":     1024,
	"medium":  8192,
	"high":    24576,
	"xhigh":   32768,
	"max":     128000,
}

// knownLevels are accepted by ParseSuffix and From* helpers.
var knownLevels = map[string]bool{
	"minimal": true,
	"low":     true,
	"medium":  true,
	"high":    true,
	"xhigh":   true,
	"max":     true,
}

// BudgetToLevel returns the nearest discrete level for a numeric budget.
// Returns "" when budget <= 0.
func BudgetToLevel(budget int) string {
	if budget <= 0 {
		return ""
	}
	if budget <= 768 {
		return "minimal"
	}
	if budget <= 4096 {
		return "low"
	}
	if budget <= 16384 {
		return "medium"
	}
	if budget <= 28672 {
		return "high"
	}
	return "xhigh"
}

// LevelFor returns an OpenAI-style effort string for cfg.
// "max" clamps to "xhigh" (OpenAI enum has no max). Empty when cfg is nil.
func LevelFor(cfg *Config) string {
	if cfg == nil {
		return ""
	}
	switch cfg.Mode {
	case ModeNone:
		return "none"
	case ModeAuto:
		return "auto"
	case ModeBudget:
		return BudgetToLevel(cfg.Budget)
	case ModeLevel:
		if cfg.Level == "max" {
			return "xhigh"
		}
		return cfg.Level
	}
	return ""
}

// BudgetFor returns a token budget for cfg, clamped to range when set.
// Auto yields -1. None / missing yields 0.
func BudgetFor(cfg *Config, min, max int) int {
	if cfg == nil {
		return 0
	}
	var budget int
	switch cfg.Mode {
	case ModeNone:
		return 0
	case ModeAuto:
		return -1
	case ModeBudget:
		budget = cfg.Budget
	case ModeLevel:
		if b, ok := LevelToBudget[cfg.Level]; ok {
			budget = b
		} else {
			budget = LevelToBudget["medium"]
		}
	default:
		return 0
	}
	if min > 0 && budget > 0 && budget < min {
		budget = min
	}
	if max > 0 && budget > max {
		budget = max
	}
	return budget
}
