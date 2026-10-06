package thinking

import (
	"encoding/json"
	"testing"

	"airouter/internal/domain"
)

func TestNonKiroCapturePrecedenceUnchanged(t *testing.T) {
	cases := []struct{ body, effort string }{
		{`{"output_config":{"effort":"low"},"reasoning_effort":"high"}`, "low"},
		{`{"reasoning_effort":"low","reasoning":{"effort":"high"}}`, "low"},
		{`{"thinking":{"type":"disabled"},"reasoning_effort":"high"}`, "none"},
		{`{"thinking":{"type":"enabled","budget_tokens":8192},"reasoning_effort":"high"}`, "medium"},
	}
	for _, tc := range cases {
		var body map[string]any
		if err := json.Unmarshal([]byte(tc.body), &body); err != nil {
			t.Fatal(err)
		}
		body["model"] = "combo"
		body["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
		raw, _ := json.Marshal(body)
		wire, err := FinalizeBody(raw, "gpt-5", "oai-chat", domain.ProtocolOpenAI, domain.ReasoningOpenAI)
		if err != nil {
			t.Fatal(err)
		}
		var output map[string]any
		if err := json.Unmarshal(wire, &output); err != nil {
			t.Fatal(err)
		}
		if output["reasoning_effort"] != tc.effort {
			t.Errorf("precedence changed for %s: effort=%v want=%s", tc.body, output["reasoning_effort"], tc.effort)
		}
	}
}

func TestIndependentThinkingMetadataRoundTrip(t *testing.T) {
	cfg := Capture([]byte(`{"reasoning_effort":"xhigh","thinking":{"type":"disabled","budget_tokens":4096}}`))
	if cfg.Mode != ModeNone || cfg.Effort != "xhigh" || cfg.Enable != EnableDisabled || !cfg.BudgetSet || cfg.Budget != 0 {
		t.Fatalf("independent intent=%+v", cfg)
	}
	got := FromIR(ToIR(cfg))
	if *got != *cfg {
		t.Fatalf("IR dropped metadata: got=%+v want=%+v", got, cfg)
	}
	_, suffix := ParseSuffix("m(high)")
	merged := Merge(cfg, suffix)
	if merged.Level != "high" || merged.Enable != EnableDisabled || !merged.BudgetSet {
		t.Fatalf("suffix dropped metadata: %+v", merged)
	}
	if cfg.Mode != ModeNone || suffix.Enable != EnableUnset {
		t.Fatal("merge mutated input")
	}
}
