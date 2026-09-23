package thinking

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"

	"airouter/internal/domain"
)

func TestParseSuffix(t *testing.T) {
	cases := []struct {
		in       string
		wantBase string
		wantMode Mode
		wantLvl  string
		wantBud  int
		wantNil  bool
	}{
		{"gpt-5(high)", "gpt-5", ModeLevel, "high", 0, false},
		{"model(8192)", "model", ModeBudget, "", 8192, false},
		{"m(none)", "m", ModeNone, "", 0, false},
		{"m(off)", "m", ModeNone, "", 0, false},
		{"m(auto)", "m", ModeAuto, "", 0, false},
		{"claude-opus-4.7", "claude-opus-4.7", "", "", 0, true},
		{"gpt-5(test)", "gpt-5(test)", "", "", 0, true}, // unknown token kept
		{"gpt-5(minimal)", "gpt-5", ModeLevel, "minimal", 0, false},
		{"x(max)", "x", ModeLevel, "max", 0, false},
		{"gpt-5.6-sol(ultra)", "gpt-5.6-sol", ModeLevel, "ultra", 0, false},
		{"gpt-5(qwen:high)", "gpt-5(qwen:high)", "", "", 0, true}, // dialect-qualified not parsed
		{"gpt-5(spark)", "gpt-5(spark)", "", "", 0, true},
		{"", "", "", "", 0, true},
	}
	for _, tc := range cases {
		base, cfg := ParseSuffix(tc.in)
		if base != tc.wantBase {
			t.Errorf("ParseSuffix(%q) base=%q want %q", tc.in, base, tc.wantBase)
		}
		if tc.wantNil {
			if cfg != nil {
				t.Errorf("ParseSuffix(%q) cfg=%+v want nil", tc.in, cfg)
			}
			continue
		}
		if cfg == nil || cfg.Mode != tc.wantMode || cfg.Level != tc.wantLvl || cfg.Budget != tc.wantBud {
			t.Errorf("ParseSuffix(%q) cfg=%+v", tc.in, cfg)
		}
	}
}

func TestBudgetToLevel(t *testing.T) {
	if BudgetToLevel(0) != "" {
		t.Fatal("0")
	}
	if BudgetToLevel(512) != "minimal" {
		t.Fatal("512")
	}
	if BudgetToLevel(1024) != "low" {
		t.Fatal("1024")
	}
	if BudgetToLevel(8192) != "medium" {
		t.Fatal("8192")
	}
	if BudgetToLevel(24576) != "high" {
		t.Fatal("24576")
	}
	if BudgetToLevel(100000) != "xhigh" {
		t.Fatal("100000")
	}
}

func TestLevelForPassThrough(t *testing.T) {
	for _, lvl := range []string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"} {
		if got := LevelFor(&Config{Mode: ModeLevel, Level: lvl}); got != lvl {
			t.Fatalf("LevelFor(%q) = %q", lvl, got)
		}
	}
	if LevelFor(&Config{Mode: ModeNone}) != "none" {
		t.Fatal("ModeNone")
	}
}

func TestFromAnthropicPriority(t *testing.T) {
	cfg := FromAnthropic("enabled", 4096, "high")
	if cfg == nil || cfg.Mode != ModeLevel || cfg.Level != "high" {
		t.Fatalf("output_config should win: %+v", cfg)
	}
	cfg = FromAnthropic("disabled", 0, "")
	if cfg == nil || cfg.Mode != ModeNone {
		t.Fatalf("disabled: %+v", cfg)
	}
	cfg = FromAnthropic("enabled", 8192, "")
	if cfg == nil || cfg.Mode != ModeBudget || cfg.Budget != 8192 {
		t.Fatalf("budget: %+v", cfg)
	}
}

func TestMerge(t *testing.T) {
	base := &Config{Mode: ModeLevel, Level: "low"}
	over := &Config{Mode: ModeLevel, Level: "high"}
	if Merge(base, over).Level != "high" {
		t.Fatal("override")
	}
	if Merge(base, nil).Level != "low" {
		t.Fatal("base")
	}
}

func TestEffectiveCanDisable(t *testing.T) {
	caps := Caps{Reasoning: true, CanDisable: false, Format: FormatOpenAI}
	cfg := Effective(&Config{Mode: ModeNone}, caps)
	if cfg == nil || cfg.Mode != ModeLevel || cfg.Level != "minimal" {
		t.Fatalf("clamp none: %+v", cfg)
	}
	caps.Reasoning = false
	if Effective(&Config{Mode: ModeLevel, Level: "high"}, caps) != nil {
		t.Fatal("non-reasoning should drop")
	}
}

