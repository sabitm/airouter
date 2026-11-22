package claudecode

import (
	"reflect"
	"strings"
	"testing"
)

func betaPolicyFlags(t *testing.T, header string) map[string]bool {
	t.Helper()
	flags := map[string]bool{}
	if header == "" {
		return flags
	}
	for _, flag := range strings.Split(header, ",") {
		if flag == "" || flags[flag] {
			t.Fatalf("empty or duplicate beta in %q", header)
		}
		flags[flag] = true
	}
	return flags
}

func TestBetaPolicyBaselineModelMatrix(t *testing.T) {
	for _, tc := range []struct {
		model          string
		code, baseline bool
	}{
		{"claude-3-5-haiku", false, false},
		{"claude-3-5-sonnet", true, false},
		{"claude-3-7-sonnet", true, false},
		{"claude-haiku-4-5", false, true},
		{"claude-haiku-5-5", false, true},
		{"claude-sonnet-4-0", true, true},
		{"claude-sonnet-4-5", true, true},
		{"claude-sonnet-4-6", true, true},
		{"claude-sonnet-5", true, true},
		{"claude-sonnet-5-5", true, true},
		{"claude-opus-4-0", true, true},
		{"claude-opus-4-1", true, true},
		{"claude-opus-4-5", true, true},
		{"claude-opus-4-6", true, true},
		{"claude-opus-4-7", true, true},
		{"claude-opus-4-8", true, true},
		{"claude-opus-5", true, true},
		{"claude-opus-5-5", true, true},
		{"claude-fable-5", true, true},
		{"claude-fable-5-1", true, true},
		{"claude-mythos-5", true, true},
		{"claude-mythos-5-1", true, true},
		{"claude-sonnet-4-20250514", true, true},
		{"claude-opus-4-1-20250805", true, true},
		{"claude-haiku-4-5-20251001", false, true},
		{"anthropic/claude-sonnet-4.6[1m]", true, true},
		{"claude-sonnet-4-6-preview", true, false},
		{"claude-sonnet-4-6-20990101", true, false},
		{"not-a-model", true, false},
		{"", false, false},
	} {
		t.Run(tc.model, func(t *testing.T) {
			want := map[string]bool{}
			if tc.code {
				want[BetaClaudeCode] = true
			}
			if tc.baseline {
				want[BetaInterleaved], want[BetaContextMgmt] = true, true
			}
			got := betaPolicyFlags(t, BetaHeaders(BetaRequest{Model: tc.model}))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("baseline flags = %v, want %v", got, want)
			}
		})
	}
}

func TestBetaPolicyThinkingAndOptIns(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		active, summarized, redact bool
	}{
		{"absent", false, false, false},
		{"disabled", false, true, false},
		{"active", true, false, true},
		{"summarized", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flags := betaPolicyFlags(t, BetaHeaders(BetaRequest{
				Model: "claude-sonnet-4-6", ThinkingActive: tc.active, ThinkingSummarized: tc.summarized,
				CallerBetas: []string{BetaInterleaved, BetaContextMgmt, BetaRedactThinking, BetaInterleaved, "not-a-beta"},
			}))
			if !flags[BetaInterleaved] || !flags[BetaContextMgmt] || flags[BetaRedactThinking] != tc.redact || flags["not-a-beta"] {
				t.Fatalf("thinking flags = %v", flags)
			}
		})
	}
	for _, active := range []bool{false, true} {
		flags := betaPolicyFlags(t, BetaHeaders(BetaRequest{
			Model: "claude-3-7-sonnet", ThinkingActive: active, ContextManagement: true,
			CallerBetas: []string{BetaInterleaved, BetaRedactThinking, BetaContextMgmt},
		}))
		if flags[BetaInterleaved] || flags[BetaRedactThinking] || flags[BetaContextMgmt] {
			t.Fatalf("Claude 3 gained baseline flags: %v", flags)
		}
	}
	unknown := betaPolicyFlags(t, BetaHeaders(BetaRequest{Model: "claude-sonnet-4-6-preview", ThinkingActive: true, ContextManagement: true, Effort: true}))
	if !unknown[BetaInterleaved] || !unknown[BetaRedactThinking] || unknown[BetaContextMgmt] || unknown[BetaEffort] {
		t.Fatalf("existing unknown-model opt-ins changed: %v", unknown)
	}
}

func TestBetaPolicyCacheScopeBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		official, explicit, caller, want bool
	}{
		{"custom unrequested", false, false, false, false},
		{"official automatic", true, false, false, true},
		{"custom body", false, true, false, true},
		{"custom caller", false, false, true, true},
		{"official deduplication", true, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := BetaRequest{Model: "claude-sonnet-4-6", OfficialHost: tc.official, CacheScope: tc.explicit}
			if tc.caller {
				req.CallerBetas = []string{BetaPromptCache, BetaPromptCache}
			}
			flags := betaPolicyFlags(t, BetaHeaders(req))
			if flags[BetaPromptCache] != tc.want || flags[BetaExtendedTTL] || flags[BetaCacheEvict] {
				t.Fatalf("cache flags = %v", flags)
			}
		})
	}
	for _, oauth := range []bool{false, true} {
		flags := betaPolicyFlags(t, BetaHeaders(BetaRequest{OAuth: oauth}))
		if len(flags) != boolBetaPolicy(oauth) || flags[BetaOAuth] != oauth {
			t.Fatalf("discovery flags = %v", flags)
		}
	}
}

func boolBetaPolicy(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestBetaPolicyNativeContextMatrix(t *testing.T) {
	for _, tc := range []struct {
		model                      string
		native, selector, explicit bool
	}{
		{"claude-haiku-5-5", true, false, true},
		{"claude-sonnet-5", true, false, true},
		{"claude-sonnet-5-5", true, false, true},
		{"claude-opus-4-7", true, false, true},
		{"claude-opus-4-8", true, false, true},
		{"claude-opus-5", true, false, true},
		{"claude-opus-5-5", true, false, true},
		{"claude-fable-5", true, false, true},
		{"claude-fable-5-1", true, false, true},
		{"claude-mythos-5", true, false, true},
		{"claude-mythos-5-1", true, false, true},
		{"claude-sonnet-4-6", false, true, true},
		{"claude-opus-4-6", false, true, true},
		{"claude-sonnet-4-0", false, true, true},
		{"claude-sonnet-4-5-20250929", false, true, true},
		{"claude-haiku-4-5-20251001", false, true, false},
		{"claude-opus-4-1-20250805", false, true, false},
		{"claude-sonnet-4-6-preview", false, true, false},
	} {
		t.Run(tc.model, func(t *testing.T) {
			if got := modelHasNativeOneMillion(tc.model); got != tc.native {
				t.Fatalf("native context = %v, want %v", got, tc.native)
			}
			requests := []struct {
				name string
				req  BetaRequest
				want bool
			}{
				{"none", BetaRequest{Model: tc.model}, false},
				{"prepared selector", BetaRequest{Model: tc.model, OneMillion: true, ContextOneMillion: true}, tc.selector},
				{"direct selector", BetaRequest{Model: tc.model + "[1m]", OneMillion: true}, tc.selector},
				{"legacy explicit", BetaRequest{Model: tc.model, OneMillion: true}, tc.explicit},
				{"body explicit and selector", BetaRequest{Model: tc.model, OneMillion: true, ContextOneMillion: true, ExplicitOneMillion: true}, tc.selector || tc.explicit},
				{"caller", BetaRequest{Model: tc.model, CallerBetas: []string{BetaLongContext}}, tc.explicit},
				{"caller and selector", BetaRequest{Model: tc.model, OneMillion: true, ContextOneMillion: true, CallerBetas: []string{BetaLongContext}}, tc.selector || tc.explicit},
				{"two million", BetaRequest{Model: tc.model + "[2m]"}, false},
			}
			for _, request := range requests {
				t.Run(request.name, func(t *testing.T) {
					flags := betaPolicyFlags(t, BetaHeaders(request.req))
					if flags[BetaLongContext] != request.want {
						t.Fatalf("long-context = %v, want %v; flags %v", flags[BetaLongContext], request.want, flags)
					}
				})
			}
		})
	}
}

func TestBetaPolicyStructuredOutputBoundaries(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  bool
	}{
		{"claude-3-7-sonnet", false},
		{"claude-sonnet-4-0", false},
		{"claude-sonnet-4-20250514", false},
		{"claude-opus-4-0", false},
		{"claude-opus-4-20250514", false},
		{"claude-opus-4-1", true},
		{"claude-opus-4-1-20250805", true},
		{"claude-sonnet-4-5", true},
		{"claude-sonnet-4-6", true},
		{"claude-sonnet-5", true},
		{"claude-sonnet-4-6-preview", true},
		{"not-a-model", false},
	} {
		for _, source := range []string{"none", "body", "caller", "both"} {
			t.Run(tc.model+"/"+source, func(t *testing.T) {
				req := BetaRequest{Model: tc.model, StructuredOutput: source == "body" || source == "both"}
				if source == "caller" || source == "both" {
					req.CallerBetas = []string{BetaStructured, BetaStructured}
				}
				flags := betaPolicyFlags(t, BetaHeaders(req))
				if flags[BetaStructured] != (tc.want && source != "none") {
					t.Fatalf("structured flags = %v", flags)
				}
			})
		}
	}
}
