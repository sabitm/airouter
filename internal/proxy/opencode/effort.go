package opencode

import (
	"regexp"
	"strings"
)

// AnthropicEffortShape is the official @ai-sdk/anthropic effort body.
// Empty means the catalog lists efforts but official OpenCode sends no field.
type AnthropicEffortShape string

const (
	AnthropicEffortNone       AnthropicEffortShape = ""
	AnthropicEffortEnabled    AnthropicEffortShape = "enabled"
	AnthropicEffortAdaptive   AnthropicEffortShape = "adaptive"
	AnthropicEffortSummarized AnthropicEffortShape = "summarized"
)

// AnthropicEffort reports the official effort body for an Anthropic model id.
// Opus 4.5 uses enabled plus a fixed budget. 4.6 uses adaptive. Claude 4.7+
// uses adaptive with summarized display. Other ids, including non-Claude
// Anthropic rows, send no effort field.
func AnthropicEffort(model string) AnthropicEffortShape {
	id := strings.ToLower(model)
	if strings.Contains(id, "opus-4-5") || strings.Contains(id, "opus-4.5") {
		return AnthropicEffortEnabled
	}
	if anthropicModernAdaptive(id) {
		return AnthropicEffortSummarized
	}
	if anthropicAdaptive46(id) {
		return AnthropicEffortAdaptive
	}
	return AnthropicEffortNone
}

// Opus45BudgetTokens is the official fixed budget for Opus 4.5 effort.
// It is min(16000, output/2-1), not the catalog budget range.
func Opus45BudgetTokens(output int) int {
	budget := 16000
	if output > 1 {
		half := output/2 - 1
		if half < budget {
			budget = half
		}
	}
	if budget < 0 {
		return 0
	}
	return budget
}

var anthropicModernVersion = regexp.MustCompile(`(?i)claude-(?:[a-z]+-)?(\d+)(?:[.-](\d{1,2}))?(?:[.@-]|$)`)

func anthropicModernAdaptive(id string) bool {
	if !strings.Contains(id, "claude-") {
		return false
	}
	match := anthropicModernVersion.FindStringSubmatch(id)
	if match == nil {
		return true
	}
	major := atoi(match[1])
	minor := 0
	if match[2] != "" {
		minor = atoi(match[2])
	}
	return major > 4 || (major == 4 && minor >= 7)
}

func anthropicAdaptive46(id string) bool {
	needles := []string{
		"opus-4-6", "opus-4.6", "4-6-opus", "4.6-opus",
		"sonnet-4-6", "sonnet-4.6", "4-6-sonnet", "4.6-sonnet",
	}
	for _, needle := range needles {
		if strings.Contains(id, needle) {
			return true
		}
	}
	return false
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// effortRank orders unified levels. A requested level maps to the nearest
// listed value that is not below it when one exists, otherwise the nearest
// listed value. none is special: an unlisted none uses the lowest positive
// listed value rather than omission, because omission selects the model default.
var effortRank = map[string]int{
	"none":    0,
	"minimal": 1,
	"low":     2,
	"medium":  3,
	"high":    4,
	"xhigh":   5,
	"max":     6,
	"ultra":   7,
}

// MapEffort maps a unified level onto the model's listed efforts.
// ok is false when the field must be omitted (auto, or no listed efforts).
func MapEffort(level string, listed []string) (string, bool) {
	if len(listed) == 0 {
		return "", false
	}
	switch level {
	case "", "auto":
		return "", false
	case "off":
		level = "none"
	}
	if levelIn(level, listed) {
		return level, true
	}
	if level == "none" {
		return lowestPositive(listed), true
	}
	rank, known := effortRank[level]
	if !known {
		return lowestPositive(listed), true
	}
	// A gap such as low|high maps medium to high: the next listed value at or
	// above the request. A request above every listed value uses the highest.
	if atOrAbove := firstListedAtOrAbove(rank, listed); atOrAbove != "" {
		return atOrAbove, true
	}
	return highestListed(listed), true
}

func levelIn(level string, listed []string) bool {
	for _, item := range listed {
		if item == level {
			return true
		}
	}
	return false
}

func lowestPositive(listed []string) string {
	best := ""
	bestRank := int(^uint(0) >> 1)
	for _, item := range listed {
		if item == "none" || item == "" {
			continue
		}
		rank, ok := effortRank[item]
		if !ok {
			continue
		}
		if rank < bestRank {
			best, bestRank = item, rank
		}
	}
	if best == "" {
		return listed[0]
	}
	return best
}

func firstListedAtOrAbove(rank int, listed []string) string {
	best := ""
	bestRank := int(^uint(0) >> 1)
	for _, item := range listed {
		itemRank, ok := effortRank[item]
		if !ok || itemRank < rank {
			continue
		}
		if itemRank < bestRank {
			best, bestRank = item, itemRank
		}
	}
	return best
}

func highestListed(listed []string) string {
	best := listed[0]
	bestRank := -1
	for _, item := range listed {
		rank, ok := effortRank[item]
		if !ok {
			continue
		}
		if rank > bestRank {
			best, bestRank = item, rank
		}
	}
	return best
}

// BudgetTokens returns the official high or max budget for a budget-only row.
// maximum is the lowest of the catalog max, the output limit minus one, and
// 127999. high is half of that span, not below the catalog minimum.
// ok is false when the row has no usable budget.
func BudgetTokens(spec ModelSpec, level string) (int, bool) {
	if !spec.Budget || len(spec.Efforts) > 0 {
		return 0, false
	}
	maximum := 127999
	if spec.BudgetMax > 0 && spec.BudgetMax < maximum {
		maximum = spec.BudgetMax
	}
	if spec.Output > 1 && spec.Output-1 < maximum {
		maximum = spec.Output - 1
	}
	if maximum <= 0 {
		return 0, false
	}
	high := (maximum + 1) / 2
	if spec.BudgetMin > high {
		high = spec.BudgetMin
	}
	if high > maximum {
		high = maximum
	}
	if level == "max" || level == "ultra" || level == "xhigh" {
		return maximum, true
	}
	return high, true
}