func TestCapsForDialects(t *testing.T) {
	// Claude adaptive by model, not by Claude Code protocol alone.
	c := CapsFor("claude-opus-4-7", domain.ProtocolAnthropic, domain.ReasoningClaude)
	if c.Format != FormatClaudeAdaptive {
		t.Fatalf("opus format=%v", c.Format)
	}
	c = CapsFor("claude-haiku-4.5", domain.ProtocolAnthropic, domain.ReasoningClaude)
	if c.Format != FormatClaudeBudget {
		t.Fatalf("haiku format=%v", c.Format)
	}
	c = CapsFor("claude-haiku-4.5", domain.ProtocolClaudeCode, domain.ReasoningClaude)
	if c.Format != FormatClaudeBudget {
		t.Fatalf("claude-code haiku should be budget, got %v", c.Format)
	}
	c = CapsFor("claude-sonnet-5", domain.ProtocolClaudeCode, domain.ReasoningClaude)
	if c.Format != FormatClaudeAdaptive {
		t.Fatalf("claude-code sonnet-5 format=%v", c.Format)
	}

	// Dialect drives format on OpenAI transport.
	c = CapsFor("qwen3", domain.ProtocolOpenAI, domain.ReasoningQwen)
	if c.Format != FormatQwen {
		t.Fatalf("qwen format=%v", c.Format)
	}
	c = CapsFor("kimi-k2.5", domain.ProtocolOpenAI, domain.ReasoningKimi)
	if c.Format != FormatKimi {
		t.Fatalf("kimi format=%v", c.Format)
	}
	c = CapsFor("deepseek-v4", domain.ProtocolOpenAI, domain.ReasoningDeepSeek)
	if c.Format != FormatDeepSeek {
		t.Fatalf("deepseek format=%v", c.Format)
	}
	c = CapsFor("glm-5", domain.ProtocolOpenAI, domain.ReasoningZAI)
	if c.Format != FormatZAI {
		t.Fatalf("zai format=%v", c.Format)
	}
	c = CapsFor("grok-4", domain.ProtocolOpenAI, domain.ReasoningGrok)
	if c.Format != FormatGrok || !c.Reasoning || c.CanDisable {
		t.Fatalf("grok-4 fail-open caps=%+v", c)
	}
	c = CapsFor("grok-4.7", domain.ProtocolOpenAIResponses, domain.ReasoningGrok)
	if c.Format != FormatOpenAIResponses {
		t.Fatalf("grok responses format=%v", c.Format)
	}

	// Explicit none disables writer.
	c = CapsFor("gpt-5", domain.ProtocolOpenAI, domain.ReasoningNone)
	if c.Reasoning || c.Format != FormatNone {
		t.Fatalf("none dialect: %+v", c)
	}

	// Codex cannot disable classic models.
	c = CapsFor("gpt-5.3-codex", domain.ProtocolOpenAICodex, domain.ReasoningCodex)
	if c.CanDisable || c.RequiredDefault != "low" {
		t.Fatalf("codex caps: %+v", c)
	}

	// Cursor stays native.
	c = CapsFor("any", domain.ProtocolCursor, domain.ReasoningNone)
	if c.Format != FormatCursor {
		t.Fatalf("cursor format=%v", c.Format)
	}
}

func TestCapsForKnownNonReasoners(t *testing.T) {
	cases := []struct {
		model   string
		proto   domain.Protocol
		dialect domain.ReasoningDialect
	}{
		{"claude-3-opus", domain.ProtocolAnthropic, domain.ReasoningClaude},
		{"gpt-4.1", domain.ProtocolOpenAI, domain.ReasoningOpenAI},
		{"deepseek-chat", domain.ProtocolOpenAI, domain.ReasoningDeepSeek},
		{"grok-4-image", domain.ProtocolOpenAI, domain.ReasoningGrok},
	}
	for _, tc := range cases {
		caps := CapsFor(tc.model, tc.proto, tc.dialect)
		if caps.Reasoning || caps.Format != FormatNone {
			t.Errorf("%s caps = %+v", tc.model, caps)
		}
	}
}

func TestNormalizeCodexMax(t *testing.T) {
	cases := []struct {
		name  string
		model string
		level string
		want  string
	}{
		{"openai max stays xhigh without allow", "gpt-5", "max", "xhigh"},
		{"classic ultra", "gpt-5.3-codex", "ultra", "xhigh"},
		{"classic auto", "gpt-5.3-codex", "auto", "medium"},
		{"luna ultra", "gpt-5.6-luna", "ultra", "max"},
		{"luna auto", "gpt-5.6-luna", "auto", "medium"},
		{"sol ultra", "gpt-5.6-sol", "ultra", "ultra"},
		{"terra ultra", "gpt-5.6-terra", "ultra", "ultra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caps := CapsFor(tc.model, domain.ProtocolOpenAICodex, domain.ReasoningCodex)
			if tc.model == "gpt-5" {
				caps = CapsFor(tc.model, domain.ProtocolOpenAI, domain.ReasoningOpenAI)
			}
			if got := NormalizeCodexLevel(tc.level, caps); got != tc.want {
				t.Fatalf("NormalizeCodexLevel(%q) = %q, want %q", tc.level, got, tc.want)
			}
		})
	}
	caps := CapsFor("gpt-5", domain.ProtocolOpenAI, domain.ReasoningOpenAI)
	caps.AllowMax = true
	if got := NormalizeCodexLevel("max", caps); got != "max" {
		t.Fatalf("allow max = %q", got)
	}
}

