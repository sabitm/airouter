package thinking

import "strings"

// ClaudeCodeCapabilityID returns the first-party capability identity used by
// the pinned Claude Code runtime. It does not change the wire model. Known
// first-party dated ids map to their catalog id; unknown names stay distinct.
func ClaudeCodeCapabilityID(model string) string {
	id := normalizeClaudeModel(model)
	if mapped, ok := firstPartyDatedIDs[id]; ok {
		return mapped
	}
	return id
}

// normalizeClaudeModel is the lexical model identity shared by Claude
// capability tables. It removes only spellings the pinned CLI also removes.
func normalizeClaudeModel(model string) string {
	m := StripContextSelectors(strings.ToLower(strings.TrimSpace(model)))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	m = strings.ReplaceAll(m, ".", "-")
	if i := strings.Index(m, "@"); i >= 0 {
		m = m[:i]
	}
	return m
}

// firstPartyDatedIDs is the provider_ids.first_party set from the pinned
// 2.1.303 catalog. Only an exact source-listed dated id maps to its catalog id.
var firstPartyDatedIDs = map[string]string{
	"claude-3-5-haiku-20241022":  "claude-3-5-haiku",
	"claude-haiku-4-5-20251001":  "claude-haiku-4-5",
	"claude-3-5-sonnet-20241022": "claude-3-5-sonnet",
	"claude-3-7-sonnet-20250219": "claude-3-7-sonnet",
	"claude-sonnet-4-20250514":   "claude-sonnet-4-0",
	"claude-sonnet-4-5-20250929": "claude-sonnet-4-5",
	"claude-opus-4-20250514":     "claude-opus-4-0",
	"claude-opus-4-1-20250805":   "claude-opus-4-1",
	"claude-opus-4-5-20251101":   "claude-opus-4-5",
}
