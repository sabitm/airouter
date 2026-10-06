package kiro

import (
	"encoding/json"
	"testing"
)

func ideCapability() *Capability {
	return &Capability{
		HasEffort:       true,
		EffortPath:      "output_config",
		EffortLevels:    []string{"low", "medium", "high", "xhigh", "max"},
		DefaultEffort:   "medium",
		HasThinking:     true,
		ThinkingToggle:  true,
		ThinkingEnabled: true,
	}
}

func TestResolveSelectionIDEChain(t *testing.T) {
	cases := []struct {
		name      string
		effort    string
		effortSet bool
		thinking  string
		want      string
	}{
		{name: "no intent", want: `{"output_config":{"effort":"medium"},"thinking":{"type":"adaptive"}}`},
		{name: "xhigh", effort: "xhigh", effortSet: true, want: `{"output_config":{"effort":"xhigh"},"thinking":{"type":"adaptive"}}`},
		{name: "unsupported", effort: "ultra", effortSet: true, want: `{"output_config":{"effort":"medium"},"thinking":{"type":"adaptive"}}`},
		{name: "xhigh disabled", effort: "xhigh", effortSet: true, thinking: ThinkingDisabled, want: `{"output_config":{"effort":"medium"},"thinking":{"type":"disabled"}}`},
		{name: "high disabled", effort: "high", effortSet: true, thinking: ThinkingDisabled, want: `{"output_config":{"effort":"high"},"thinking":{"type":"disabled"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sel := ResolveSelection(ideCapability(), tc.effort, tc.effortSet, tc.thinking, false)
			got := mustSelectionJSON(t, sel)
			if got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestResolveSelectionPathAndMissing(t *testing.T) {
	gpt := ideCapability()
	gpt.EffortPath = "reasoning"
	sel := ResolveSelection(gpt, "high", true, "", false)
	if got := mustSelectionJSON(t, sel); got != `{"reasoning":{"effort":"high"},"thinking":{"type":"adaptive"}}` {
		t.Fatalf("gpt path = %s", got)
	}
	if ResolveSelection(nil, "high", true, "", false) != nil {
		t.Fatal("missing capability wrote controls")
	}
	restricted := &Capability{HasEffort: true, EffortPath: "output_config", EffortLevels: []string{"xhigh", "max"}, DefaultEffort: "xhigh", HasThinking: true, ThinkingToggle: true}
	sel = ResolveSelection(restricted, "xhigh", true, ThinkingDisabled, false)
	if sel == nil || len(sel.Fields) != 1 || !sel.EffortOmitted {
		t.Fatalf("restricted = %#v", sel)
	}
	if _, ok := sel.Fields["thinking"]; !ok {
		t.Fatal("thinking omitted")
	}
	if _, ok := sel.Fields["output_config"]; ok {
		t.Fatal("empty effort enum still encoded")
	}
	noToggle := ideCapability()
	noToggle.ThinkingToggle = false
	sel = ResolveSelection(noToggle, "", false, ThinkingDisabled, false)
	if got := mustSelectionJSON(t, sel); got != `{"output_config":{"effort":"medium"}}` {
		t.Fatalf("non-toggle = %s", got)
	}
	sel = ResolveSelection(noToggle, "xhigh", true, ThinkingDisabled, false)
	if got := mustSelectionJSON(t, sel); got != `{"output_config":{"effort":"xhigh"}}` {
		t.Fatalf("unsupported toggle filtered effort: %s", got)
	}
}

func TestResolveSelectionDisabledCatalogDefault(t *testing.T) {
	cap := ideCapability()
	cap.DefaultEffort = "xhigh"
	cap.ThinkingEnabled = false
	sel := ResolveSelection(cap, "", false, "", false)
	if got := mustSelectionJSON(t, sel); got != `{"output_config":{"effort":"high"},"thinking":{"type":"disabled"}}` {
		t.Fatalf("disabled default = %s", got)
	}
	noToggle := *cap
	noToggle.ThinkingToggle = false
	sel = ResolveSelection(&noToggle, "", false, "", false)
	if got := mustSelectionJSON(t, sel); got != `{"output_config":{"effort":"xhigh"}}` {
		t.Fatalf("non-toggle disabled default sent thinking: %s", got)
	}
}

func TestApplyAdditionalFieldsDoesNotInvent(t *testing.T) {
	body := []byte(`{"conversationState":{},"inferenceConfig":{"maxTokens":1}}`)
	if string(ApplyAdditionalFields(body, nil)) != string(body) {
		t.Fatal("nil selection changed body")
	}
	sel := ResolveSelection(ideCapability(), "high", true, ThinkingDisabled, true)
	out := ApplyAdditionalFields(body, sel)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["inferenceConfig"]; !ok {
		t.Fatal("inferenceConfig lost")
	}
	fields := got["additionalModelRequestFields"].(map[string]any)
	if fields["output_config"].(map[string]any)["effort"] != "high" {
		t.Fatalf("fields = %#v", fields)
	}
	if !sel.BudgetIgnored {
		t.Fatal("budget diagnostic missing")
	}
}

func mustSelectionJSON(t *testing.T, sel *Selection) string {
	t.Helper()
	if sel == nil {
		t.Fatal("nil selection")
	}
	raw, err := json.Marshal(sel.Fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