func TestApplyWireOpenAIMaxPolicy(t *testing.T) {
	cases := []struct {
		name     string
		formatID string
		protocol domain.Protocol
		dialect  domain.ReasoningDialect
		model    string
		level    string
		want     string
	}{
		{"chat compatible preserves max", "oai-chat", domain.ProtocolOpenAI, domain.ReasoningOpenAI, "cline-pass/deepseek-v4-pro", "max", "max"},
		{"chat compatible preserves ultra", "oai-chat", domain.ProtocolOpenAI, domain.ReasoningOpenAI, "hosted-reasoner", "ultra", "ultra"},
		{"responses compatible preserves max", "oai-responses", domain.ProtocolOpenAIResponses, domain.ReasoningOpenAI, "hosted-reasoner", "max", "max"},
		{"classic codex clamps max", "oai-codex", domain.ProtocolOpenAICodex, domain.ReasoningCodex, "gpt-5.3-codex", "max", "xhigh"},
		{"classic codex maps ultra", "oai-codex", domain.ProtocolOpenAICodex, domain.ReasoningCodex, "gpt-5.3-codex", "ultra", "xhigh"},
		{"expanded codex preserves max", "oai-codex", domain.ProtocolOpenAICodex, domain.ReasoningCodex, "gpt-5.6-luna", "max", "max"},
		{"max-only codex maps ultra", "oai-codex", domain.ProtocolOpenAICodex, domain.ReasoningCodex, "gpt-5.6-luna", "ultra", "max"},
		{"expanded codex preserves ultra", "oai-codex", domain.ProtocolOpenAICodex, domain.ReasoningCodex, "gpt-5.6-sol", "ultra", "ultra"},
		{"terra codex preserves ultra", "oai-codex", domain.ProtocolOpenAICodex, domain.ReasoningCodex, "gpt-5.6-terra", "ultra", "ultra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ApplyWire(tc.formatID, []byte(`{"model":"combo","messages":[]}`), tc.model, &Config{Mode: ModeLevel, Level: tc.level}, tc.protocol, tc.dialect)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			effort, _ := got["reasoning_effort"].(string)
			if reasoning, ok := got["reasoning"].(map[string]any); ok {
				effort, _ = reasoning["effort"].(string)
			}
			if effort != tc.want {
				t.Fatalf("effort = %q, want %q; body=%s", effort, tc.want, out)
			}
		})
	}
}

func TestMapLevels(t *testing.T) {
	if MapClaudeAdaptiveLevel("minimal") != "low" || MapClaudeAdaptiveLevel("xhigh") != "high" || MapClaudeAdaptiveLevel("max") != "max" {
		t.Fatal("claude adaptive map")
	}
	if MapKimiLevel("minimal") != "low" || MapKimiLevel("xhigh") != "max" {
		t.Fatal("kimi map")
	}
	if MapDeepSeekLevel("low") != "high" || MapDeepSeekLevel("xhigh") != "max" {
		t.Fatal("deepseek map")
	}
}

