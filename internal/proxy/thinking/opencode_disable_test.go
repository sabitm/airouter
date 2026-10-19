package thinking

import (
	"encoding/json"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/opencode"
)

// opencodeClaudeDisableRows is the approved exact scope: Anthropic Messages
// rows whose explicit disable must reach the writer.
var opencodeClaudeDisableRows = []string{
	"claude-sonnet-4",
	"claude-opus-4-1",
	"claude-haiku-4-5",
	"claude-sonnet-4-5",
	"claude-opus-4-5",
	"claude-sonnet-4-6",
	"claude-opus-4-6",
	"claude-opus-4-7",
	"claude-opus-4-8",
}

func TestOpencodeClaudeDisableCaps(t *testing.T) {
	for _, id := range opencodeClaudeDisableRows {
		spec, ok := opencode.Lookup("zen", id)
		if !ok || spec.ID != id || spec.SDK != "anthropic" {
			t.Fatalf("%s spec=%+v ok=%v", id, spec, ok)
		}
		if !OpencodeAnthropicDisableSupported(spec) {
			t.Fatalf("%s not in disable scope", id)
		}
		caps := OpencodeCaps(spec, true)
		if !caps.Reasoning || !caps.CanDisable {
			t.Fatalf("%s caps=%+v", id, caps)
		}
		none := EffectiveForDialect(&Config{Mode: ModeNone, Enable: EnableDisabled}, caps, domain.ReasoningOpencode)
		if none == nil || none.Mode != ModeNone || none.Level != "" {
			t.Fatalf("%s disabled effective=%+v", id, none)
		}
		// EnableDisabled paired with a positive intensity stays positive here;
		// the scoped writer, not the clamp, chooses the disabled shape.
		positive := EffectiveForDialect(&Config{Mode: ModeLevel, Level: "high", Enable: EnableDisabled}, caps, domain.ReasoningOpencode)
		if positive == nil || positive.Mode != ModeLevel {
			t.Fatalf("%s positive effective=%+v", id, positive)
		}
	}
}

func TestOpencodeClaudeDisableScope(t *testing.T) {
	supported := map[string]bool{}
	for _, id := range opencodeClaudeDisableRows {
		supported[id] = true
	}
	if len(supported) != 9 {
		t.Fatalf("scope size = %d, want 9", len(supported))
	}
	rows := []string{
		"claude-sonnet-4-5-extra", "claude-sonnet-4.5", "claude-opus-4",
		"claude-opus-5", "claude-opus-5-5", "claude-sonnet-5",
		"claude-fable-5", "claude-fable-5-1", "claude-haiku-4-5-extra",
		"qwen3.8-flash", "qwen3.6-plus", "minimax-m3", "minimax-m3-free",
		"glm-5.3", "gpt-5.4", "unknown-model",
	}
	for _, id := range rows {
		if supported[id] {
			t.Fatalf("%s unexpectedly supported", id)
		}
		spec, ok := opencode.Lookup("zen", id)
		if !ok {
			if OpencodeAnthropicDisableSupported(spec) {
				t.Fatalf("absent %s supported with zero spec", id)
			}
			continue
		}
		if OpencodeAnthropicDisableSupported(spec) {
			t.Fatalf("%s spec=%+v unexpectedly supported", id, spec)
		}
	}
	// Vendor prefix normalizes through Lookup to the pinned row.
	prefixed, ok := opencode.Lookup("zen", "anthropic/claude-sonnet-4-5")
	if !ok || !OpencodeAnthropicDisableSupported(prefixed) {
		t.Fatalf("prefixed row=%+v ok=%v", prefixed, ok)
	}
	// Suffix normalizes through Lookup.
	suffixed, ok := opencode.Lookup("zen", "claude-sonnet-4-5(none)")
	if !ok || !OpencodeAnthropicDisableSupported(suffixed) {
		t.Fatalf("suffixed row=%+v ok=%v", suffixed, ok)
	}
	// Missing Go rows do not invent a Go-tier disable.
	if spec, ok := opencode.Lookup("go", "claude-sonnet-4-5"); ok {
		t.Fatalf("go tier unexpectedly has row: %+v", spec)
	}
}

