package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/claudecode"
)

func TestFinalReviewTaskBudgetBeta(t *testing.T) {
	for _, model := range []string{"claude-sonnet-4-5", "claude-sonnet-5"} {
		body := []byte(`{"model":"` + model + `","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"output_config":{"task_budget":{"type":"tokens","total":1000}}}`)
		h := claudeBetaHeader(context.Background(), &domain.Provider{}, body)
		if !strings.Contains(h, claudecode.BetaTaskBudget) {
			t.Fatalf("%s task-budget beta missing: %s", model, h)
		}
		caller := claudecode.BetaHeaders(claudecode.BetaRequest{
			Model:       model,
			CallerBetas: []string{claudecode.BetaTaskBudget},
		})
		if !strings.Contains(caller, claudecode.BetaTaskBudget) {
			t.Fatalf("%s validated caller task-budget beta dropped: %s", model, caller)
		}
	}
}
func TestFinalReviewDirectCallerBeta(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	p := &domain.Provider{Protocol: domain.ProtocolClaudeCode, APIKey: "static"}
	applyUpstreamHeaders(req, p, http.Header{"Anthropic-Beta": []string{claudecode.BetaLongContext}}, context.Background(), []byte(`{"model":"claude-sonnet-5"}`))
	if !strings.Contains(req.Header.Get("anthropic-beta"), claudecode.BetaLongContext) {
		t.Fatalf("caller beta lost %s", req.Header.Get("anthropic-beta"))
	}
}
func TestFinalReviewAPIKeyDeviceStableAndIsolated(t *testing.T) {
	p := &domain.Provider{ID: 1, Name: "old", Protocol: domain.ProtocolClaudeCode, BaseURL: "https://one"}
	renamed := *p
	renamed.Name = "new"
	if syntheticAPIKeyDevice(p) != syntheticAPIKeyDevice(&renamed) {
		t.Error("API-key device changes after rename")
	}
	p.OAuthCreds = &domain.OAuthCreds{ClaudeAccountUUID: "stale-account", ClaudeDeviceID: strings.Repeat("ab", 32)}
	acct, dev := claudeIdentity(p)
	if acct != "" || dev != "" {
		t.Errorf("API-key row borrowed stale OAuth identity %q %q", acct, dev)
	}
}
func TestFinalReviewCitationBlockOffsets(t *testing.T) {
	raw := `event: message_start
data: {"message":{"id":"m","model":"x","usage":{"input_tokens":1}}}

event: content_block_start
data: {"index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"index":0,"delta":{"type":"text_delta","text":"first"}}

event: content_block_stop
data: {"index":0}

event: content_block_start
data: {"index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"index":1,"delta":{"type":"text_delta","text":"second"}}

event: content_block_delta
data: {"index":1,"delta":{"type":"citations_delta","citation":{"type":"char_location","start_char_index":0,"end_char_index":1}}}

event: content_block_stop
data: {"index":1}

event: message_delta
data: {"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {}

`
	resp, err := collectStreamResponse(strings.NewReader(raw), anthropicCodec, nil, "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(resp.Content)
	if len(resp.Content) != 2 {
		t.Fatalf("text blocks joined, citation offsets now wrong: %s", b)
	}
	if len(resp.Content[0].Citations) != 0 || len(resp.Content[1].Citations) != 1 || resp.Content[1].Text != "second" {
		t.Fatalf("citation misattributed: %s", b)
	}
}
