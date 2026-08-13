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
// Intent is intensity-only: no source dialect is attached. The target provider
// controls the outgoing dialect.
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

// Capture extracts intensity-only thinking intent from a raw JSON body before
// typed codecs drop unfamiliar fields. Recognizes reasoning_effort,
// reasoning.effort, thinking.type/budget_tokens, output_config.effort,
// enable_thinking, thinking_budget. Returns nil when no intent is present.
func Capture(body []byte) *Config {
	if len(body) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil
	}
	return CaptureMap(m)
}

// CaptureMap is the map form of Capture.
func CaptureMap(m map[string]any) *Config {
	if m == nil {
		return nil
	}

	// Claude output_config.effort — priority over adaptive thinking.
	if oc, ok := m["output_config"].(map[string]any); ok {
		if e, ok := oc["effort"].(string); ok && strings.TrimSpace(e) != "" {
			return FromOpenAIEffort(e)
		}
	}

	// Claude / DeepSeek / ZAI thinking object.
	if t, ok := m["thinking"].(map[string]any); ok {
		typ, _ := t["type"].(string)
		budget := 0
		switch b := t["budget_tokens"].(type) {
		case float64:
			budget = int(b)
		case json.Number:
			if n, err := b.Int64(); err == nil {
				budget = int(n)
			}
		}
		if cfg := FromAnthropic(typ, budget, ""); cfg != nil {
			return cfg
		}
	}

	// OpenAI chat reasoning_effort.
	if e, ok := m["reasoning_effort"].(string); ok && strings.TrimSpace(e) != "" {
		return FromOpenAIEffort(e)
	}

	// OpenAI Responses / Codex reasoning.effort.
	if r, ok := m["reasoning"].(map[string]any); ok {
		if e, ok := r["effort"].(string); ok && strings.TrimSpace(e) != "" {
			return FromOpenAIEffort(e)
		}
	}

	// Qwen enable_thinking + thinking_budget.
	if et, ok := m["enable_thinking"].(bool); ok {
		if !et {
			return &Config{Mode: ModeNone}
		}
		budget := 0
		switch b := m["thinking_budget"].(type) {
		case float64:
			budget = int(b)
		case json.Number:
			if n, err := b.Int64(); err == nil {
				budget = int(n)
			}
		}
		if budget > 0 {
			return &Config{Mode: ModeBudget, Budget: budget}
		}
		return &Config{Mode: ModeAuto}
	}

	return nil
}

// Effective returns cfg after capability clamps. Nil when there is nothing to send.
// Non-reasoning caps drop intent. !CanDisable turns none into the min accepted level.
func Effective(cfg *Config, caps Caps) *Config {
	if cfg == nil || !caps.Reasoning || caps.Format == FormatNone {
		return nil
	}
	out := *cfg
	if out.Mode == ModeNone && !caps.CanDisable {
		out = Config{Mode: ModeLevel, Level: MinAcceptedLevel(caps)}
	}
	return &out
}

// ResolveIntent applies precedence for one failover attempt:
// suffix override > body/IR intent > required default > absent.
// Does not inject solely because the provider has a dialect.
func ResolveIntent(bodyCfg, suffixCfg *Config, caps Caps) *Config {
	cfg := Merge(bodyCfg, suffixCfg)
	if cfg != nil {
		return Effective(cfg, caps)
	}
	if caps.RequiredDefault != "" && caps.Reasoning && caps.Format != FormatNone {
		return Effective(&Config{Mode: ModeLevel, Level: caps.RequiredDefault}, caps)
	}
	return nil
}

// ApplyWire patches a JSON body: set model, selectively strip recognized
// reasoning members, and write the native shape for the provider dialect.
// formatID selects the transport wire family (oai-chat, anth-msg, oai-responses)
// when the dialect alone is ambiguous (e.g. openai vs responses effort field).
func ApplyWire(formatID string, body []byte, model string, cfg *Config, protocol domain.Protocol, dialect domain.ReasoningDialect) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	m["model"] = model
	stripRecognizedReasoning(m)
	if cfg != nil {
		caps := CapsFor(model, protocol, dialect)
		eff := Effective(cfg, caps)
		if eff != nil && (!isClaudeFormat(caps.Format) || lastMessageIsUser(m)) {
			writeNative(m, eff, caps, formatID, protocol)
		}
	}
	return json.Marshal(m)
}

