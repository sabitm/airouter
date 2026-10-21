package claudecode

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPrepareStripsOnlyRecognizedContextSelectors(t *testing.T) {
	cases := []struct {
		model string
		want  string
	}{
		{"claude-sonnet-4-6[1m]", "claude-sonnet-4-6"},
		{"claude-sonnet-4-6[2M]", "claude-sonnet-4-6"},
		{"claude-sonnet-4-6[1M][2m]", "claude-sonnet-4-6"},
		{"CLAUDE-[1M]SONNET-4-6[2m]", "CLAUDE-SONNET-4-6"},
		{"claude-sonnet-4-6[9m]", "claude-sonnet-4-6[9m]"},
		{"claude-sonnet-4-6", "claude-sonnet-4-6"},
	}
	for _, tc := range cases {
		body := []byte(`{"model":"` + tc.model + `","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
		out, err := Prepare(body, PrepareInput{
			SessionID: "11111111-1111-4111-8111-111111111111",
			DeviceID:  strings.Repeat("ab", 32),
			Profile:   DefaultProfile(),
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.model, err)
		}
		var doc struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(out, &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Model != tc.want {
			t.Fatalf("%s wire model = %q want %q", tc.model, doc.Model, tc.want)
		}
	}
}

func TestContextSelectorCapabilityIdentity(t *testing.T) {
	base, ok := LookupModel("claude-sonnet-4-6")
	if !ok {
		t.Fatal("base model missing")
	}
	for _, model := range []string{"claude-sonnet-4-6[1m]", "CLAUDE-SONNET-4-6[2M]", "CLAUDE-[1M]SONNET-4-6[2m]", "anthropic/claude-sonnet-4.6[1m]"} {
		got, ok := LookupModel(model)
		if !ok || got != base {
			t.Fatalf("%s row = %+v/%v want %+v", model, got, ok, base)
		}
	}
	if !modelSupportsOneMillion("claude-sonnet-4-6[1m]") {
		t.Fatal("supported tagged model lost long-context support")
	}
	if modelSupportsOneMillion("claude-haiku-4-5[1m]") {
		t.Fatal("suffix-only haiku gained long-context beta support")
	}
}

func TestBetaHeadersPreserveContextSelectorIntent(t *testing.T) {
	tagged := BetaHeaders(BetaRequest{Model: "claude-sonnet-4-6", OneMillion: true, ContextOneMillion: true})
	if !strings.Contains(tagged, BetaLongContext) {
		t.Fatalf("prepared tagged request lost long-context beta: %s", tagged)
	}
	untagged := BetaHeaders(BetaRequest{Model: "claude-sonnet-4-6", OneMillion: true})
	if !strings.Contains(untagged, BetaLongContext) {
		t.Fatalf("supported untagged request lost long-context beta: %s", untagged)
	}
	haiku := BetaHeaders(BetaRequest{Model: "claude-haiku-4-5", OneMillion: true, ContextOneMillion: true})
	if !strings.Contains(haiku, BetaLongContext) {
		t.Fatalf("pinned [1m] suffix beta was gated off: %s", haiku)
	}
	callerHaiku := BetaHeaders(BetaRequest{Model: "claude-haiku-4-5", CallerBetas: []string{BetaLongContext}})
	if strings.Contains(callerHaiku, BetaLongContext) {
		t.Fatalf("caller long-context beta bypassed the catalog gate: %s", callerHaiku)
	}
	twoMillion := BetaHeaders(BetaRequest{Model: "claude-sonnet-4-6[2m]"})
	if strings.Contains(twoMillion, BetaLongContext) {
		t.Fatalf("[2m] gained a long-context beta: %s", twoMillion)
	}
	directTag := BetaHeaders(BetaRequest{Model: "claude-sonnet-4-6[1m]", OneMillion: true})
	if !strings.Contains(directTag, BetaLongContext) {
		t.Fatalf("direct tagged request lost long-context beta: %s", directTag)
	}
}
