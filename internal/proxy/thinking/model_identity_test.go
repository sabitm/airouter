package thinking

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"airouter/internal/domain"
)

func TestClaudeCodeCapabilityIDKnownFirstPartyDates(t *testing.T) {
	cases := []struct {
		model string
		want  string
	}{
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5"},
		{"anthropic/claude-haiku-4.5-20251001[1m]", "claude-haiku-4-5"},
		{"claude-sonnet-4-20250514", "claude-sonnet-4-0"},
		{"claude-sonnet-4-5-20250929", "claude-sonnet-4-5"},
		{"claude-opus-4-20250514", "claude-opus-4-0"},
		{"claude-opus-4-1-20250805", "claude-opus-4-1"},
		{"claude-opus-4-5-20251101", "claude-opus-4-5"},
		{"claude-3-5-sonnet-20241022", "claude-3-5-sonnet"},
		{"claude-3-7-sonnet-20250219", "claude-3-7-sonnet"},
	}
	for _, tc := range cases {
		if got := ClaudeCodeCapabilityID(tc.model); got != tc.want {
			t.Errorf("ClaudeCodeCapabilityID(%q) = %q want %q", tc.model, got, tc.want)
		}
	}
}

func TestClaudeCodeCapabilityIDKeepsUnknownNamesDistinct(t *testing.T) {
	for _, model := range []string{
		"claude-sonnet-4-6-preview",
		"claude-opus-4-7-preview",
		"claude-haiku-4-5-preview",
		"claude-sonnet-4-6-20990101",
		"claude-haiku-4-5-20990101",
		"claude-sonnet-4-6[9m]",
	} {
		if got := ClaudeCodeCapabilityID(model); got != normalizeClaudeModel(model) {
			t.Errorf("ClaudeCodeCapabilityID(%q) = %q, want unchanged identity", model, got)
		}
	}
}

func TestClaudeCodeDatedCapsEquivalent(t *testing.T) {
	pairs := [][2]string{
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5"},
		{"claude-sonnet-4-20250514", "claude-sonnet-4-0"},
		{"claude-sonnet-4-5-20250929", "claude-sonnet-4-5"},
		{"claude-opus-4-20250514", "claude-opus-4-0"},
		{"claude-opus-4-1-20250805", "claude-opus-4-1"},
		{"claude-opus-4-5-20251101", "claude-opus-4-5"},
	}
	for _, pair := range pairs {
		dated := CapsFor(pair[0], domain.ProtocolClaudeCode, domain.ReasoningClaude)
		canonical := CapsFor(pair[1], domain.ProtocolClaudeCode, domain.ReasoningClaude)
		if !reflect.DeepEqual(dated, canonical) {
			t.Errorf("%s caps = %+v want %+v", pair[0], dated, canonical)
		}
	}
}

func TestClaudeCodeMythos5RuntimeCaps(t *testing.T) {
	generic := CapsFor("claude-mythos-5", domain.ProtocolAnthropic, domain.ReasoningClaude)
	if generic.Format != FormatClaudeBudget {
		t.Fatalf("generic mythos format = %v want budget", generic.Format)
	}
	for _, level := range []string{"xhigh", "max"} {
		if levelIn(level, generic.Levels) {
			t.Fatalf("generic mythos gained %s: %+v", level, generic)
		}
	}

	cc := CapsFor("claude-mythos-5", domain.ProtocolClaudeCode, domain.ReasoningClaude)
	if !cc.Reasoning || cc.Format != FormatClaudeAdaptive {
		t.Fatalf("claude-code mythos caps = %+v", cc)
	}
	if cc.CanDisable {
		t.Fatalf("claude-code mythos accepts disabled thinking: %+v", cc)
	}
	if cc.MaxOutput != 128000 {
		t.Fatalf("claude-code mythos max output = %d", cc.MaxOutput)
	}
	for _, level := range []string{"low", "medium", "high", "xhigh", "max"} {
		if !levelIn(level, cc.Levels) {
			t.Fatalf("claude-code mythos missing %s: %+v", level, cc.Levels)
		}
	}
	if levelIn("none", cc.Levels) {
		t.Fatalf("claude-code mythos lists none: %+v", cc.Levels)
	}

	for _, model := range []string{"claude-sonnet-4-6-preview", "claude-sonnet-4-6-20990101"} {
		unknown := CapsFor(model, domain.ProtocolClaudeCode, domain.ReasoningClaude)
		if unknown.Format != FormatClaudeBudget || levelIn("xhigh", unknown.Levels) || levelIn("max", unknown.Levels) {
			t.Fatalf("%s gained runtime caps: %+v", model, unknown)
		}
	}
}

