package kiro

import "encoding/json"

const (
	ThinkingUnset    = ""
	ThinkingDisabled = "disabled"
	ThinkingAdaptive = "adaptive"
)

// Selection is the resolved additionalModelRequestFields document. Nil means
// no control is advertised or selected.
type Selection struct {
	Fields map[string]any
	// BudgetIgnored is true when the caller supplied a numeric budget. The
	// approved schemas have no budget field, so the number is not encoded.
	BudgetIgnored bool
	// EffortOmitted is true when an advertised effort list became empty after
	// the disabled-thinking filter.
	EffortOmitted bool
}

// ResolveSelection applies the active IDE normal selector. qSr resolves the
// thinking toggle first. jSr then selects effort and filters xhigh/max when
// that resolved thinking is disabled. ogt caps any residual extreme level.
// Missing capability omits controls. Unsupported requested effort uses the
// catalog fallback, not a guessed conversion.
func ResolveSelection(cap *Capability, effort string, effortSet bool, thinking string, budget bool) *Selection {
	if cap == nil {
		if budget {
			return &Selection{BudgetIgnored: true}
		}
		return nil
	}
	thinkingType, thinkingOK := selectThinking(cap, thinking)
	level, levelOK := selectEffort(cap, effort, effortSet, thinkingType)
	if thinkingType == ThinkingDisabled {
		level, levelOK = capPreselected(cap, level, levelOK)
	}
	fields := map[string]any{}
	if levelOK && cap.HasEffort && cap.EffortPath != "" && level != "" {
		fields[cap.EffortPath] = map[string]any{"effort": level}
	}
	if thinkingOK && thinkingType != "" {
		fields["thinking"] = map[string]any{"type": thinkingType}
	}
	if len(fields) == 0 && !budget && !(cap.HasEffort && !levelOK) {
		return nil
	}
	return &Selection{Fields: fields, BudgetIgnored: budget, EffortOmitted: cap.HasEffort && !levelOK}
}

// selectEffort is jSr. Missing path, levels, or default omits effort. Disabled
// thinking removes xhigh/max before fallback. Requested effort is used only
// when the remaining list contains it.
func selectEffort(cap *Capability, effort string, effortSet bool, thinking string) (string, bool) {
	if cap == nil || !cap.HasEffort || cap.EffortPath == "" || len(cap.EffortLevels) == 0 || cap.DefaultEffort == "" {
		return "", false
	}
	levels := append([]string(nil), cap.EffortLevels...)
	if thinking == ThinkingDisabled {
		levels = filterExtreme(levels)
	}
	if len(levels) == 0 {
		return "", false
	}
	fallback := cap.DefaultEffort
	if !contains(levels, fallback) {
		fallback = levels[len(levels)-1]
	}
	if effortSet && contains(levels, effort) {
		return effort, true
	}
	return fallback, true
}

// selectThinking is qSr. Only a toggleable schema sends thinking.type.
// Explicit disabled or adaptive wins. Otherwise the advertised default is used.
func selectThinking(cap *Capability, thinking string) (string, bool) {
	if cap == nil || !cap.HasThinking || !cap.ThinkingToggle {
		return "", false
	}
	switch thinking {
	case ThinkingDisabled:
		return ThinkingDisabled, true
	case ThinkingAdaptive:
		return ThinkingAdaptive, true
	default:
		if cap.ThinkingEnabled {
			return ThinkingAdaptive, true
		}
		return ThinkingDisabled, true
	}
}

// capPreselected is BSr. A preselected xhigh/max is capped to the last enum
// level outside that pair when thinking is disabled. Other levels stay.
func capPreselected(cap *Capability, level string, ok bool) (string, bool) {
	if !ok || cap == nil || (level != "xhigh" && level != "max") {
		return level, ok
	}
	filtered := filterExtreme(cap.EffortLevels)
	if len(filtered) == 0 {
		return "", false
	}
	return filtered[len(filtered)-1], true
}

func filterExtreme(levels []string) []string {
	out := make([]string, 0, len(levels))
	for _, level := range levels {
		if level == "xhigh" || level == "max" {
			continue
		}
		out = append(out, level)
	}
	return out
}

func contains(levels []string, level string) bool {
	for _, item := range levels {
		if item == level {
			return true
		}
	}
	return false
}

// ApplyAdditionalFields sets additionalModelRequestFields on an encoded Kiro
// object. A nil or empty selection leaves the body unchanged. Existing fields
// are replaced by the resolved document. The function does not mutate IR.
func ApplyAdditionalFields(body []byte, sel *Selection) []byte {
	if sel == nil || len(sel.Fields) == 0 {
		return body
	}
	raw, err := json.Marshal(sel.Fields)
	if err != nil {
		return body
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil || m == nil {
		return body
	}
	m["additionalModelRequestFields"] = raw
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
