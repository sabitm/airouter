package claudecode

import (
	"strings"
	"testing"
)

func fullBetaRequest(model string) BetaRequest {
	return BetaRequest{
		Model:              model,
		OAuth:              true,
		ContextManagement:  true,
		StructuredOutput:   true,
		Effort:             true,
		CacheScope:         true,
		ExtendedTTL:        true,
		CacheEvict:         true,
		AdvancedTools:      true,
		TaskBudget:         true,
		ThinkingActive:     true,
		ThinkingSummarized: false,
		FastMode:           true,
		OneMillion:         true,
		CallerBetas: []string{
			BetaClaudeCode, BetaOAuth, BetaInterleaved, BetaLongContext,
			BetaContextMgmt, BetaPromptCache, BetaStructured, BetaEffort,
			BetaRedactThinking, BetaExtendedTTL, BetaCacheEvict,
			BetaAdvancedToolUse, BetaFastMode, BetaTaskBudget,
		},
	}
}

func TestKnownDatedBetaHeadersEquivalent(t *testing.T) {
	pairs := [][2]string{
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5"},
		{"claude-sonnet-4-20250514", "claude-sonnet-4-0"},
		{"claude-sonnet-4-5-20250929", "claude-sonnet-4-5"},
		{"claude-opus-4-20250514", "claude-opus-4-0"},
		{"claude-opus-4-1-20250805", "claude-opus-4-1"},
		{"claude-opus-4-5-20251101", "claude-opus-4-5"},
	}
	for _, pair := range pairs {
		dated := BetaHeaders(fullBetaRequest(pair[0]))
		canonical := BetaHeaders(fullBetaRequest(pair[1]))
		if dated != canonical {
			t.Errorf("%s beta = %q want %q", pair[0], dated, canonical)
		}
	}
}

func TestLookupModelUsesOnlySourceDatedIDs(t *testing.T) {
	pairs := [][2]string{
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5"},
		{"claude-sonnet-4-20250514", "claude-sonnet-4-0"},
		{"claude-sonnet-4-5-20250929", "claude-sonnet-4-5"},
		{"claude-opus-4-20250514", "claude-opus-4-0"},
		{"claude-opus-4-1-20250805", "claude-opus-4-1"},
		{"claude-opus-4-5-20251101", "claude-opus-4-5"},
	}
	for _, pair := range pairs {
		dated, ok := LookupModel(pair[0])
		if !ok {
			t.Fatalf("%s has no catalog row", pair[0])
		}
		canonical, ok := LookupModel(pair[1])
		if !ok || dated != canonical {
			t.Fatalf("%s row = %+v/%v want %+v", pair[0], dated, ok, canonical)
		}
	}
	for _, model := range []string{
		"claude-sonnet-4-6-20990101",
		"claude-haiku-4-5-20990101",
		"claude-sonnet-4-6-preview",
	} {
		if row, ok := LookupModel(model); ok {
			t.Fatalf("%s invented catalog row %+v", model, row)
		}
	}
}

func TestHaiku45InterleavedAndContextGatesForBothSpellings(t *testing.T) {
	for _, model := range []string{"claude-haiku-4-5", "claude-haiku-4-5-20251001"} {
		active := BetaHeaders(BetaRequest{Model: model, ThinkingActive: true})
		if !strings.Contains(active, BetaInterleaved) || !strings.Contains(active, BetaRedactThinking) {
			t.Fatalf("%s active thinking beta = %q", model, active)
		}
		summarized := BetaHeaders(BetaRequest{Model: model, ThinkingActive: true, ThinkingSummarized: true})
		if !strings.Contains(summarized, BetaInterleaved) || strings.Contains(summarized, BetaRedactThinking) {
			t.Fatalf("%s summarized beta = %q", model, summarized)
		}
		context := BetaHeaders(BetaRequest{Model: model, ContextManagement: true})
		if !strings.Contains(context, BetaContextMgmt) {
			t.Fatalf("%s context management beta = %q", model, context)
		}
		effort := BetaHeaders(BetaRequest{Model: model, Effort: true})
		if strings.Contains(effort, BetaEffort) {
			t.Fatalf("%s gained effort beta = %q", model, effort)
		}
		tagged := BetaHeaders(BetaRequest{Model: model, OneMillion: true, ContextOneMillion: true})
		if !strings.Contains(tagged, BetaLongContext) {
			t.Fatalf("%s selector long-context beta = %q", model, tagged)
		}
		caller := BetaHeaders(BetaRequest{Model: model, CallerBetas: []string{BetaLongContext}})
		if strings.Contains(caller, BetaLongContext) {
			t.Fatalf("%s caller long-context bypassed raw catalog = %q", model, caller)
		}
	}
}

func TestMythos5EffectiveBetaGatesSeparateFromRawCatalog(t *testing.T) {
	raw, ok := LookupModel("claude-mythos-5")
	if !ok {
		t.Fatal("mythos 5 raw row missing")
	}
	if raw.Adaptive || raw.Effort || raw.MaxEffort || raw.XHigh || raw.ContextMgmt || raw.RejectsDisabled {
		t.Fatalf("mythos 5 raw row changed: %+v", raw)
	}
	beta := BetaHeaders(BetaRequest{
		Model: "claude-mythos-5", ContextManagement: true, Effort: true, ThinkingActive: true,
	})
	for _, flag := range []string{BetaContextMgmt, BetaEffort, BetaInterleaved, BetaRedactThinking} {
		if !strings.Contains(beta, flag) {
			t.Fatalf("mythos 5 missing %s in %q", flag, beta)
		}
	}
	if strings.Contains(beta, BetaFastMode) {
		t.Fatalf("mythos 5 gained fast mode: %q", beta)
	}
}

func TestUnknownClaudeIDsKeepConservativeBetaGates(t *testing.T) {
	for _, model := range []string{
		"claude-sonnet-4-6-preview",
		"claude-opus-4-7-preview",
		"claude-haiku-4-5-preview",
		"claude-sonnet-4-6-20990101",
	} {
		beta := BetaHeaders(BetaRequest{Model: model, ContextManagement: true, Effort: true, OneMillion: true})
		for _, flag := range []string{BetaContextMgmt, BetaEffort, BetaLongContext} {
			if strings.Contains(beta, flag) {
				t.Fatalf("%s gained %s: %q", model, flag, beta)
			}
		}
	}
}