func TestClaudeCodeMythos5DisableScope(t *testing.T) {
	cc := CapsFor("claude-mythos-5", domain.ProtocolClaudeCode, domain.ReasoningClaude)
	if !cc.OmitOnDisable || cc.CanDisable {
		t.Fatalf("claude-code mythos disable caps = %+v", cc)
	}
	for _, model := range []string{
		"CLAUDE-MYTHOS-5",
		"anthropic/claude-mythos-5[1m]",
		"anthropic/CLAUDE-MYTHOS-5[1M]",
	} {
		caps := CapsFor(model, domain.ProtocolClaudeCode, domain.ReasoningClaude)
		if !caps.OmitOnDisable || caps.Format != FormatClaudeAdaptive {
			t.Fatalf("%s omits=%v caps=%+v", model, caps.OmitOnDisable, caps)
		}
	}
	generic := CapsFor("claude-mythos-5", domain.ProtocolAnthropic, domain.ReasoningClaude)
	if generic.OmitOnDisable {
		t.Fatalf("generic mythos gained omit-on-disable: %+v", generic)
	}
	for _, model := range []string{
		"claude-mythos-5-1",
		"claude-mythos-5-preview",
		"claude-mythos-5-20990101",
	} {
		caps := CapsFor(model, domain.ProtocolClaudeCode, domain.ReasoningClaude)
		if caps.OmitOnDisable {
			t.Fatalf("%s unexpectedly omits on disable: %+v", model, caps)
		}
	}

	none := EffectiveForDialect(&Config{Mode: ModeNone, Enable: EnableDisabled}, cc, domain.ReasoningClaude)
	if none == nil || none.Mode != ModeNone || none.Level != "" {
		t.Fatalf("explicit disable became %+v", none)
	}
	positive := EffectiveForDialect(&Config{Mode: ModeLevel, Level: "high", Enable: EnableDisabled}, cc, domain.ReasoningClaude)
	if positive == nil || positive.Mode != ModeLevel || positive.Level != "high" {
		t.Fatalf("positive intent changed: %+v", positive)
	}
}

func TestClaudeCodeMythos5DisableWire(t *testing.T) {
	caps := CapsFor("claude-mythos-5", domain.ProtocolClaudeCode, domain.ReasoningClaude)
	raw := []byte(`{"model":"combo","max_tokens":32,"thinking":{"type":"adaptive","display":"summarized","budget_tokens":8192},"output_config":{"effort":"high","format":{"type":"json_schema","schema":{"type":"object","title":"Out"}},"task_budget":{"type":"tokens","total":1000,"remaining":0},"flag":true},"reasoning_effort":"high","enable_thinking":true,"thinking_budget":4096,"metadata":{"keep":true},"messages":[{"role":"user","content":"hi"}]}`)
	out, err := ApplyWire("anth-msg", raw, "claude-mythos-5", &Config{Mode: ModeNone, Enable: EnableDisabled}, domain.ProtocolClaudeCode, domain.ReasoningClaude)
	if err != nil {
		t.Fatal(err)
	}
	got := assertMythos5DisableControls(t, out)
	assertOutputConfigSiblings(t, got)
	if !strings.Contains(string(got["metadata"]), "keep") {
		t.Fatalf("metadata lost: %s", out)
	}

	stale := map[string]any{
		"thinking": map[string]any{"type": "adaptive", "display": "summarized", "budget_tokens": 8192},
		"output_config": map[string]any{
			"effort":      "high",
			"format":      map[string]any{"type": "json_object"},
			"task_budget": map[string]any{"type": "tokens", "total": 1000},
			"flag":        true,
		},
	}
	WriteAnthropic(stale, &Config{Mode: ModeNone}, caps)
	if _, ok := stale["thinking"]; ok {
		t.Fatalf("direct writer kept thinking: %#v", stale)
	}
	oc, _ := stale["output_config"].(map[string]any)
	if _, ok := oc["effort"]; ok || oc["format"] == nil || oc["task_budget"] == nil || oc["flag"] != true {
		t.Fatalf("direct writer output_config = %#v", oc)
	}
}

