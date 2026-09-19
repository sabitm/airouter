package thinking

import (
	"encoding/json"
	"testing"

	"airouter/internal/domain"
)

func TestCapsForClineKeepsNamespace(t *testing.T) {
	c := CapsFor("cline-pass/glm-5.2", domain.ProtocolOpenAI, domain.ReasoningCline)
	if !c.Reasoning || c.Format != FormatCline || !c.CanDisable {
		t.Fatalf("cline caps = %+v", c)
	}
}

func TestClineFinalizeMatrix(t *testing.T) {
	const dirty = `{
		"model":"combo",
		"reasoning_effort":"high",
		"reasoning":{"effort":"low","summary":"auto","enabled":true,"exclude":false},
		"thinking":{"type":"enabled","budget_tokens":1024,"vendor":"keep"},
		"enable_thinking":true,
		"thinking_budget":2048,
		"output_config":{"effort":"low","format":{"type":"json_object"}},
		"foo":1
	}`
	noneBody := `{"model":"combo","reasoning_effort":"none","messages":[{"role":"user","content":"hi"}]}`
	offBody := `{"model":"combo","reasoning_effort":"off","messages":[{"role":"user","content":"hi"}]}`

	type checkFn func(t *testing.T, m map[string]any)

	cases := []struct {
		name  string
		body  string
		model string
		check checkFn
	}{
		{
			name:  "auto strips all recognized controls",
			body:  `{"model":"combo","reasoning_effort":"auto","reasoning":{"effort":"auto","summary":"auto","enabled":true,"exclude":false},"thinking":{"type":"enabled","budget_tokens":1024,"vendor":"keep"},"enable_thinking":true,"thinking_budget":2048,"output_config":{"effort":"auto","format":{"type":"json_object"}},"foo":1}`,
			model: "cline-pass/glm-5.2",
			check: func(t *testing.T, m map[string]any) {
				assertClineControlsGone(t, m)
				if m["foo"] != float64(1) {
					t.Fatalf("foo lost: %v", m["foo"])
				}
				r, _ := m["reasoning"].(map[string]any)
				if r == nil || r["summary"] != "auto" {
					t.Fatalf("summary lost: %v", r)
				}
				th, _ := m["thinking"].(map[string]any)
				if th == nil || th["vendor"] != "keep" {
					t.Fatalf("thinking.vendor lost: %v", th)
				}
				oc, _ := m["output_config"].(map[string]any)
				if oc == nil || oc["format"] == nil {
					t.Fatalf("output_config.format lost: %v", oc)
				}
			},
		},
		{
			name:  "suffix auto overrides body effort",
			body:  `{"model":"combo","reasoning_effort":"high"}`,
			model: "cline-pass/glm-5.2(auto)",
			check: func(t *testing.T, m map[string]any) {
				if m["model"] != "cline-pass/glm-5.2" {
					t.Fatalf("model=%v", m["model"])
				}
				assertClineControlsGone(t, m)
			},
		},
		{
			name:  "deepseek dual disable",
			body:  noneBody,
			model: "cline-pass/deepseek-v4-flash",
			check: func(t *testing.T, m map[string]any) {
				assertReasoningEnabledFalse(t, m)
				th, _ := m["thinking"].(map[string]any)
				if th == nil || th["type"] != "disabled" {
					t.Fatalf("thinking=%v", th)
				}
			},
		},
		{
			name:  "clinepass glm 5.2 enabled false",
			body:  noneBody,
			model: "cline-pass/glm-5.2",
			check: func(t *testing.T, m map[string]any) {
				assertReasoningEnabledFalse(t, m)
				if _, ok := m["thinking"]; ok {
					t.Fatalf("thinking leaked: %v", m["thinking"])
				}
			},
		},
		{
			name:  "clinepass glm 5.3 omit",
			body:  noneBody,
			model: "cline-pass/glm-5.3",
			check: assertClineControlsGone,
		},
		{
			name:  "direct zai glm exclude",
			body:  dirty,
			model: "z-ai/glm-5.2(none)",
			check: func(t *testing.T, m map[string]any) {
				assertReasoningExcludeTrue(t, m)
				r, _ := m["reasoning"].(map[string]any)
				if r["summary"] != "auto" {
					t.Fatalf("summary lost: %v", r)
				}
				th, _ := m["thinking"].(map[string]any)
				if th == nil || th["vendor"] != "keep" {
					t.Fatalf("thinking.vendor lost: %v", th)
				}
				if _, ok := th["type"]; ok {
					t.Fatalf("thinking.type leaked: %v", th)
				}
			},
		},
		{
			name:  "direct tilde zai glm exclude",
			body:  noneBody,
			model: "~z-ai/glm-5.2",
			check: assertReasoningExcludeTrue,
		},
		{
			name:  "direct zai glm-4.7 exclude",
			body:  noneBody,
			model: "z-ai/glm-4.7",
			check: assertReasoningExcludeTrue,
		},
		{
			name:  "direct zai glm-5.3-flash omit",
			body:  dirty,
			model: "z-ai/glm-5.3-flash(none)",
			check: assertClineControlsGone,
		},
		{
			name:  "direct tilde zai glm-5.3-flash omit",
			body:  noneBody,
			model: "~z-ai/glm-5.3-flash",
			check: assertClineControlsGone,
		},
		{
			name:  "unknown direct zai omit",
			body:  dirty,
			model: "z-ai/unknown-glm(none)",
			check: assertClineControlsGone,
		},
		{
			name:  "cline-free deepseek dual disable",
			body:  dirty,
			model: "cline-free/deepseek-v4.1-flash(none)",
			check: func(t *testing.T, m map[string]any) {
				assertReasoningEnabledFalse(t, m)
				r, _ := m["reasoning"].(map[string]any)
				if _, ok := r["exclude"]; ok {
					t.Fatalf("stale exclude: %v", r)
				}
				th, _ := m["thinking"].(map[string]any)
				if th == nil || th["type"] != "disabled" {
					t.Fatalf("thinking=%v", th)
				}
				if th["vendor"] != "keep" {
					t.Fatalf("thinking.vendor lost: %v", th)
				}
			},
		},
		{
			name:  "cline-free muse spark omit",
			body:  dirty,
			model: "cline-free/muse-spark-1.3-contributor(none)",
			check: assertClineControlsGone,
		},
		{
			name:  "unknown cline-free omit",
			body:  dirty,
			model: "cline-free/unknown-model(none)",
			check: assertClineControlsGone,
		},
		{
			name:  "cline-free solar-pro4 enabled false",
			body:  dirty,
			model: "cline-free/solar-pro4(none)",
			check: func(t *testing.T, m map[string]any) {
				assertReasoningEnabledFalse(t, m)
				r, _ := m["reasoning"].(map[string]any)
				if _, ok := r["exclude"]; ok {
					t.Fatalf("stale exclude: %v", r)
				}
				th, _ := m["thinking"].(map[string]any)
				if th == nil || th["vendor"] != "keep" {
					t.Fatalf("thinking.vendor lost: %v", th)
				}
				if _, ok := th["type"]; ok {
					t.Fatalf("thinking.type leaked: %v", th)
				}
			},
		},
		{
			name:  "clinepass kimi enabled false",
			body:  noneBody,
			model: "cline-pass/kimi-k2.6",
			check: assertReasoningEnabledFalse,
		},
		{
			name:  "clinepass minimax enabled false",
			body:  noneBody,
			model: "cline-pass/minimax-m3",
			check: assertReasoningEnabledFalse,
		},
		{
			name:  "clinepass mimo enabled false",
			body:  noneBody,
			model: "cline-pass/mimo-v2.5",
			check: assertReasoningEnabledFalse,
		},
		{
			name:  "clinepass qwen omit",
			body:  noneBody,
			model: "cline-pass/qwen3.7-max",
			check: assertClineControlsGone,
		},
		{
			name:  "clinepass kimi k2.7 code omit",
			body:  noneBody,
			model: "cline-pass/kimi-k2.7-code",
			check: assertClineControlsGone,
		},
		{
			name:  "unknown model omit",
			body:  noneBody,
			model: "vendor/unknown-model",
			check: assertClineControlsGone,
		},
		{
			name:  "effort only gpt omit",
			body:  noneBody,
			model: "openai/gpt-5.4-pro",
			check: assertClineControlsGone,
		},
		{
			name:  "effort only gemini omit",
			body:  noneBody,
			model: "google/gemini-3.1-pro-preview",
			check: assertClineControlsGone,
		},
		{
			name:  "effort only claude fable omit",
			body:  noneBody,
			model: "anthropic/claude-fable-5.1",
			check: assertClineControlsGone,
		},
		{
			name:  "deepseek forced reasoner omit",
			body:  noneBody,
			model: "deepseek/deepseek-r1",
			check: assertClineControlsGone,
		},
		{
			name:  "gpt none capable enabled false",
			body:  noneBody,
			model: "openai/gpt-5.4",
			check: assertReasoningEnabledFalse,
		},
		{
			name:  "other catalog toggle family enabled false",
			body:  noneBody,
			model: "inclusionai/ling-3.0-flash",
			check: assertReasoningEnabledFalse,
		},
		{
			name:  "max remains reasoning_effort max",
			body:  `{"model":"combo","reasoning_effort":"max"}`,
			model: "cline-pass/glm-5.2",
			check: func(t *testing.T, m map[string]any) {
				if m["reasoning_effort"] != "max" {
					t.Fatalf("effort=%v", m["reasoning_effort"])
				}
			},
		},
		{
			name:  "positive effort removes stale disable",
			body:  `{"model":"combo","reasoning":{"enabled":false,"exclude":true,"summary":"auto"},"thinking":{"type":"disabled","vendor":"keep"}}`,
			model: "cline-pass/glm-5.2(high)",
			check: func(t *testing.T, m map[string]any) {
				if m["reasoning_effort"] != "high" {
					t.Fatalf("effort=%v", m["reasoning_effort"])
				}
				r, _ := m["reasoning"].(map[string]any)
				if r == nil || r["summary"] != "auto" {
					t.Fatalf("summary lost: %v", r)
				}
				if _, ok := r["enabled"]; ok {
					t.Fatalf("stale enabled: %v", r)
				}
				if _, ok := r["exclude"]; ok {
					t.Fatalf("stale exclude: %v", r)
				}
				th, _ := m["thinking"].(map[string]any)
				if th == nil || th["vendor"] != "keep" {
					t.Fatalf("thinking.vendor lost: %v", th)
				}
				if _, ok := th["type"]; ok {
					t.Fatalf("stale thinking.type: %v", th)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			model := tc.model
			out, err := FinalizeBody(body, model, "oai-chat", domain.ProtocolOpenAI, domain.ReasoningCline)
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

	t.Run("none and off equal", func(t *testing.T) {
		a, err := FinalizeBody([]byte(noneBody), "cline-pass/deepseek-v4-flash", "oai-chat", domain.ProtocolOpenAI, domain.ReasoningCline)
		if err != nil {
			t.Fatal(err)
		}
		b, err := FinalizeBody([]byte(offBody), "cline-pass/deepseek-v4-flash", "oai-chat", domain.ProtocolOpenAI, domain.ReasoningCline)
		if err != nil {
			t.Fatal(err)
		}
		if string(a) != string(b) {
			t.Fatalf("none=%s off=%s", a, b)
		}
	})

	t.Run("unrelated siblings survive disable", func(t *testing.T) {
		out, err := FinalizeBody([]byte(dirty), "cline-pass/glm-5.2(none)", "oai-chat", domain.ProtocolOpenAI, domain.ReasoningCline)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatal(err)
		}
		r, _ := m["reasoning"].(map[string]any)
		if r == nil || r["summary"] != "auto" || r["enabled"] != false {
			t.Fatalf("reasoning=%v", r)
		}
		th, _ := m["thinking"].(map[string]any)
		if th == nil || th["vendor"] != "keep" {
			t.Fatalf("thinking=%v", th)
		}
		oc, _ := m["output_config"].(map[string]any)
		if oc == nil || oc["format"] == nil {
			t.Fatalf("output_config=%v", oc)
		}
		if _, ok := oc["effort"]; ok {
			t.Fatalf("effort sibling should be stripped: %v", oc)
		}
		if m["foo"] != float64(1) {
			t.Fatal("foo lost")
		}
	})
}

func TestClineNoIntentModelOnly(t *testing.T) {
	body := []byte(`{"model":"combo","messages":[],"foo":1}`)
	out, err := FinalizeBody(body, "cline-pass/glm-5.2", "oai-chat", domain.ProtocolOpenAI, domain.ReasoningCline)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "cline-pass/glm-5.2" || m["foo"] != float64(1) {
		t.Fatalf("model-only broken: %s", out)
	}
	if _, ok := m["reasoning_effort"]; ok {
		t.Fatal("injected effort without intent")
	}
	if _, ok := m["reasoning"]; ok {
		t.Fatal("injected reasoning without intent")
	}
}

func TestClineDoesNotChangeGenericOpenAI(t *testing.T) {
	autoOut, err := FinalizeBody([]byte(`{"model":"combo","reasoning_effort":"auto"}`), "gpt-5", "oai-chat", domain.ProtocolOpenAI, domain.ReasoningOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	var am map[string]any
	if err := json.Unmarshal(autoOut, &am); err != nil {
		t.Fatal(err)
	}
	if am["reasoning_effort"] != "auto" {
		t.Fatalf("openai auto = %s", autoOut)
	}

	noneOut, err := FinalizeBody([]byte(`{"model":"combo","reasoning_effort":"none"}`), "gpt-5", "oai-chat", domain.ProtocolOpenAI, domain.ReasoningOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	var nm map[string]any
	if err := json.Unmarshal(noneOut, &nm); err != nil {
		t.Fatal(err)
	}
	if nm["reasoning_effort"] != "none" {
		t.Fatalf("openai none = %s", noneOut)
	}
}

func assertClineControlsGone(t *testing.T, m map[string]any) {
	t.Helper()
	for _, field := range []string{"reasoning_effort", "enable_thinking", "thinking_budget"} {
		if _, ok := m[field]; ok {
			t.Fatalf("%s leaked: %v", field, m[field])
		}
	}
	if r, ok := m["reasoning"].(map[string]any); ok {
		for _, field := range []string{"effort", "enabled", "exclude"} {
			if _, ok := r[field]; ok {
				t.Fatalf("reasoning.%s leaked: %v", field, r)
			}
		}
	}
	if th, ok := m["thinking"].(map[string]any); ok {
		for _, field := range []string{"type", "budget_tokens"} {
			if _, ok := th[field]; ok {
				t.Fatalf("thinking.%s leaked: %v", field, th)
			}
		}
	}
	if oc, ok := m["output_config"].(map[string]any); ok {
		if _, ok := oc["effort"]; ok {
			t.Fatalf("output_config.effort leaked: %v", oc)
		}
	}
}

func assertReasoningEnabledFalse(t *testing.T, m map[string]any) {
	t.Helper()
	if _, ok := m["reasoning_effort"]; ok {
		t.Fatalf("reasoning_effort leaked: %v", m["reasoning_effort"])
	}
	r, _ := m["reasoning"].(map[string]any)
	if r == nil || r["enabled"] != false {
		t.Fatalf("reasoning=%v", r)
	}
}

func assertReasoningExcludeTrue(t *testing.T, m map[string]any) {
	t.Helper()
	r, _ := m["reasoning"].(map[string]any)
	if r == nil || r["exclude"] != true {
		t.Fatalf("reasoning=%v", r)
	}
	if _, ok := r["enabled"]; ok {
		t.Fatalf("enabled leaked: %v", r)
	}
}
