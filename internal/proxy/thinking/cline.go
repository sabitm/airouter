package thinking

import "strings"

// Cline disable wire style. Unknown and effort-only models omit replacement.
type clineDisableStyle int

const (
	clineDisableOmit clineDisableStyle = iota
	clineDisableEnabledFalse
	clineDisableEnabledFalseAndThinkingDisabled
	clineDisableExcludeTrue
)

func clineCaps(_ string) Caps {
	return Caps{
		Reasoning:  true,
		CanDisable: true,
		Format:     FormatCline,
		Levels:     []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"},
	}
}

// stripClineControls drops Cline-recognized disable knobs that the generic
// strip leaves in place (reasoning.enabled / reasoning.exclude). Other
// dialects must not see this cleanup.
func stripClineControls(m map[string]any) {
	if r, ok := m["reasoning"].(map[string]any); ok {
		delete(r, "enabled")
		delete(r, "exclude")
		if len(r) == 0 {
			delete(m, "reasoning")
		} else {
			m["reasoning"] = r
		}
	}
}

func writeCline(m map[string]any, cfg *Config, model string) {
	if cfg == nil || cfg.Mode == ModeAuto {
		return
	}
	if cfg.Mode == ModeNone {
		writeClineDisable(m, clineDisableFor(model))
		return
	}
	level := LevelFor(cfg)
	if level == "" || level == "none" || level == "auto" {
		return
	}
	m["reasoning_effort"] = level
}

func writeClineDisable(m map[string]any, style clineDisableStyle) {
	switch style {
	case clineDisableEnabledFalse:
		setReasoningField(m, "enabled", false)
	case clineDisableEnabledFalseAndThinkingDisabled:
		setReasoningField(m, "enabled", false)
		m["thinking"] = mergeObject(m["thinking"], map[string]any{"type": "disabled"})
	case clineDisableExcludeTrue:
		setReasoningField(m, "exclude", true)
	}
}

func setReasoningField(m map[string]any, key string, val any) {
	r, _ := m["reasoning"].(map[string]any)
	if r == nil {
		r = map[string]any{}
	}
	r[key] = val
	m["reasoning"] = r
}

func clineDisableFor(model string) clineDisableStyle {
	id := strings.ToLower(strings.TrimSpace(model))
	if strings.HasPrefix(id, "cline-pass/") {
		return clinePassDisableFor(id)
	}
	return clineDirectDisableFor(id)
}

func clinePassDisableFor(id string) clineDisableStyle {
	switch {
	case strings.Contains(id, "deepseek"):
		return clineDisableEnabledFalseAndThinkingDisabled
	case strings.Contains(id, "glm-5.3"):
		return clineDisableOmit
	case strings.Contains(id, "glm-5.2"):
		return clineDisableEnabledFalse
	case strings.Contains(id, "kimi-k2.7-code"):
		return clineDisableOmit
	case strings.Contains(id, "qwen"):
		return clineDisableOmit
	case strings.Contains(id, "kimi"):
		return clineDisableEnabledFalse
	case strings.Contains(id, "minimax"):
		return clineDisableEnabledFalse
	case strings.Contains(id, "mimo"):
		return clineDisableEnabledFalse
	default:
		return clineDisableOmit
	}
}

func clineDirectDisableFor(id string) clineDisableStyle {
	switch {
	case strings.HasPrefix(id, "cline-free/"):
		return clineFreeDisableFor(id)
	case strings.Contains(id, "deepseek-r1"):
		return clineDisableOmit
	case strings.Contains(id, "deepseek"):
		return clineDisableEnabledFalseAndThinkingDisabled
	case strings.HasPrefix(id, "z-ai/"), strings.HasPrefix(id, "~z-ai/"):
		return clineDirectZAIDisableFor(id)
	case strings.Contains(id, "kimi-k2.7-code"), strings.Contains(id, "kimi-k2-thinking"):
		return clineDisableOmit
	case strings.Contains(id, "qwen"):
		return clineDisableOmit
	case strings.Contains(id, "kimi"):
		return clineDisableEnabledFalseAndThinkingDisabled
	case clineClaudeCanDisable(id), clineGPTCanDisable(id), clineGeminiCanDisable(id),
		strings.Contains(id, "minimax-m3"), strings.Contains(id, "minimax-m1"),
		strings.Contains(id, "mimo-v2.5"), clineReasoningEnabledFalseFamily(id):
		return clineDisableEnabledFalse
	default:
		return clineDisableOmit
	}
}