func TestReconcileClaudeBudgetAtOutputCeiling(t *testing.T) {
	body := []byte(`{"model":"claude-haiku-4.5","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := ApplyWire("anth-msg", body, "claude-haiku-4.5", &Config{Mode: ModeLevel, Level: "max"}, domain.ProtocolAnthropic, domain.ReasoningClaude)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		MaxTokens int `json:"max_tokens"`
		Thinking  struct {
			Budget int `json:"budget_tokens"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.MaxTokens != 64000 || got.Thinking.Budget != 62976 || got.Thinking.Budget >= got.MaxTokens {
		t.Fatalf("invalid ceiling reconciliation: %+v body=%s", got, out)
	}
}

func TestCapture(t *testing.T) {
	cases := []struct {
		body string
		mode Mode
		lvl  string
		bud  int
	}{
		{`{"reasoning_effort":"high"}`, ModeLevel, "high", 0},
		{`{"reasoning":{"effort":"low","summary":"auto"}}`, ModeLevel, "low", 0},
		{`{"thinking":{"type":"enabled","budget_tokens":4096}}`, ModeBudget, "", 4096},
		{`{"thinking":{"type":"disabled"}}`, ModeNone, "", 0},
		{`{"output_config":{"effort":"max"},"thinking":{"type":"adaptive"}}`, ModeLevel, "max", 0},
		{`{"enable_thinking":true,"thinking_budget":2048}`, ModeBudget, "", 2048},
		{`{"enable_thinking":false}`, ModeNone, "", 0},
		{`{"model":"x"}`, "", "", 0},
	}
	for _, tc := range cases {
		cfg := Capture([]byte(tc.body))
		if tc.mode == "" {
			if cfg != nil {
				t.Errorf("Capture(%s) = %+v want nil", tc.body, cfg)
			}
			continue
		}
		if cfg == nil || cfg.Mode != tc.mode || cfg.Level != tc.lvl || cfg.Budget != tc.bud {
			t.Errorf("Capture(%s) = %+v", tc.body, cfg)
		}
	}
}

func TestApplyWireOpenAI(t *testing.T) {
	body := []byte(`{"model":"x","messages":[],"temperature":0.5,"thinking":{"type":"enabled"},"reasoning":{"summary":"auto","effort":"low"}}`)
	out, err := ApplyWire("oai-chat", body, "gpt-5", &Config{Mode: ModeLevel, Level: "high"}, domain.ProtocolOpenAI, domain.ReasoningOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "gpt-5" {
		t.Fatalf("model=%v", m["model"])
	}
	if m["reasoning_effort"] != "high" {
		t.Fatalf("effort=%v", m["reasoning_effort"])
	}
	if _, ok := m["thinking"]; ok {
		t.Fatal("thinking should be stripped")
	}
	// summary preserved on reasoning object when only effort removed then re-written
	// for chat format reasoning object may be gone; temperature kept.
	if m["temperature"] != 0.5 {
		t.Fatal("foreign field lost")
	}
}

func TestApplyWirePreservesSummaryAndOutputConfigSiblings(t *testing.T) {
	body := []byte(`{"model":"x","reasoning":{"effort":"low","summary":"auto"},"output_config":{"effort":"low","format":{"type":"json_object"}},"foo":1}`)
	out, err := ApplyWire("oai-responses", body, "gpt-5", &Config{Mode: ModeLevel, Level: "high"}, domain.ProtocolOpenAIResponses, domain.ReasoningOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	r := m["reasoning"].(map[string]any)
	if r["effort"] != "high" || r["summary"] != "auto" {
		t.Fatalf("reasoning=%v", r)
	}
	oc := m["output_config"].(map[string]any)
	if _, ok := oc["effort"]; ok {
		t.Fatalf("effort should be stripped from output_config on openai dialect: %v", oc)
	}
	if oc["format"] == nil {
		t.Fatal("output_config.format lost")
	}
	if m["foo"] != float64(1) {
		t.Fatal("foo lost")
	}
}

func TestClaudeWriterRequiresFinalUserMessage(t *testing.T) {
	body := []byte(`{"model":"combo","messages":[{"role":"assistant","content":"continue"}]}`)
	out, err := ApplyWire("anth-msg", body, "claude-haiku-4.5", &Config{Mode: ModeLevel, Level: "high"}, domain.ProtocolAnthropic, domain.ReasoningClaude)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["thinking"]; ok {
		t.Fatalf("thinking written after assistant turn: %s", out)
	}
}

func TestApplyWireDialectMatrix(t *testing.T) {
	body := []byte(`{"model":"combo","messages":[{"role":"user","content":"hi"}]}`)
	cases := []struct {
		name     string
		dialect  domain.ReasoningDialect
		proto    domain.Protocol
		formatID string
		model    string
		cfg      *Config
		check    func(t *testing.T, m map[string]any)
	}{
		{
			name: "qwen-enabled", dialect: domain.ReasoningQwen, proto: domain.ProtocolOpenAI, formatID: "oai-chat",
			model: "qwen3", cfg: &Config{Mode: ModeLevel, Level: "high"},
			check: func(t *testing.T, m map[string]any) {
				if m["enable_thinking"] != true {
					t.Fatalf("enable_thinking=%v", m["enable_thinking"])
				}
				if int(m["thinking_budget"].(float64)) != 24576 {
					t.Fatalf("budget=%v", m["thinking_budget"])
				}
			},
		},
		{
			name: "qwen-disabled", dialect: domain.ReasoningQwen, proto: domain.ProtocolOpenAI, formatID: "oai-chat",
			model: "qwen3", cfg: &Config{Mode: ModeNone},
			check: func(t *testing.T, m map[string]any) {
				if m["enable_thinking"] != false {
					t.Fatalf("enable_thinking=%v", m["enable_thinking"])
				}
			},
		},
		{
			name: "deepseek-high", dialect: domain.ReasoningDeepSeek, proto: domain.ProtocolOpenAI, formatID: "oai-chat",
			model: "deepseek-v4", cfg: &Config{Mode: ModeLevel, Level: "low"},
			check: func(t *testing.T, m map[string]any) {
				th := m["thinking"].(map[string]any)
				if th["type"] != "enabled" {
					t.Fatalf("thinking=%v", th)
				}
				if m["reasoning_effort"] != "high" {
					t.Fatalf("effort=%v", m["reasoning_effort"])
				}
			},
		},
		{
			name: "deepseek-max", dialect: domain.ReasoningDeepSeek, proto: domain.ProtocolOpenAI, formatID: "oai-chat",
			model: "deepseek-v4", cfg: &Config{Mode: ModeLevel, Level: "max"},
			check: func(t *testing.T, m map[string]any) {
				if m["reasoning_effort"] != "max" {
					t.Fatalf("effort=%v", m["reasoning_effort"])
				}
			},
		},
		{
			name: "zai-enabled", dialect: domain.ReasoningZAI, proto: domain.ProtocolOpenAI, formatID: "oai-chat",
			model: "glm-5", cfg: &Config{Mode: ModeLevel, Level: "high"},
			check: func(t *testing.T, m map[string]any) {
				th := m["thinking"].(map[string]any)
				if th["type"] != "enabled" {
					t.Fatalf("thinking=%v", th)
				}
			},
		},
		{
			name: "zai-disabled", dialect: domain.ReasoningZAI, proto: domain.ProtocolOpenAI, formatID: "oai-chat",
			model: "glm-5", cfg: &Config{Mode: ModeNone},
			check: func(t *testing.T, m map[string]any) {
				if m["enable_thinking"] != false {
					t.Fatalf("enable_thinking=%v", m["enable_thinking"])
				}
			},
		},
		{
			name: "kimi", dialect: domain.ReasoningKimi, proto: domain.ProtocolOpenAI, formatID: "oai-chat",
			model: "kimi-k2.5", cfg: &Config{Mode: ModeLevel, Level: "minimal"},
			check: func(t *testing.T, m map[string]any) {
				if m["reasoning_effort"] != "low" {
					t.Fatalf("effort=%v", m["reasoning_effort"])
				}
			},
		},
		{
			name: "claude-adaptive-max", dialect: domain.ReasoningClaude, proto: domain.ProtocolAnthropic, formatID: "anth-msg",
			model: "claude-opus-4-7", cfg: &Config{Mode: ModeLevel, Level: "max"},
			check: func(t *testing.T, m map[string]any) {
				th := m["thinking"].(map[string]any)
				if th["type"] != "adaptive" {
					t.Fatalf("thinking=%v", th)
				}
				oc := m["output_config"].(map[string]any)
				if oc["effort"] != "max" {
					t.Fatalf("effort=%v want max preserved", oc["effort"])
				}
			},
		},
		{
			name: "openai-max-preserved", dialect: domain.ReasoningOpenAI, proto: domain.ProtocolOpenAI, formatID: "oai-chat",
			model: "gpt-5", cfg: &Config{Mode: ModeLevel, Level: "max"},
			check: func(t *testing.T, m map[string]any) {
				if m["reasoning_effort"] != "max" {
					t.Fatalf("effort=%v want max", m["reasoning_effort"])
				}
			},
		},
		{
			name: "grok-max-to-xhigh", dialect: domain.ReasoningGrok, proto: domain.ProtocolOpenAI, formatID: "oai-chat",
			model: "grok-4.7", cfg: &Config{Mode: ModeLevel, Level: "max"},
			check: func(t *testing.T, m map[string]any) {
				if m["reasoning_effort"] != "xhigh" {
					t.Fatalf("effort=%v want xhigh", m["reasoning_effort"])
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ApplyWire(tc.formatID, body, tc.model, tc.cfg, tc.proto, tc.dialect)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatal(err)
			}
			tc.check(t, m)
		})
	}
}

func TestFinalizeBodyNoIntentModelOnly(t *testing.T) {
	body := []byte(`{"model":"combo","messages":[],"foo":1,"temperature":0.2}`)
	out, err := FinalizeBody(body, "gpt-5", "oai-chat", domain.ProtocolOpenAI, domain.ReasoningOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "gpt-5" || m["foo"] != float64(1) || m["temperature"] != 0.2 {
		t.Fatalf("model-only broken: %s", out)
	}
	if _, ok := m["reasoning_effort"]; ok {
		t.Fatal("should not inject without intent")
	}
}

func TestFinalizeBodySuffixOverridesBody(t *testing.T) {
	body := []byte(`{"model":"combo","reasoning_effort":"low"}`)
	out, err := FinalizeBody(body, "gpt-5(high)", "oai-chat", domain.ProtocolOpenAI, domain.ReasoningOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "gpt-5" || m["reasoning_effort"] != "high" {
		t.Fatalf("suffix override: %s", out)
	}
}

func TestFinalizeBodyBodyWithoutSuffix(t *testing.T) {
	body := []byte(`{"model":"combo","enable_thinking":true,"thinking_budget":1024}`)
	out, err := FinalizeBody(body, "qwen3", "oai-chat", domain.ProtocolOpenAI, domain.ReasoningQwen)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["enable_thinking"] != true {
		t.Fatalf("%s", out)
	}
}

func TestResolveIntentNoInjectionFromDialectAlone(t *testing.T) {
	caps := CapsFor("gpt-5", domain.ProtocolOpenAI, domain.ReasoningOpenAI)
	if ResolveIntent(nil, nil, caps, domain.ReasoningOpenAI) != nil {
		t.Fatal("no required default should stay nil")
	}
	caps = CapsFor("gpt-5.3-codex", domain.ProtocolOpenAICodex, domain.ReasoningCodex)
	cfg := ResolveIntent(nil, nil, caps, domain.ReasoningCodex)
	if cfg == nil || cfg.Level != "low" {
		t.Fatalf("codex required default: %+v", cfg)
	}
}

func TestCapsForOpencodeMiniMaxAndMiMo(t *testing.T) {
	cases := []struct {
		model      string
		reasoning  bool
		format     Format
		canDisable bool
		maxOutput  int
	}{
		{"minimax-m3", true, FormatMiniMax, true, 131072},
		{"MiniMax-M3-highspeed", true, FormatMiniMax, true, 131072},
		{"minimax-m2.7", true, FormatMiniMax, false, 131072},
		{"minimax-m2.5", true, FormatMiniMax, false, 131072},
		{"minimax-m2", true, FormatMiniMax, false, 131072},
		{"mimo-v2.5-pro", false, FormatNone, true, 131072},
		{"xiaomi/mimo-v2.5", false, FormatNone, true, 131072},
		{"glm-5", true, FormatZAI, true, 128000},
	}
	for _, tc := range cases {
		caps := CapsFor(tc.model, domain.ProtocolOpencode, domain.ReasoningOpencode)
		if caps.Reasoning != tc.reasoning || caps.Format != tc.format || caps.CanDisable != tc.canDisable {
			t.Errorf("%s caps = %+v", tc.model, caps)
		}
		if tc.maxOutput != 0 && caps.MaxOutput != tc.maxOutput {
			t.Errorf("%s MaxOutput = %d want %d", tc.model, caps.MaxOutput, tc.maxOutput)
		}
		if caps.Format == FormatMiniMax && !slices.Equal(caps.Levels, []string{"none", "thinking"}) {
			t.Errorf("%s levels = %v", tc.model, caps.Levels)
		}
	}
}

func TestApplyWireMiniMaxAdaptive(t *testing.T) {
	body := []byte(`{"model":"combo","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low","enable_thinking":true,"thinking":{"type":"enabled","vendor":"keep"},"output_config":{"effort":"low","format":{"type":"json_object"}}}`)
	cases := []struct {
		name     string
		model    string
		cfg      *Config
		wantType string
	}{
		{"m3-high", "minimax-m3", &Config{Mode: ModeLevel, Level: "high"}, "adaptive"},
		{"m3-auto", "minimax-m3", &Config{Mode: ModeAuto}, "adaptive"},
		{"m3-budget", "minimax-m3", &Config{Mode: ModeBudget, Budget: 8192}, "adaptive"},
		{"m3-none", "minimax-m3", &Config{Mode: ModeNone}, "disabled"},
		{"m27-none-clamped", "minimax-m2.7", &Config{Mode: ModeNone}, "adaptive"},
		{"m25-high", "minimax-m2.5", &Config{Mode: ModeLevel, Level: "high"}, "adaptive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ApplyWire("oai-chat", body, tc.model, tc.cfg, domain.ProtocolOpencode, domain.ReasoningOpencode)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatal(err)
			}
			th, _ := m["thinking"].(map[string]any)
			if th == nil || th["type"] != tc.wantType {
				t.Fatalf("thinking=%v want type %q body=%s", th, tc.wantType, out)
			}
			if th["vendor"] != "keep" {
				t.Fatalf("thinking sibling lost: %v", th)
			}
			if _, ok := m["enable_thinking"]; ok {
				t.Fatalf("enable_thinking leaked: %s", out)
			}
			if _, ok := m["reasoning_effort"]; ok {
				t.Fatalf("reasoning_effort leaked: %s", out)
			}
			if th["type"] == "enabled" {
				t.Fatalf("minimax must not emit type enabled: %s", out)
			}
			if oc, ok := m["output_config"].(map[string]any); ok {
				if _, has := oc["effort"]; has {
					t.Fatalf("output_config.effort leaked: %s", out)
				}
				if oc["format"] == nil {
					t.Fatalf("output_config sibling lost: %s", out)
				}
			}
		})
	}
}

func TestEffectiveMiniMaxCannotDisable(t *testing.T) {
	caps := CapsFor("minimax-m2.7", domain.ProtocolOpencode, domain.ReasoningOpencode)
	cfg := Effective(&Config{Mode: ModeNone}, caps)
	if cfg == nil || cfg.Mode == ModeNone {
		t.Fatalf("m2.7 none should clamp: %+v", cfg)
	}
	caps = CapsFor("minimax-m3", domain.ProtocolOpencode, domain.ReasoningOpencode)
	cfg = Effective(&Config{Mode: ModeNone}, caps)
	if cfg == nil || cfg.Mode != ModeNone {
		t.Fatalf("m3 none should stay none: %+v", cfg)
	}
}

func TestFinalizeBodyCodexAutoAndUltra(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		model      string
		protocol   domain.Protocol
		dialect    domain.ReasoningDialect
		wantModel  string
		wantEffort string
	}{
		{
			name:       "codex explicit auto",
			body:       `{"model":"combo","reasoning":{"effort":"auto"}}`,
			model:      "gpt-5.3-codex",
			protocol:   domain.ProtocolOpenAICodex,
			dialect:    domain.ReasoningCodex,
			wantModel:  "gpt-5.3-codex",
			wantEffort: "medium",
		},
		{
			name:       "codex no intent defaults low",
			body:       `{"model":"combo"}`,
			model:      "gpt-5.3-codex",
			protocol:   domain.ProtocolOpenAICodex,
			dialect:    domain.ReasoningCodex,
			wantModel:  "gpt-5.3-codex",
			wantEffort: "low",
		},
		{
			name:       "sol ultra suffix",
			body:       `{"model":"combo"}`,
			model:      "gpt-5.6-sol(ultra)",
			protocol:   domain.ProtocolOpenAICodex,
			dialect:    domain.ReasoningCodex,
			wantModel:  "gpt-5.6-sol",
			wantEffort: "ultra",
		},
		{
			name:       "luna ultra suffix",
			body:       `{"model":"combo"}`,
			model:      "gpt-5.6-luna(ultra)",
			protocol:   domain.ProtocolOpenAICodex,
			dialect:    domain.ReasoningCodex,
			wantModel:  "gpt-5.6-luna",
			wantEffort: "max",
		},
		{
			name:       "classic ultra suffix",
			body:       `{"model":"combo"}`,
			model:      "gpt-5.3-codex(ultra)",
			protocol:   domain.ProtocolOpenAICodex,
			dialect:    domain.ReasoningCodex,
			wantModel:  "gpt-5.3-codex",
			wantEffort: "xhigh",
		},
		{
			name:       "generic openai auto unchanged",
			body:       `{"model":"combo","reasoning_effort":"auto"}`,
			model:      "gpt-5",
			protocol:   domain.ProtocolOpenAI,
			dialect:    domain.ReasoningOpenAI,
			wantModel:  "gpt-5",
			wantEffort: "auto",
		},
		{
			name:       "generic openai ultra unchanged",
			body:       `{"model":"combo"}`,
			model:      "gpt-5(ultra)",
			protocol:   domain.ProtocolOpenAI,
			dialect:    domain.ReasoningOpenAI,
			wantModel:  "gpt-5",
			wantEffort: "ultra",
		},
		{
			name:       "generic openai max unchanged",
			body:       `{"model":"combo","reasoning_effort":"max"}`,
			model:      "gpt-5",
			protocol:   domain.ProtocolOpenAI,
			dialect:    domain.ReasoningOpenAI,
			wantModel:  "gpt-5",
			wantEffort: "max",
		},
		{
			name:       "generic responses auto unchanged",
			body:       `{"model":"combo","reasoning":{"effort":"auto"}}`,
			model:      "gpt-5",
			protocol:   domain.ProtocolOpenAIResponses,
			dialect:    domain.ReasoningOpenAI,
			wantModel:  "gpt-5",
			wantEffort: "auto",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			formatID := "oai-chat"
			if tc.protocol == domain.ProtocolOpenAICodex || tc.protocol == domain.ProtocolOpenAIResponses {
				formatID = "oai-responses"
			}
			out, err := FinalizeBody([]byte(tc.body), tc.model, formatID, tc.protocol, tc.dialect)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if got["model"] != tc.wantModel {
				t.Fatalf("model = %v, want %q; body=%s", got["model"], tc.wantModel, out)
			}
			effort, _ := got["reasoning_effort"].(string)
			if reasoning, ok := got["reasoning"].(map[string]any); ok {
				effort, _ = reasoning["effort"].(string)
			}
			if effort != tc.wantEffort {
				t.Fatalf("effort = %q, want %q; body=%s", effort, tc.wantEffort, out)
			}
		})
	}
}

func TestApplyWirePreservesNestedLargeNumbers(t *testing.T) {
	body := []byte(`{"model":"combo","reasoning":{"effort":"low"},"tools":[{"parameters":{"maximum":9007199254740993,"huge":1e400}}]}`)
	out, err := ApplyWire("oai-responses", body, "gpt-5.3-codex", &Config{Mode: ModeLevel, Level: "high"}, domain.ProtocolOpenAICodex, domain.ReasoningCodex)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("9007199254740993")) {
		t.Fatalf("lost integer token: %s", out)
	}
	if !bytes.Contains(out, []byte("1e400")) {
		t.Fatalf("lost 1e400 token: %s", out)
	}
	var got struct {
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Reasoning.Effort != "high" {
		t.Fatalf("reasoning = %+v", got.Reasoning)
	}
}

func TestFinalizeBodyPreservesNestedLargeNumbers(t *testing.T) {
	body := []byte(`{"model":"combo","reasoning":{"effort":"low"},"tools":[{"parameters":{"maximum":9007199254740993,"huge":1e400}}]}`)
	out, err := FinalizeBody(body, "gpt-5.3-codex", "oai-responses", domain.ProtocolOpenAICodex, domain.ReasoningCodex)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("9007199254740993")) {
		t.Fatalf("lost integer token: %s", out)
	}
	if !bytes.Contains(out, []byte("1e400")) {
		t.Fatalf("lost 1e400 token: %s", out)
	}
	var got struct {
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Reasoning.Effort != "low" {
		t.Fatalf("reasoning = %+v, want effort low", got.Reasoning)
	}
}

func TestDecodeObjectUseNumberStrict(t *testing.T) {
	t.Run("trailing data", func(t *testing.T) {
		if _, err := decodeObjectUseNumber([]byte(`{"a":1} {"b":2}`)); err == nil {
			t.Fatal("expected trailing-data error")
		}
	})
	t.Run("top-level null", func(t *testing.T) {
		if _, err := decodeObjectUseNumber([]byte("null")); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("non-object", func(t *testing.T) {
		if _, err := decodeObjectUseNumber([]byte(`[1]`)); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("trailing whitespace ok", func(t *testing.T) {
		m, err := decodeObjectUseNumber([]byte("{\"n\":9007199254740993}  \n\t "))
		if err != nil {
			t.Fatal(err)
		}
		n, ok := m["n"].(json.Number)
		if !ok || n.String() != "9007199254740993" {
			t.Fatalf("n = %T %v", m["n"], m["n"])
		}
	})
}

func TestGrokCapsFamilies(t *testing.T) {
	tests := []struct {
		model      string
		reasoning  bool
		canDisable bool
		levels     []string
	}{
		{"grok-4.5-latest", true, false, []string{"low", "medium", "high"}},
		{"grok-build-latest", true, false, []string{"low", "medium", "high"}},
		{"grok-4.3-latest", true, true, []string{"none", "low", "medium", "high"}},
		{"grok-4.7", true, false, []string{"low", "medium", "high", "xhigh"}},
		{"grok-4.6", true, false, []string{"low", "medium", "high", "xhigh"}},
		{"grok-5", true, false, []string{"low", "medium", "high", "xhigh"}},
		{"grok-build-0.1", false, false, nil},
		{"grok-code-fast-1", false, false, nil},
		{"grok-imagine", false, false, nil},
	}
	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			caps := CapsFor(tc.model, domain.ProtocolOpenAI, domain.ReasoningGrok)
			if caps.Reasoning != tc.reasoning {
				t.Fatalf("Reasoning = %v, want %v", caps.Reasoning, tc.reasoning)
			}
			if caps.CanDisable != tc.canDisable {
				t.Fatalf("CanDisable = %v, want %v", caps.CanDisable, tc.canDisable)
			}
			if tc.levels == nil {
				if caps.Format != FormatNone {
					t.Fatalf("Format = %v, want FormatNone", caps.Format)
				}
				return
			}
			if caps.Format != FormatGrok {
				t.Fatalf("Format = %v, want FormatGrok", caps.Format)
			}
			if len(caps.Levels) != len(tc.levels) {
				t.Fatalf("Levels = %#v, want %#v", caps.Levels, tc.levels)
			}
			for i := range tc.levels {
				if caps.Levels[i] != tc.levels[i] {
					t.Fatalf("Levels[%d] = %q, want %q", i, caps.Levels[i], tc.levels[i])
				}
			}
		})
	}
}

func TestApplyWireGrokNoneOn43WritesNone(t *testing.T) {
	out, err := ApplyWire("oai-chat", []byte(`{"model":"grok-4.3","messages":[{"role":"user","content":"hi"}]}`), "grok-4.3", &Config{Mode: ModeNone}, domain.ProtocolOpenAI, domain.ReasoningGrok)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["reasoning_effort"] != "none" {
		t.Fatalf("reasoning_effort = %#v, want none", m["reasoning_effort"])
	}
}

func TestApplyWireGrokNoneOn46OmitsField(t *testing.T) {
	out, err := ApplyWire("oai-chat", []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"hi"}]}`), "grok-4.6", &Config{Mode: ModeNone}, domain.ProtocolOpenAI, domain.ReasoningGrok)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["reasoning_effort"]; ok {
		t.Fatal("reasoning_effort present, want omitted")
	}
}

func TestFinalizeBodyGrokNoIntentDoesNotInjectEffort(t *testing.T) {
	out, err := FinalizeBody([]byte(`{"model":"combo","messages":[]}`), "grok-4.7", "oai-chat", domain.ProtocolOpenAI, domain.ReasoningGrok)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["reasoning_effort"]; ok {
		t.Fatal("reasoning_effort present, want omitted")
	}
}

func TestApplyWireGrok45ConvertsXHighToHigh(t *testing.T) {
	out, err := ApplyWire("oai-chat", []byte(`{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}]}`), "grok-4.5", &Config{Mode: ModeLevel, Level: "xhigh"}, domain.ProtocolOpenAI, domain.ReasoningGrok)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %#v, want high", m["reasoning_effort"])
	}
}

func TestApplyWireGrokUnknownEffortOmitsField(t *testing.T) {
	out, err := ApplyWire("oai-chat", []byte(`{"model":"grok-4.7","messages":[{"role":"user","content":"hi"}]}`), "grok-4.7", &Config{Mode: ModeLevel, Level: "banana"}, domain.ProtocolOpenAI, domain.ReasoningGrok)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["reasoning_effort"]; ok {
		t.Fatal("reasoning_effort present, want omitted")
	}
}

func TestApplyWireGrokResponsesEffort(t *testing.T) {
	out, err := ApplyWire("oai-responses", []byte(`{"model":"grok-4.7","input":[]}`), "grok-4.7", &Config{Mode: ModeLevel, Level: "medium"}, domain.ProtocolOpenAIResponses, domain.ReasoningGrok)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	reasoning, ok := m["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning = %#v, want object", m["reasoning"])
	}
	if reasoning["effort"] != "medium" {
		t.Fatalf("reasoning.effort = %#v, want medium", reasoning["effort"])
	}
	if _, ok := reasoning["summary"]; ok {
		t.Fatal("reasoning.summary present, want preserved-only (not injected)")
	}
	if _, ok := m["include"]; ok {
		t.Fatal("include present, want absent")
	}
}

func TestApplyWireGrokResponsesPreservesSummary(t *testing.T) {
	body := []byte(`{"model":"grok-4.7","input":[],"reasoning":{"summary":"detailed"}}`)
	out, err := ApplyWire("oai-responses", body, "grok-4.7", &Config{Mode: ModeLevel, Level: "high"}, domain.ProtocolOpenAIResponses, domain.ReasoningGrok)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	reasoning, ok := m["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning = %#v, want object", m["reasoning"])
	}
	if reasoning["summary"] != "detailed" {
		t.Fatalf("reasoning.summary = %#v, want detailed", reasoning["summary"])
	}
	if reasoning["effort"] != "high" {
		t.Fatalf("reasoning.effort = %#v, want high", reasoning["effort"])
	}
}

func TestApplyWireGrokStripsNoneWhenUnsupported(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","reasoning_effort":"none","messages":[{"role":"user","content":"hi"}]}`)
	out, err := ApplyWire("oai-chat", body, "grok-4.5", &Config{Mode: ModeNone}, domain.ProtocolOpenAI, domain.ReasoningGrok)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["reasoning_effort"]; ok {
		t.Fatalf("reasoning_effort = %#v, want stripped", m["reasoning_effort"])
	}
}

func TestApplyWireGrokMediumReplacesAutoReasoningEffort(t *testing.T) {
	body := []byte(`{"model":"grok-4.6","reasoning_effort":"auto","messages":[{"role":"user","content":"hi"}]}`)
	out, err := ApplyWire("oai-chat", body, "grok-4.6", &Config{Mode: ModeLevel, Level: "medium"}, domain.ProtocolOpenAI, domain.ReasoningGrok)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["reasoning_effort"] != "medium" {
		t.Fatalf("reasoning_effort = %#v, want medium", m["reasoning_effort"])
	}
}

func TestApplyWireGrokNonEffortStripsIncomingEffort(t *testing.T) {
	body := []byte(`{"model":"grok-imagine","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	out, err := ApplyWire("oai-chat", body, "grok-imagine", &Config{Mode: ModeLevel, Level: "high"}, domain.ProtocolOpenAI, domain.ReasoningGrok)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["reasoning_effort"]; ok {
		t.Fatalf("reasoning_effort = %#v, want stripped", m["reasoning_effort"])
	}
}
