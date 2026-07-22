package cursor

import (
	"testing"

	"airouter/internal/domain"
)

func TestParseUsableModels(t *testing.T) {
	// Two ModelDetails in field 1: id(1) + display_name(4).
	detail1 := concatBytes(
		encodeField(1, wireLen, "gpt-5.2"),
		encodeField(4, wireLen, "GPT 5.2"),
	)
	detail2 := concatBytes(
		encodeField(1, wireLen, "claude-4.5-sonnet"),
		encodeField(4, wireLen, "Claude 4.5 Sonnet"),
	)
	resp := concatBytes(
		encodeField(1, wireLen, detail1),
		encodeField(1, wireLen, detail2),
	)
	ids := ParseUsableModels(resp)
	if len(ids) != 2 || ids[0] != "gpt-5.2" || ids[1] != "claude-4.5-sonnet" {
		t.Errorf("ids = %v", ids)
	}
}

func TestParseUsableModelsDedup(t *testing.T) {
	detail := encodeField(1, wireLen, "dup-model")
	resp := concatBytes(
		encodeField(1, wireLen, detail),
		encodeField(1, wireLen, detail),
	)
	ids := ParseUsableModels(resp)
	if len(ids) != 1 {
		t.Errorf("ids = %v, want 1 (deduped)", ids)
	}
}

func TestParseUsableModelsFallsBackToDisplayModelID(t *testing.T) {
	// No id(1), only display_model_id(3).
	detail := encodeField(3, wireLen, "fallback-id")
	resp := encodeField(1, wireLen, detail)
	ids := ParseUsableModels(resp)
	if len(ids) != 1 || ids[0] != "fallback-id" {
		t.Errorf("ids = %v, want [fallback-id]", ids)
	}
}

func TestListModelIDsWithoutTokenReturnsStatic(t *testing.T) {
	ids := ListModelIDs(nil, &domain.Provider{})
	if len(ids) != len(StaticModels) {
		t.Errorf("ids = %d, want static %d", len(ids), len(StaticModels))
	}
}

func TestProviderCredsStripsPrefix(t *testing.T) {
	p := &domain.Provider{
		APIKey:     "sess::tok",
		OAuthCreds: &domain.OAuthCreds{MachineID: "m1"},
	}
	tok, mid := providerCreds(p)
	if tok != "tok" {
		t.Errorf("token = %q, want tok", tok)
	}
	if mid != "m1" {
		t.Errorf("machine = %q, want m1", mid)
	}
}

func TestProviderCredsFallbackMachineID(t *testing.T) {
	p := &domain.Provider{APIKey: "tok"}
	tok, mid := providerCreds(p)
	if tok != "tok" {
		t.Errorf("token = %q", tok)
	}
	if mid != machineIDFallback("tok") {
		t.Errorf("machine = %q, want fallback", mid)
	}
}