// Cline Free disable is an explicit catalog snapshot, not a namespace default.
// Unknown cline-free/* models omit rather than inherit later family matchers.
func clineFreeDisableFor(id string) clineDisableStyle {
	switch clineModelBasename(id) {
	case "deepseek-v4.1-flash":
		return clineDisableEnabledFalseAndThinkingDisabled
	case "solar-pro4":
		return clineDisableEnabledFalse
	default:
		return clineDisableOmit
	}
}

func clineDirectZAIDisableFor(id string) clineDisableStyle {
	if clineDirectZAIOffCapable(id) {
		return clineDisableExcludeTrue
	}
	return clineDisableOmit
}

// Off-capable direct Cline Z.AI/GLM ids from the static snapshot. glm-5.3-flash
// is effort-only; unknown z-ai/* and ~z-ai/* models omit.
func clineDirectZAIOffCapable(id string) bool {
	switch clineModelBasename(id) {
	case "glm-5.2", "glm-5.1", "glm-5v-turbo", "glm-5-turbo", "glm-5",
		"glm-4.7-flash", "glm-4.7", "glm-4.6v", "glm-4.6",
		"glm-4.5v", "glm-4.5-air", "glm-4.5":
		return true
	default:
		return false
	}
}

func clineClaudeCanDisable(id string) bool {
	return strings.Contains(id, "claude") &&
		!strings.Contains(id, "claude-fable") &&
		!strings.Contains(id, "claude-3")
}

func clineGPTCanDisable(id string) bool {
	model := clineModelBasename(id)
	switch model {
	case "o1", "o3", "o3-pro", "o3-mini", "o4-mini":
		return true
	}
	if !strings.HasPrefix(model, "gpt-") {
		return false
	}
	if strings.Contains(model, "codex") {
		return strings.HasPrefix(model, "gpt-5.3-codex") || model == "gpt-5.1-codex-mini"
	}
	for _, prefix := range []string{"gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra"} {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	if strings.Contains(model, "-pro") {
		return false
	}
	for _, prefix := range []string{"gpt-5.5", "gpt-5.4", "gpt-5.2", "gpt-5.1"} {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return model == "gpt-mini-latest" || model == "gpt-luna-latest" ||
		model == "gpt-sol-latest" || model == "gpt-terra-latest"
}

func clineGeminiCanDisable(id string) bool {
	model := clineModelBasename(id)
	return strings.HasPrefix(model, "gemini-3.1-flash-lite") ||
		strings.HasPrefix(model, "gemini-3.1-flash-image") ||
		strings.HasPrefix(model, "gemini-3-flash-preview") ||
		strings.HasPrefix(model, "gemini-2.5-flash")
}

func clineReasoningEnabledFalseFamily(id string) bool {
	for _, part := range []string{
		"inclusionai/ling-", "inception/mercury-", "nex-agi/nex-", "ibm-granite/",
		"tencent/hy", "dots-studio/", "bytedance-seed/", "nvidia/nemotron-",
		"upstage/solar-", "sakana/sakana-namazu", "thinkingmachines/inkling",
		"poolside/laguna-", "meituan/longcat-", "kwaipilot/kat-coder-",
		"cohere/north-", "mistralai/mistral-medium-3-5", "mistralai/mistral-small-2603",
		"x-ai/grok-4.3", "x-ai/grok-4.20", "google/gemma-4-", "rekaai/reka-edge",
	} {
		if strings.Contains(id, part) {
			return true
		}
	}
	return false
}

func clineModelBasename(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		return id[i+1:]
	}
	return id
}