func lastMessageIsUser(m map[string]any) bool {
	messages, ok := m["messages"].([]any)
	if !ok || len(messages) == 0 {
		return false
	}
	last, ok := messages[len(messages)-1].(map[string]any)
	if !ok {
		return false
	}
	role, _ := last["role"].(string)
	return role == "user"
}

func isClaudeFormat(format Format) bool {
	return format == FormatClaudeAdaptive || format == FormatClaudeBudget
}

// FinalizeBody is the central provider-aware request finalizer shared by unary
// and streaming paths. It resolves suffix/body intent independently per attempt
// and writes native reasoning fields for the target provider dialect.
// When neither suffix nor body carries intent and the dialect has no required
// default, only the model field is rewritten (passthrough invariant).
func FinalizeBody(body []byte, upstreamModel string, formatID string, protocol domain.Protocol, dialect domain.ReasoningDialect) ([]byte, error) {
	base, suffixCfg := ParseSuffix(upstreamModel)
	bodyCfg := Capture(body)
	caps := CapsFor(base, protocol, dialect)
	cfg := ResolveIntent(bodyCfg, suffixCfg, caps)
	if cfg == nil && suffixCfg == nil && bodyCfg == nil {
		// No intent at all: model-only rewrite.
		return rewriteModelOnly(body, base)
	}
	return ApplyWire(formatID, body, base, cfg, protocol, dialect)
}

// FinalizeIR applies suffix override onto an IR request and returns the cleaned
// model plus the effective config for the target provider. Encoders may still
// write thinking from IR; callers that use the central body finalizer after
// encode should clear IR thinking to avoid double-application, or pass the
// resolved config into a dialect-aware encode path.
func FinalizeIR(req *ir.Request, upstreamModel string, protocol domain.Protocol, dialect domain.ReasoningDialect) (base string, cfg *Config) {
	base, suffixCfg := ParseSuffix(upstreamModel)
	req.Model = base
	bodyCfg := FromIR(req.Thinking)
	caps := CapsFor(base, protocol, dialect)
	cfg = ResolveIntent(bodyCfg, suffixCfg, caps)
	req.Thinking = ToIR(cfg)
	return base, cfg
}

func rewriteModelOnly(body []byte, model string) ([]byte, error) {
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(body, &generic); err != nil {
		return nil, err
	}
	generic["model"], _ = json.Marshal(model)
	return json.Marshal(generic)
}

// stripRecognizedReasoning removes only known reasoning members, preserving
// reasoning.summary, output_config siblings, and unknown fields. Objects emptied
// by the strip are deleted.
func stripRecognizedReasoning(m map[string]any) {
	delete(m, "reasoning_effort")
	delete(m, "enable_thinking")
	delete(m, "thinking_budget")

	if r, ok := m["reasoning"].(map[string]any); ok {
		delete(r, "effort")
		if len(r) == 0 {
			delete(m, "reasoning")
		} else {
			m["reasoning"] = r
		}
	}

	if t, ok := m["thinking"].(map[string]any); ok {
		// Drop recognized thinking controls; if anything unknown remains keep the object.
		delete(t, "type")
		delete(t, "budget_tokens")
		if len(t) == 0 {
			delete(m, "thinking")
		} else {
			m["thinking"] = t
		}
	}

	if oc, ok := m["output_config"].(map[string]any); ok {
		delete(oc, "effort")
		if len(oc) == 0 {
			delete(m, "output_config")
		} else {
			m["output_config"] = oc
		}
	}

	// Nested Gemini-style leftovers (not written by our dialects but may arrive).
	if gc, ok := m["generationConfig"].(map[string]any); ok {
		delete(gc, "thinkingConfig")
	}
	delete(m, "thinkingConfig")
}

