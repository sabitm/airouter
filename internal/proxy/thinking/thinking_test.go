package thinking

import (
	"encoding/json"
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

func TestCapsForClaudeAdaptive(t *testing.T) {
	c := CapsFor("claude-opus-4-7", domain.ProtocolAnthropic)
	if c.Format != FormatClaudeAdaptive {
		t.Fatalf("format=%v", c.Format)
	}
	c = CapsFor("claude-haiku-4.5", domain.ProtocolAnthropic)
	if c.Format != FormatClaudeBudget {
		t.Fatalf("haiku format=%v", c.Format)
	}
	c = CapsFor("any", domain.ProtocolClaudeCode)
	if c.Format != FormatClaudeAdaptive {
		t.Fatalf("claude-code format=%v", c.Format)
	}
}

func TestApplyWireOpenAI(t *testing.T) {
	body := []byte(`{"model":"x","messages":[],"temperature":0.5,"thinking":{"type":"enabled"}}`)
	out, err := ApplyWire("oai-chat", body, "gpt-5", &Config{Mode: ModeLevel, Level: "high"})
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
	if m["temperature"] != 0.5 {
		t.Fatal("foreign field lost")
	}
}