func TestOpencodeClaudeDisableWire(t *testing.T) {
	controls := []struct {
		name string
		raw  string
	}{
		{name: "reasoning none", raw: `{"reasoning_effort":"none"}`},
		{name: "reasoning off", raw: `{"reasoning_effort":"off"}`},
		{name: "responses none", raw: `{"reasoning":{"effort":"none"}}`},
		{name: "responses off", raw: `{"reasoning":{"effort":"off"}}`},
		{name: "thinking disabled", raw: `{"thinking":{"type":"disabled"}}`},
		{name: "thinking disabled budget", raw: `{"thinking":{"type":"disabled","budget_tokens":8192}}`},
		{name: "enable false", raw: `{"enable_thinking":false}`},
		{name: "disable beats output effort", raw: `{"output_config":{"effort":"high"},"thinking":{"type":"disabled"}}`},
		{name: "disable beats positive effort", raw: `{"reasoning_effort":"high","enable_thinking":false}`},
	}
	for _, id := range opencodeClaudeDisableRows {
		t.Run(id, func(t *testing.T) {
			for _, tc := range controls {
				t.Run(tc.name, func(t *testing.T) {
					body := opencodeClaudeDisableBody(t, tc.raw)
					out, err := FinalizeBody(body, id, "anth-msg", domain.ProtocolOpencode, domain.ReasoningOpencode)
					if err != nil {
						t.Fatal(err)
					}
					var got map[string]any
					if err := json.Unmarshal(out, &got); err != nil {
						t.Fatal(err)
					}
					if got["model"] != id {
						t.Fatalf("model = %v, want %s", got["model"], id)
					}
					assertOpencodeClaudeDisabled(t, got)
				})
			}
			for _, suffix := range []string{"(none)", "(off)"} {
				t.Run("suffix "+suffix, func(t *testing.T) {
					body := opencodeClaudeDisableBody(t, `{"reasoning_effort":"high","enable_thinking":true}`)
					out, err := FinalizeBody(body, id+suffix, "anth-msg", domain.ProtocolOpencode, domain.ReasoningOpencode)
					if err != nil {
						t.Fatal(err)
					}
					var got map[string]any
					if err := json.Unmarshal(out, &got); err != nil {
						t.Fatal(err)
					}
					if got["model"] != id {
						t.Fatalf("model = %v, want %s", got["model"], id)
					}
					assertOpencodeClaudeDisabled(t, got)
				})
			}
		})
	}
}

func TestOpencodeClaudeDisablePositiveAndNoIntent(t *testing.T) {
	// Positive intent is unchanged: budget rows emit an enabled budget, effort
	// rows emit their effort shape. No row keeps a disabled thinking block.
	positive := []struct {
		model string
		body  string
	}{
		{model: "claude-sonnet-4-5", body: `{"reasoning_effort":"high"}`},
		{model: "claude-opus-4-1", body: `{"reasoning_effort":"max"}`},
		{model: "claude-haiku-4-5", body: `{"reasoning_effort":"high"}`},
		{model: "claude-sonnet-4", body: `{"reasoning_effort":"high"}`},
		{model: "claude-opus-4-5", body: `{"reasoning_effort":"high"}`},
		{model: "claude-sonnet-4-6", body: `{"reasoning_effort":"high"}`},
		{model: "claude-opus-4-6", body: `{"reasoning_effort":"high"}`},
		{model: "claude-opus-4-7", body: `{"reasoning_effort":"high"}`},
		{model: "claude-opus-4-8", body: `{"reasoning_effort":"high"}`},
	}
	for _, tc := range positive {
		t.Run("positive/"+tc.model, func(t *testing.T) {
			out, err := FinalizeBody(opencodeClaudeDisableBody(t, tc.body), tc.model, "anth-msg", domain.ProtocolOpencode, domain.ReasoningOpencode)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if th, ok := got["thinking"].(map[string]any); ok {
				if th["type"] == "disabled" {
					t.Fatalf("%s positive wrote disabled: %s", tc.model, out)
				}
			}
		})
	}

	t.Run("no intent", func(t *testing.T) {
		body := []byte(`{"model":"combo","messages":[{"role":"user","content":"hi"}]}`)
		for _, id := range opencodeClaudeDisableRows {
			out, err := FinalizeBody(body, id, "anth-msg", domain.ProtocolOpencode, domain.ReasoningOpencode)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if _, ok := got["thinking"]; ok {
				t.Fatalf("%s no-intent wrote thinking: %s", id, out)
			}
		}
	})

	t.Run("assistant final gate", func(t *testing.T) {
		body := opencodeClaudeDisableBody(t, `{"reasoning_effort":"none"}`)
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatal(err)
		}
		m["messages"] = []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "done"},
		}
		body, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		out, err := FinalizeBody(body, "claude-sonnet-4-5", "anth-msg", domain.ProtocolOpencode, domain.ReasoningOpencode)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if _, ok := got["thinking"]; ok {
			t.Fatalf("assistant-final wrote thinking: %s", out)
		}
	})
}