// writeNative applies the effective config in the provider dialect's wire shape.
func writeNative(m map[string]any, cfg *Config, caps Caps, formatID string, protocol domain.Protocol) {
	if cfg == nil {
		return
	}
	switch caps.Format {
	case FormatOpenAI, FormatGrok:
		writeOpenAIChat(m, cfg, caps, protocol)
	case FormatOpenAIResponses:
		writeOpenAIResponses(m, cfg, caps, protocol)
	case FormatClaudeAdaptive:
		writeClaudeAdaptive(m, cfg)
	case FormatClaudeBudget:
		writeClaudeBudget(m, cfg, caps)
	case FormatKimi:
		writeKimi(m, cfg, caps)
	case FormatQwen:
		writeQwen(m, cfg, caps)
	case FormatDeepSeek:
		writeDeepSeek(m, cfg, caps)
	case FormatZAI:
		writeZAI(m, cfg, caps)
	case FormatCursor, FormatNone:
		// Cursor and protocol-managed formats are handled by their codecs.
	default:
		// Fallback by transport id when format is unset but dialect resolved oddly.
		switch formatID {
		case "oai-chat":
			writeOpenAIChat(m, cfg, caps, protocol)
		case "oai-responses":
			writeOpenAIResponses(m, cfg, caps, protocol)
		case "anth-msg":
			if caps.Format == FormatClaudeAdaptive {
				writeClaudeAdaptive(m, cfg)
			} else {
				writeClaudeBudget(m, cfg, caps)
			}
		}
	}
	_ = protocol
}

func writeOpenAIChat(m map[string]any, cfg *Config, caps Caps, protocol domain.Protocol) {
	if cfg.Mode == ModeNone {
		if caps.CanDisable {
			m["reasoning_effort"] = "none"
		}
		return
	}
	level := LevelFor(cfg)
	if level == "" || level == "none" {
		return
	}
	if protocol == domain.ProtocolOpenAICodex {
		level = NormalizeCodexLevel(level, caps)
	}
	m["reasoning_effort"] = level
}

func writeOpenAIResponses(m map[string]any, cfg *Config, caps Caps, protocol domain.Protocol) {
	r, _ := m["reasoning"].(map[string]any)
	if r == nil {
		r = map[string]any{}
	}
	if cfg.Mode == ModeNone {
		if caps.CanDisable {
			r["effort"] = "none"
			m["reasoning"] = r
		}
		return
	}
	level := LevelFor(cfg)
	if level == "" || level == "none" {
		return
	}
	if protocol == domain.ProtocolOpenAICodex {
		level = NormalizeCodexLevel(level, caps)
	}
	r["effort"] = level
	m["reasoning"] = r
}

func writeClaudeAdaptive(m map[string]any, cfg *Config) {
	if cfg.Mode == ModeNone {
		m["thinking"] = mergeObject(m["thinking"], map[string]any{"type": "disabled"})
		return
	}
	m["thinking"] = mergeObject(m["thinking"], map[string]any{"type": "adaptive"})
	level := cfg.Level
	switch cfg.Mode {
	case ModeBudget:
		level = BudgetToLevel(cfg.Budget)
	case ModeAuto:
		level = "auto"
	}
	level = MapClaudeAdaptiveLevel(level)
	if level == "" {
		level = "medium"
	}
	// Preserve non-effort siblings on output_config.
	oc, _ := m["output_config"].(map[string]any)
	if oc == nil {
		oc = map[string]any{}
	}
	oc["effort"] = level
	m["output_config"] = oc
}

func writeClaudeBudget(m map[string]any, cfg *Config, caps Caps) {
	if cfg.Mode == ModeNone {
		m["thinking"] = mergeObject(m["thinking"], map[string]any{"type": "disabled"})
		return
	}
	if cfg.Mode == ModeAuto {
		m["thinking"] = mergeObject(m["thinking"], map[string]any{"type": "enabled"})
		return
	}
	budget := BudgetFor(cfg, caps.BudgetMin, caps.BudgetMax)
	if budget <= 0 {
		budget = 8192
	}
	m["thinking"] = mergeObject(m["thinking"], map[string]any{"type": "enabled", "budget_tokens": budget})
	// Reconcile max_tokens: thinking tokens count against it.
	ReconcileMaxTokens(m, budget, caps.MaxOutput)
}

