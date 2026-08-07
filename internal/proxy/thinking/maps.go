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

// LevelFor returns an effort string for cfg. ModeLevel values are forwarded
// verbatim (including max/minimal/xhigh); backends disagree on valid enums, so
// per-model validity is the caller's responsibility. Budget mode still converts
// via BudgetToLevel. Empty when cfg is nil.
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

// NormalizeOpenAILevel clamps max/ultra to xhigh unless AllowMax (or level is
// listed in caps.Levels).
func NormalizeOpenAILevel(level string, caps Caps) string {
	if level != "max" && level != "ultra" {
		return level
	}
	if caps.AllowMax || levelIn(level, caps.Levels) {
		return level
	}
	if level == "ultra" && (caps.AllowMax || levelIn("max", caps.Levels)) {
		return "max"
	}
	return "xhigh"
}

// MapClaudeAdaptiveLevel maps unified levels onto the adaptive effort set
// (none/low/medium/high/max). minimal->low, xhigh->high; max is preserved.
func MapClaudeAdaptiveLevel(level string) string {
	switch level {
	case "", "auto":
		return level
	case "minimal":
		return "low"
	case "xhigh":
		return "high"
	case "none", "low", "medium", "high", "max":
		return level
	default:
		return level
	}
}

// MapKimiLevel maps unified levels onto Kimi's set (none/low/medium/high/max).
// minimal->low, xhigh->max.
func MapKimiLevel(level string) string {
	switch level {
	case "minimal":
		return "low"
	case "xhigh":
		return "max"
	case "low", "medium", "high", "max", "auto":
		return level
	default:
		return ""
	}
}

// MapDeepSeekLevel maps unified levels onto DeepSeek's effective set
// (none/high/max). minimal..high -> high; xhigh/max -> max.
func MapDeepSeekLevel(level string) string {
	switch level {
	case "xhigh", "max":
		return "max"
	case "none":
		return "none"
	case "auto":
		return "high"
	default:
		// minimal, low, medium, high, unknown -> high
		return "high"
	}
}

func levelIn(level string, levels []string) bool {
	for _, l := range levels {
		if l == level {
			return true
		}
	}
	return false
}

// MinAcceptedLevel returns the lowest non-none level for caps (used when
// !CanDisable turns none into a positive effort).
func MinAcceptedLevel(caps Caps) string {
	if len(caps.Levels) > 0 {
		for _, l := range caps.Levels {
			if l != "none" && l != "" {
				return l
			}
		}
	}
	switch caps.Format {
	case FormatKimi, FormatClaudeAdaptive:
		return "low"
	case FormatDeepSeek:
		return "high"
	default:
		return "minimal"
	}
}