func TestOpencodeClaudeDisableUnchangedNeighbors(t *testing.T) {
	// Claude 5.x and Fable keep their previous enable/upgrade behavior.
	for _, model := range []string{"claude-opus-5", "claude-opus-5-5", "claude-sonnet-5", "claude-fable-5"} {
		t.Run(model, func(t *testing.T) {
			out, err := FinalizeBody(opencodeClaudeDisableBody(t, `{"reasoning_effort":"none"}`), model, "anth-msg", domain.ProtocolOpencode, domain.ReasoningOpencode)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if th, ok := got["thinking"].(map[string]any); ok && th["type"] == "disabled" {
				t.Fatalf("%s unexpectedly disabled: %s", model, out)
			}
		})
	}
	// qwen3.8-flash keeps its own exact disable path.
	out, err := FinalizeBody(opencodeClaudeDisableBody(t, `{"reasoning_effort":"none"}`), "qwen3.8-flash", "anth-msg", domain.ProtocolOpencode, domain.ReasoningOpencode)
	if err != nil {
		t.Fatal(err)
	}
	var qwen map[string]any
	if err := json.Unmarshal(out, &qwen); err != nil {
		t.Fatal(err)
	}
	assertOpencodeClaudeDisabled(t, qwen)
}

func opencodeClaudeDisableBody(t *testing.T, controls string) []byte {
	t.Helper()
	extra := map[string]any{}
	if err := json.Unmarshal([]byte(controls), &extra); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"model":    "combo",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"output_config": map[string]any{
			"format": map[string]any{"type": "json_object"},
		},
	}
	for key, value := range extra {
		if key == "output_config" {
			oc := body["output_config"].(map[string]any)
			for nestedKey, nested := range value.(map[string]any) {
				oc[nestedKey] = nested
			}
			continue
		}
		body[key] = value
	}
	out, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertOpencodeClaudeDisabled(t *testing.T, got map[string]any) {
	t.Helper()
	th, _ := got["thinking"].(map[string]any)
	if len(th) != 1 || th["type"] != "disabled" {
		t.Fatalf("thinking = %#v, want exactly type disabled", th)
	}
	for _, field := range []string{"reasoning_effort", "enable_thinking", "thinking_budget", "reasoning"} {
		if _, ok := got[field]; ok {
			t.Fatalf("%s leaked: %#v", field, got[field])
		}
	}
	oc, _ := got["output_config"].(map[string]any)
	if _, has := oc["effort"]; has {
		t.Fatalf("output_config.effort leaked: %#v", oc)
	}
	format, _ := oc["format"].(map[string]any)
	if format["type"] != "json_object" {
		t.Fatalf("output_config sibling lost: %#v", oc)
	}
}

