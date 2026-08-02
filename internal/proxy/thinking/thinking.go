package thinking

import (
	"encoding/json"
	"strings"

	"airouter/internal/domain"
	"airouter/internal/proxy/ir"
)

// Mode is the unified thinking intent kind.
type Mode string

const (
	ModeNone   Mode = "none"
	ModeAuto   Mode = "auto"
	ModeLevel  Mode = "level"
	ModeBudget Mode = "budget"
)

// Config is the package-local intent; convertible to/from ir.Thinking.
type Config struct {
	Mode   Mode
	Level  string
	Budget int
}

// ToIR converts cfg to ir.Thinking. Nil in, nil out.
func ToIR(cfg *Config) *ir.Thinking {
	if cfg == nil {
		return nil
	}
	return &ir.Thinking{
		Mode:   ir.ThinkingMode(cfg.Mode),
		Level:  cfg.Level,
		Budget: cfg.Budget,
	}
}

// FromIR converts t to Config. Nil in, nil out.
func FromIR(t *ir.Thinking) *Config {
	if t == nil {
		return nil
	}
	return &Config{
		Mode:   Mode(t.Mode),
		Level:  t.Level,
		Budget: t.Budget,
	}
}

// Merge returns override when set, otherwise base.
func Merge(base, override *Config) *Config {
	if override != nil {
		return override
	}
	return base
}

// FromOpenAIEffort maps a reasoning_effort / reasoning.effort string.
func FromOpenAIEffort(effort string) *Config {
	e := strings.ToLower(strings.TrimSpace(effort))
	if e == "" {
		return nil
	}
	switch e {
	case "none", "off":
		return &Config{Mode: ModeNone}
	case "auto":
		return &Config{Mode: ModeAuto}
	default:
		if knownLevels[e] {
			return &Config{Mode: ModeLevel, Level: e}
		}
		// Pass through unknown levels so encode can forward them.
		return &Config{Mode: ModeLevel, Level: e}
	}
}

// FromAnthropic maps Anthropic thinking + output_config.effort.
// output_config.effort wins over the thinking block (9router order).
func FromAnthropic(thinkingType string, budgetTokens int, outputEffort string) *Config {
	if e := strings.ToLower(strings.TrimSpace(outputEffort)); e != "" {
		return FromOpenAIEffort(e)
	}
	switch strings.ToLower(strings.TrimSpace(thinkingType)) {
	case "":
		return nil
	case "disabled":
		return &Config{Mode: ModeNone}
	case "adaptive", "enabled":
		if budgetTokens > 0 {
			return &Config{Mode: ModeBudget, Budget: budgetTokens}
		}
		return &Config{Mode: ModeAuto}
	default:
		return nil
	}
}

// Effective returns cfg after capability clamps. Nil when there is nothing to send.
// Non-reasoning caps drop intent. !CanDisable turns none into minimal.
func Effective(cfg *Config, caps Caps) *Config {
	if cfg == nil || !caps.Reasoning || caps.Format == FormatNone {
		return nil
	}
	out := *cfg
	if out.Mode == ModeNone && !caps.CanDisable {
		out = Config{Mode: ModeLevel, Level: "minimal"}
	}
	return &out
}

// ApplyWire patches a passthrough JSON body: set model, strip known thinking
// fields, and write the native shape for formatID (oai-chat, anth-msg, oai-responses).
func ApplyWire(formatID string, body []byte, model string, cfg *Config) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	m["model"] = model
	stripWireFields(m)
	if cfg != nil {
		writeWire(formatID, m, cfg)
	}
	return json.Marshal(m)
}

func stripWireFields(m map[string]any) {
	delete(m, "thinking")
	delete(m, "reasoning_effort")
	delete(m, "reasoning")
	delete(m, "output_config")
	delete(m, "thinkingConfig")
	delete(m, "enable_thinking")
	delete(m, "thinking_budget")
	if gc, ok := m["generationConfig"].(map[string]any); ok {
		delete(gc, "thinkingConfig")
	}
}

func writeWire(formatID string, m map[string]any, cfg *Config) {
	model, _ := m["model"].(string)
	switch formatID {
	case "oai-chat":
		eff := Effective(cfg, CapsFor(model, domain.ProtocolOpenAI))
		if eff == nil {
			return
		}
		if level := LevelFor(eff); level != "" {
			m["reasoning_effort"] = level
		}
	case "oai-responses":
		eff := Effective(cfg, CapsFor(model, domain.ProtocolOpenAIResponses))
		if eff == nil {
			return
		}
		if eff.Mode == ModeNone {
			m["reasoning"] = map[string]any{"effort": "none"}
			return
		}
		if level := LevelFor(eff); level != "" && level != "none" {
			m["reasoning"] = map[string]any{"effort": level}
		}
	case "anth-msg":
		caps := CapsFor(model, domain.ProtocolAnthropic)
		eff := Effective(cfg, caps)
		if eff == nil {
			return
		}
		writeAnthropicWire(m, eff, caps.Format)
	}
}

// WriteAnthropicMap applies thinking fields onto an Anthropic-shaped map.
// Used by ApplyWire and available for tests.
func writeAnthropicWire(m map[string]any, cfg *Config, format Format) {
	if cfg == nil {
		return
	}
	if cfg.Mode == ModeNone {
		m["thinking"] = map[string]any{"type": "disabled"}
		return
	}
	if format == FormatClaudeAdaptive {
		m["thinking"] = map[string]any{"type": "adaptive"}
		level := cfg.Level
		if cfg.Mode == ModeBudget {
			level = BudgetToLevel(cfg.Budget)
		}
		if cfg.Mode == ModeAuto {
			level = "auto"
		}
		if level == "xhigh" || level == "max" {
			level = "high"
		}
		if level == "" {
			level = "medium"
		}
		m["output_config"] = map[string]any{"effort": level}
		return
	}
	// budget format
	if cfg.Mode == ModeAuto {
		m["thinking"] = map[string]any{"type": "enabled"}
		return
	}
	budget := BudgetFor(cfg, 1024, 128000)
	if budget <= 0 {
		budget = 8192
	}
	m["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
}
