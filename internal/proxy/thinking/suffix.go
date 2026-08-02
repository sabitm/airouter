package thinking

import (
	"strconv"
	"strings"
)

// ParseSuffix splits "model(high)" / "model(8192)" / "model(none)" / "model(auto)".
// Only known levels, digits, none/off, or auto are consumed; any other parenthetical
// is left on the model name so legitimate ids are not mangled.
func ParseSuffix(model string) (base string, cfg *Config) {
	base = model
	if model == "" {
		return model, nil
	}
	// Trailing "(value)" only.
	i := strings.LastIndex(model, "(")
	if i < 0 || !strings.HasSuffix(model, ")") {
		return model, nil
	}
	inner := strings.TrimSpace(model[i+1 : len(model)-1])
	if inner == "" || strings.ContainsAny(inner, "()") {
		return model, nil
	}
	clean := strings.TrimSpace(model[:i])
	if clean == "" {
		return model, nil
	}
	raw := strings.ToLower(inner)
	switch {
	case raw == "none" || raw == "off":
		return clean, &Config{Mode: ModeNone}
	case raw == "auto":
		return clean, &Config{Mode: ModeAuto}
	case isAllDigits(raw):
		n, err := strconv.Atoi(raw)
		if err != nil {
			return model, nil
		}
		return clean, &Config{Mode: ModeBudget, Budget: n}
	case knownLevels[raw]:
		return clean, &Config{Mode: ModeLevel, Level: raw}
	default:
		return model, nil
	}
}

// StripSuffix returns the model without a recognized thinking suffix.
func StripSuffix(model string) string {
	base, _ := ParseSuffix(model)
	return base
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