// The disabled writer must delete a caller budget and a generated effort while
// preserving raw sibling numbers for the finalizer that serializes the body.
func TestOpencodeClaudeDisableRawSiblings(t *testing.T) {
	const large = "9007199254740993"
	raw := []byte(`{"model":"combo","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","budget_tokens":8192},"output_config":{"effort":"high","format":{"type":"json_object"},"raw":{"n":` + large + `},"flag":true},"metadata":{"keep":` + large + `}}`)
	out, err := FinalizeBody(raw, "claude-sonnet-4-5", "anth-msg", domain.ProtocolOpencode, domain.ReasoningOpencode)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), large) {
		t.Fatalf("large raw number lost: %s", out)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	assertOpencodeClaudeDisabled(t, got)
}

// The shared decision requires both an approved exact row and explicit disable
// intent. A lookalike row, nil config, or positive intent must stay false.
func TestOpencodeAnthropicDisableDecision(t *testing.T) {
	approved, ok := opencode.Lookup("zen", "claude-sonnet-4-5")
	if !ok {
		t.Fatal("approved row missing")
	}
	lookalike, _ := opencode.Lookup("zen", "claude-sonnet-4-5-extra")
	disable := &Config{Mode: ModeNone, Enable: EnableDisabled}
	if !OpencodeAnthropicDisableDecision(approved, disable) {
		t.Fatal("approved disable rejected")
	}
	if OpencodeAnthropicDisableDecision(lookalike, disable) {
		t.Fatal("lookalike accepted")
	}
	cases := []struct {
		name string
		cfg  *Config
		want bool
	}{
		{name: "nil", cfg: nil},
		{name: "positive level", cfg: &Config{Mode: ModeLevel, Level: "high"}},
		{name: "positive budget", cfg: &Config{Mode: ModeBudget, Budget: 8192}},
		{name: "disable beats budget", cfg: &Config{Mode: ModeBudget, Budget: 8192, Enable: EnableDisabled}, want: true},
		{name: "disable beats level", cfg: &Config{Mode: ModeLevel, Level: "high", Enable: EnableDisabled}, want: true},
		{name: "none mode", cfg: &Config{Mode: ModeNone}, want: true},
		{name: "off level", cfg: &Config{Mode: ModeLevel, Level: "off"}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := OpencodeAnthropicDisableDecision(approved, tc.cfg); got != tc.want {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
}

// Capture precedence keeps an explicit positive thinking.type over the same-body
// enable_thinking=false, so the numeric budget stays enabled at the writer.
// A plain enable_thinking=false plus a numeric suffix disables and writes no
// budget, so the same writer branch serves both guards.
func TestOpencodeClaudeDisableNumericBudgetPrecedence(t *testing.T) {
	// Explicit thinking.type enabled overrides enable_thinking=false.
	body := opencodeClaudeDisableBody(t, `{"enable_thinking":false,"thinking":{"type":"enabled","budget_tokens":8192}}`)
	cfg := Capture(body)
	if cfg == nil || cfg.Enable != EnableEnabled || cfg.Mode != ModeBudget || cfg.Budget != 8192 {
		t.Fatalf("capture = %+v, want enabled budget", cfg)
	}
	out, err := FinalizeBody(body, "claude-sonnet-4-5", "anth-msg", domain.ProtocolOpencode, domain.ReasoningOpencode)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	th, _ := got["thinking"].(map[string]any)
	if th["type"] != "enabled" || th["budget_tokens"] != float64(8192) {
		t.Fatalf("thinking = %#v, want enabled budget 8192", th)
	}

	// enable_thinking=false plus a numeric suffix disables.
	out, err = FinalizeBody(opencodeClaudeDisableBody(t, `{"enable_thinking":false}`), "claude-sonnet-4-5(8192)", "anth-msg", domain.ProtocolOpencode, domain.ReasoningOpencode)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	assertOpencodeClaudeDisabled(t, got)
}