func TestClaudeCodeMythos5DisableFinalize(t *testing.T) {
	raw := []byte(`{"model":"combo","max_tokens":32,"output_config":{"effort":"high","format":{"type":"json_schema","schema":{"type":"object","title":"Out"}},"task_budget":{"type":"tokens","total":1000,"remaining":0}},"messages":[{"role":"user","content":"hi"}]}`)
	out, err := FinalizeBody(raw, "claude-mythos-5(none)", "anth-msg", domain.ProtocolClaudeCode, domain.ReasoningClaude)
	if err != nil {
		t.Fatal(err)
	}
	assertOutputConfigSiblings(t, assertMythos5DisableControls(t, out))

	noneBody := []byte(`{"model":"combo","max_tokens":32,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`)
	out, err = FinalizeBody(noneBody, "claude-mythos-5(high)", "anth-msg", domain.ProtocolClaudeCode, domain.ReasoningClaude)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	th, _ := got["thinking"].(map[string]any)
	oc, _ := got["output_config"].(map[string]any)
	if th["type"] != "adaptive" || oc["effort"] != "high" {
		t.Fatalf("suffix high lost to body disable: %s", out)
	}
}

func TestClaudeDisableClampUnaffectedOutsideMythos5(t *testing.T) {
	raw := []byte(`{"model":"combo","thinking":{"type":"enabled","budget_tokens":2048},"messages":[{"role":"user","content":"hi"}]}`)
	generic, err := ApplyWire("anth-msg", raw, "claude-mythos-5", &Config{Mode: ModeNone}, domain.ProtocolAnthropic, domain.ReasoningClaude)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(generic, &got); err != nil {
		t.Fatal(err)
	}
	th, _ := got["thinking"].(map[string]any)
	if len(th) != 1 || th["type"] != "disabled" {
		t.Fatalf("generic mythos disable = %s", generic)
	}

	alwaysOn, err := ApplyWire("anth-msg", raw, "claude-haiku-5-5", &Config{Mode: ModeNone}, domain.ProtocolAnthropic, domain.ReasoningClaude)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(alwaysOn, &got); err != nil {
		t.Fatal(err)
	}
	th, _ = got["thinking"].(map[string]any)
	oc, _ := got["output_config"].(map[string]any)
	if th["type"] != "adaptive" || oc["effort"] != "low" {
		t.Fatalf("always-on clamp changed: %s", alwaysOn)
	}
}

func assertMythos5DisableControls(t *testing.T, out []byte) map[string]json.RawMessage {
	t.Helper()
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode wire: %v; body=%s", err, out)
	}
	for _, key := range []string{"thinking", "reasoning_effort", "reasoning", "enable_thinking", "thinking_budget"} {
		if _, ok := got[key]; ok {
			t.Fatalf("%s leaked: %s", key, out)
		}
	}
	if raw, ok := got["output_config"]; ok {
		var oc map[string]json.RawMessage
		if err := json.Unmarshal(raw, &oc); err != nil {
			t.Fatalf("decode output_config: %v; body=%s", err, out)
		}
		if _, ok := oc["effort"]; ok {
			t.Fatalf("output_config.effort leaked: %s", out)
		}
	}
	return got
}

func assertOutputConfigSiblings(t *testing.T, got map[string]json.RawMessage) {
	t.Helper()
	var oc map[string]json.RawMessage
	if err := json.Unmarshal(got["output_config"], &oc); err != nil {
		t.Fatalf("decode output_config siblings: %v", err)
	}
	if !strings.Contains(string(oc["format"]), "json_schema") || !strings.Contains(string(oc["task_budget"]), "1000") {
		t.Fatalf("output_config siblings lost: %s", got["output_config"])
	}
}

func TestGenericClaudeHistoricalCapsUnchanged(t *testing.T) {
	cases := []string{
		"claude-sonnet-4-20250514",
		"claude-opus-4-20250514",
	}
	for _, model := range cases {
		caps := CapsFor(model, domain.ProtocolAnthropic, domain.ReasoningClaude)
		if caps.Format != FormatClaudeBudget {
			t.Errorf("%s format = %v want budget", model, caps.Format)
		}
		for _, level := range []string{"xhigh", "max"} {
			if levelIn(level, caps.Levels) {
				t.Errorf("%s gained %s: %+v", model, level, caps)
			}
		}
	}
	haiku := CapsFor("claude-haiku-4-5-20251001", domain.ProtocolAnthropic, domain.ReasoningClaude)
	if haiku.Format != FormatClaudeBudget || haiku.MaxOutput != 64000 {
		t.Fatalf("generic dated haiku regressed: %+v", haiku)
	}
}
