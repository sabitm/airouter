package thinking

import (
	"reflect"
	"testing"

	"airouter/internal/domain"
)

func TestStripContextSelectors(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"claude-sonnet-4-6[1m]", "claude-sonnet-4-6"},
		{"claude-sonnet-4-6[2M]", "claude-sonnet-4-6"},
		{"CLAUDE-SONNET-4-6[1M][2m]", "CLAUDE-SONNET-4-6"},
		{"CLAUDE-[1M]SONNET-4-6[2m]", "CLAUDE-SONNET-4-6"},
		{"anthropic/claude-sonnet-4.6[1m]", "anthropic/claude-sonnet-4.6"},
		{"claude-sonnet-4-6[9m]", "claude-sonnet-4-6[9m]"},
		{"claude-sonnet-4-6[1m](high)", "claude-sonnet-4-6(high)"},
		{"claude-sonnet-4-6", "claude-sonnet-4-6"},
	}
	for _, tc := range cases {
		if got := StripContextSelectors(tc.in); got != tc.want {
			t.Errorf("StripContextSelectors(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
	if !HasOneMillionContextSelector("claude-sonnet-4-6[1M]") {
		t.Fatal("case-insensitive [1m] was not recognized")
	}
	if !HasOneMillionContextSelector("claude-[1m]sonnet-4-6") {
		t.Fatal("nontrailing [1m] was not recognized")
	}
	if HasOneMillionContextSelector("claude-sonnet-4-6[2m]") || HasOneMillionContextSelector("claude-sonnet-4-6[9m]") {
		t.Fatal("non-[1m] selector was treated as one million")
	}
}

func TestClaudeContextSelectorsDoNotChangeCaps(t *testing.T) {
	base := CapsFor("claude-sonnet-4-6", domain.ProtocolClaudeCode, domain.ReasoningClaude)
	for _, model := range []string{
		"claude-sonnet-4-6[1m]",
		"claude-sonnet-4-6[2M]",
		"claude-sonnet-4-6[1m][2m]",
		"CLAUDE-[1M]SONNET-4-6[2m]",
		"anthropic/claude-sonnet-4.6[1m]",
	} {
		got := CapsFor(model, domain.ProtocolClaudeCode, domain.ReasoningClaude)
		if !reflect.DeepEqual(got, base) {
			t.Fatalf("%s caps = %+v want %+v", model, got, base)
		}
	}

	unknown := CapsFor("claude-sonnet-4-6[9m]", domain.ProtocolClaudeCode, domain.ReasoningClaude)
	if unknown.Format != FormatClaudeBudget || unknown.Format == base.Format {
		t.Fatalf("unknown bracket text changed capability lookup: %+v", unknown)
	}
}