// ReconcileMaxTokens ensures max_tokens > budget_tokens with headroom while
// respecting the model output ceiling. When the requested budget reaches the
// ceiling, the budget is reduced so the request remains valid.
func ReconcileMaxTokens(m map[string]any, budget, maxOutput int) {
	if budget <= 0 {
		return
	}
	need := budget + 1024
	if maxOutput > 0 && need > maxOutput {
		need = maxOutput
	}
	cur := 0
	switch v := m["max_tokens"].(type) {
	case float64:
		cur = int(v)
	case int:
		cur = v
	case json.Number:
		if n, err := v.Int64(); err == nil {
			cur = int(n)
		}
	}
	if maxOutput > 0 && cur > maxOutput {
		cur = maxOutput
		m["max_tokens"] = cur
	}
	if cur < need {
		cur = need
		m["max_tokens"] = cur
	}
	if budget >= cur {
		budget = max(1024, cur-1024)
		if t, ok := m["thinking"].(map[string]any); ok {
			t["budget_tokens"] = budget
		}
	}
}

// ReconcileMaxTokensIR bumps ir.Request.MaxTokens for budget thinking.
func ReconcileMaxTokensIR(req *ir.Request, budget, maxOutput int) {
	if req == nil || budget <= 0 {
		return
	}
	need := budget + 1024
	if maxOutput > 0 && need > maxOutput {
		need = maxOutput
	}
	if req.MaxTokens < need {
		req.MaxTokens = need
	}
}

func writeKimi(m map[string]any, cfg *Config, caps Caps) {
	if cfg.Mode == ModeNone {
		if caps.CanDisable {
			m["thinking"] = mergeObject(m["thinking"], map[string]any{"type": "disabled"})
		}
		return
	}
	level := LevelFor(cfg)
	if level == "auto" {
		level = "high"
	}
	effort := MapKimiLevel(level)
	if effort == "" {
		effort = "high"
	}
	// Kimi uses reasoning_effort on OpenAI transport; thinking disabled on Claude.
	m["reasoning_effort"] = effort
}

func writeQwen(m map[string]any, cfg *Config, caps Caps) {
	if cfg.Mode == ModeNone {
		if caps.CanDisable {
			m["enable_thinking"] = false
		}
		return
	}
	m["enable_thinking"] = true
	budget := BudgetFor(cfg, caps.BudgetMin, caps.BudgetMax)
	if budget > 0 {
		m["thinking_budget"] = budget
	}
}

func writeDeepSeek(m map[string]any, cfg *Config, caps Caps) {
	if cfg.Mode == ModeNone {
		if caps.CanDisable {
			m["thinking"] = mergeObject(m["thinking"], map[string]any{"type": "disabled"})
		}
		return
	}
	m["thinking"] = mergeObject(m["thinking"], map[string]any{"type": "enabled"})
	level := LevelFor(cfg)
	m["reasoning_effort"] = MapDeepSeekLevel(level)
}

func writeZAI(m map[string]any, cfg *Config, caps Caps) {
	if cfg.Mode == ModeNone {
		if caps.CanDisable {
			m["enable_thinking"] = false
			delete(m, "thinking")
		}
		return
	}
	m["thinking"] = mergeObject(m["thinking"], map[string]any{"type": "enabled"})
}

func mergeObject(existing any, fields map[string]any) map[string]any {
	out, _ := existing.(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	for key, value := range fields {
		out[key] = value
	}
	return out
}

// WriteAnthropic applies thinking onto an Anthropic-shaped map (tests + passthrough).
func WriteAnthropic(m map[string]any, cfg *Config, caps Caps) {
	if cfg == nil {
		return
	}
	if caps.Format == FormatClaudeAdaptive {
		writeClaudeAdaptive(m, cfg)
		return
	}
	writeClaudeBudget(m, cfg, caps)
}
